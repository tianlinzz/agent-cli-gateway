// Command gateway is the agent-gateway entrypoint. It is the API gateway: it
// loads the gateway runtime config, exposes the OpenAI-compatible HTTP API
// (/v1/models, /v1/chat/completions, /v1/sessions/{id}/abort, /health/*), and
// owns the worker Supervisor. Every execution goes through
// runtime.ExecutionBackend (worker.LocalExecutionBackend), which forks one
// nsjail-wrapped worker per session. The API process never imports or launches
// a concrete agent CLI itself.
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

var (
	version = "dev"
	// commit and buildTime are injected via Makefile LDFLAGS (-X main.commit=...).
	commit    = "none"
	buildTime = "unknown"
)

func agentExecutionConfigs(in map[string]config.AgentConfig) map[string]runtime.AgentExecutionConfig {
	out := make(map[string]runtime.AgentExecutionConfig, len(in))
	for name, c := range in {
		env := make(map[string]string, len(c.Env))
		for k, v := range c.Env {
			env[k] = v
		}
		out[name] = runtime.AgentExecutionConfig{Command: c.Command, DefaultModel: c.DefaultModel,
			Permission: c.Permission, TurnTimeout: c.Timeout, MaxConcurrency: c.MaxConcurrency, Env: env}
	}
	return out
}

func main() {
	// -config defaults to $GATEWAY_CONFIG (set by the container entrypoint to
	// the runtime config path) so a mounted/entrypoint-written config is loaded
	// without extra argv.
	configPath := flag.String("config", envOrDefault("GATEWAY_CONFIG", ""), "path to the gateway TOML config (default: $GATEWAY_CONFIG or built-in defaults)")
	showVersion := flag.Bool("version", false, "print version and exit")
	workerExec := flag.String("worker-exec", "", "worker child executable (default: $GW_WORKER_EXEC or gateway-worker)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gateway %s (commit %s, built %s)\n", version, commit, buildTime)
		return
	}

	// Load the gateway runtime config (defaults when --config is absent).
	cfg := config.DefaultGatewayConfig()
	if *configPath != "" {
		loaded, err := config.LoadGateway(*configPath)
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
		RuntimeDir:      envOrDefault("GATEWAY_RUNTIME_DIR", "gateway-run"),
		WorkerExec:      *workerExec,
		StartTimeout:    30 * time.Second,
		ShutdownTimeout: cfg.Server.ShutdownTimeout,
		Agents:          agentExecutionConfigs(cfg.Agents),
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

	callerTokens := make(map[string]string)
	for _, caller := range cfg.Auth.Callers {
		for _, token := range caller.Tokens {
			callerTokens[token] = caller.ID
		}
	}
	handler := openai.NewHandler(openai.Options{
		Registry:     reg,
		Store:        runtime.NewMemorySessionStore(),
		Backend:      backend,
		CallerTokens: callerTokens,
		Enabled: func(name string) bool {
			agent, ok := cfg.Agents[name]
			return !ok || agent.Enabled
		},
	})

	srv := &http.Server{
		Addr:    cfg.Server.ListenAddr,
		Handler: handler.Routes(),
	}

	slog.Info("gateway starting",
		"version", version,
		"commit", commit,
		"build_time", buildTime,
		"mode", cfg.Mode,
		"listen_addr", cfg.Server.ListenAddr,
		"workspace_root", rootAbs,
		"isolation_required", cfg.Isolation.Required,
		"nsjail_version", cfg.Isolation.NsjailVersion,
		"nsjail_source", cfg.Isolation.NsjailSource,
		"worker_exec", *workerExec,
		"auth_enabled", len(callerTokens) > 0,
		"adapters", reg.List(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("gateway listening")
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("gateway shutting down")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("gateway server failed", "error", err)
			os.Exit(1)
		}
	}

	// Graceful shutdown: drain in-flight HTTP requests (aborting client
	// disconnects at the worker), then tear the supervisor down so every
	// worker process group is reaped.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("gateway http shutdown", "error", err)
	}
	if err := backend.Supervisor().Close(shutdownCtx); err != nil {
		slog.Warn("gateway supervisor close", "error", err)
	}
	slog.Info("gateway stopped")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
