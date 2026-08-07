// Command gatewayd is the NEW agent-gateway entrypoint of the rewrite. It
// coexists with the legacy cmd/gateway binary (and its config.GatewayConfig in
// config/gateway.go), which remains the active entrypoint until the old
// implementation is removed (task 7).
//
// This process is the API gateway: it loads the gateway runtime config,
// exposes the OpenAI-compatible HTTP API (/v1/models, /v1/chat/completions,
// /v1/sessions/{id}/abort, /health/*), and owns the worker Supervisor. Every
// execution goes through runtime.ExecutionBackend (worker.LocalExecutionBackend),
// which forks one nsjail-wrapped worker per session. The API process never
// imports or launches a concrete agent CLI itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/api/openai"
	"github.com/tianlinzz/agent-cli-gateway/config"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to the gateway TOML config (empty = defaults)")
	showVersion := flag.Bool("version", false, "print version and exit")
	workerExec := flag.String("worker-exec", "", "worker child executable (default: $GW_WORKER_EXEC or gateway-worker)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gatewayd %s\n", version)
		return
	}

	// Load the new gateway runtime config. The legacy config.GatewayConfig
	// (config/gateway.go) and the old cmd/gateway entrypoint are left
	// untouched until the old implementation is removed (task 7).
	cfg := config.DefaultGatewayRuntimeConfig()
	if *configPath != "" {
		loaded, err := config.LoadGatewayRuntime(*configPath)
		if err != nil {
			slog.Error("load gateway config", "path", *configPath, "error", err)
			os.Exit(1)
		}
		cfg = *loaded
	}

	// The registry is process-wide: adapter packages register into it from
	// their init() functions via runtime.Register (see plugin_agent_*.go in
	// this directory).
	reg := runtime.DefaultRegistry()

	// The workspace root must exist before the supervisor constructs its
	// resolver (fail-closed on a missing root).
	if err := os.MkdirAll(cfg.Workspace.Root, 0o700); err != nil {
		slog.Error("create workspace root", "root", cfg.Workspace.Root, "error", err)
		os.Exit(1)
	}
	rootAbs, err := filepath.Abs(cfg.Workspace.Root)
	if err != nil {
		slog.Error("absolutize workspace root", "error", err)
		os.Exit(1)
	}

	if *workerExec == "" {
		*workerExec = envOrDefault("GW_WORKER_EXEC", "gateway-worker")
	}

	// The supervisor forks one nsjail-wrapped worker child per session. The
	// API process never starts an agent CLI directly; all execution flows
	// through this backend.
	backend, err := worker.NewLocalExecutionBackend(worker.Config{
		Mode:            cfg.Mode,
		Isolation:       cfg.Isolation,
		WorkspaceRoot:   rootAbs,
		RuntimeDir:      "gateway-run",
		WorkerExec:      *workerExec,
		StartTimeout:    30 * time.Second,
		ShutdownTimeout: cfg.Server.ShutdownTimeout,
	})
	if err != nil {
		slog.Error("create execution backend", "error", err)
		os.Exit(1)
	}

	// Surface the nsjail preflight result as readiness (503 when the sandbox
	// boundary is unavailable) without taking the API process down.
	if err := backend.Preflight(context.Background()); err != nil {
		slog.Warn("nsjail preflight failed; readiness will report 503", "error", err)
	}

	handler := openai.NewHandler(openai.Options{
		Registry:    reg,
		Store:       runtime.NewMemorySessionStore(),
		Backend:     backend,
		AuthToken:   cfg.Auth.Token,
		OwnerHeader: "X-User-Id",
		Enabled: func(name string) bool {
			agent, ok := cfg.Agents[name]
			return !ok || agent.Enabled
		},
	})

	srv := &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: handler.Routes(),
	}

	slog.Info("gatewayd starting",
		"version", version,
		"mode", cfg.Mode,
		"listen_addr", cfg.Server.ListenAddr,
		"workspace_root", rootAbs,
		"isolation_required", cfg.Isolation.Required,
		"nsjail_version", cfg.Isolation.NsjailVersion,
		"nsjail_source", cfg.Isolation.NsjailSource,
		"worker_exec", *workerExec,
		"auth_enabled", cfg.Auth.Token != "",
		"adapters", reg.List(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("gatewayd listening")
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("gatewayd shutting down")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("gatewayd server failed", "error", err)
			os.Exit(1)
		}
	}

	// Graceful shutdown: drain in-flight HTTP requests (aborting client
	// disconnects at the worker), then tear the supervisor down so every
	// worker process group is reaped.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("gatewayd http shutdown", "error", err)
	}
	if err := backend.Supervisor().Close(shutdownCtx); err != nil {
		slog.Warn("gatewayd supervisor close", "error", err)
	}
	slog.Info("gatewayd stopped")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
