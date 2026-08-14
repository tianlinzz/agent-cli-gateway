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
	"github.com/tianlinzz/agent-cli-gateway/metrics"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
	"github.com/tianlinzz/agent-cli-gateway/worker/nsjail"
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
			Permission: c.Permission, TurnTimeout: c.Timeout, MaxConcurrency: c.MaxConcurrency, Env: env,
			InjectSystemPrompt: c.InjectSystemPrompt}
	}
	return out
}

func agentModels(in map[string]config.AgentConfig) map[string][]string {
	out := make(map[string][]string, len(in))
	for name, c := range in {
		out[name] = append([]string(nil), c.Models...)
	}
	return out
}

// agentCommands extracts each agent's configured CLI command so model
// discovery can probe availability (exec.LookPath) before advertising it.
// A non-nil map puts the catalog in probe mode: enabled agents with an empty
// or unresolvable command are hidden from /v1/models.
func agentCommands(in map[string]config.AgentConfig) map[string]string {
	out := make(map[string]string, len(in))
	for name, c := range in {
		out[name] = c.Command
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
	printProfile := flag.String("print-nsjail-profile", "", "print the generated nsjail profile for a session id and exit (diagnostic used by the Linux-CI smoke)")
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

	// Diagnostic: emit the exact nsjail profile the supervisor will run so the
	// Linux-CI smoke tests the REAL profile (seccomp policy, PID namespace,
	// mounts) rather than a hand-written approximation. Uses runtime.GOARCH so
	// the seccomp policy matches the architecture the binary runs on.
	if *printProfile != "" {
		ws := filepath.Join(os.TempDir(), "gw-profile-ws")
		home := filepath.Join(os.TempDir(), "gw-profile-home")
		sock := filepath.Join(os.TempDir(), "gw-profile-sock")
		for _, d := range []string{ws, home, sock} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				slog.Error("create profile dump dir", "dir", d, "error", err)
				os.Exit(1)
			}
		}
		prof, err := nsjail.Build(cfg.Isolation, nsjail.SessionLayout{
			WorkspaceDir: ws,
			AgentHomeDir: home,
			SocketDir:    sock,
		}, *printProfile)
		if err != nil {
			slog.Error("build nsjail profile", "error", err)
			os.Exit(1)
		}
		fmt.Print(prof.Config)
		return
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
		Mode:                cfg.Mode,
		Isolation:           cfg.Isolation,
		WorkspaceRoot:       rootAbs,
		RuntimeDir:          envOrDefault("GATEWAY_RUNTIME_DIR", "gateway-run"),
		WorkerExec:          *workerExec,
		StartTimeout:        30 * time.Second,
		StopGracePeriod:     cfg.Worker.StopGracePeriod,
		HeartbeatInterval:   cfg.Worker.HeartbeatInterval,
		HeartbeatTimeout:    cfg.Worker.HeartbeatTimeout,
		HeartbeatFailures:   cfg.Worker.HeartbeatFailures,
		SessionIdleTimeout:  cfg.Sessions.IdleTimeout,
		SessionReapInterval: cfg.Sessions.ReapInterval,
		MaxWorkers:          cfg.Limits.MaxWorkers,
		MaxWorkerLogBytes:   cfg.Limits.MaxWorkerLogBytes,
		Agents:              agentExecutionConfigs(cfg.Agents),
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

	// Configure structured JSON logging so every run/session/correlation field
	// is machine-parseable. The handler receives its own logger reference.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	metricsReg := metrics.NewRegistry()

	// Admission control (O-F10): the store is shared so session-count checks
	// see the same records the handler manages.
	sessionStore := runtime.NewMemorySessionStore()
	admit := openai.NewAdmissionController(struct {
		MaxActiveRuns             int
		MaxActiveRunsPerCaller    int
		MaxActiveRunsPerWorkspace int
		MaxSessionsPerCaller      int
	}{
		MaxActiveRuns:             cfg.Limits.MaxActiveRuns,
		MaxActiveRunsPerCaller:    cfg.Limits.MaxActiveRunsPerCaller,
		MaxActiveRunsPerWorkspace: cfg.Limits.MaxActiveRunsPerWorkspace,
		MaxSessionsPerCaller:      cfg.Limits.MaxSessionsPerCaller,
	}, sessionStore, metricsReg.IncCounter)

	handler := openai.NewHandler(openai.Options{
		Registry:         reg,
		Store:            sessionStore,
		Backend:          backend,
		CallerTokens:     callerTokens,
		SessionRecordTTL: cfg.Sessions.RecordTTL,
		PruneInterval:    cfg.Sessions.ReapInterval,
		Runs:             runtime.NewMemoryRunStore(),
		Logger:           logger,
		Metrics:          metricsReg,
		Admission:        admit,
		AdmissionLimits: openai.AdmissionLimits{
			MaxActiveRuns:             cfg.Limits.MaxActiveRuns,
			MaxActiveRunsPerCaller:    cfg.Limits.MaxActiveRunsPerCaller,
			MaxActiveRunsPerWorkspace: cfg.Limits.MaxActiveRunsPerWorkspace,
			MaxSessionsPerCaller:      cfg.Limits.MaxSessionsPerCaller,
			MaxWorkers:                cfg.Limits.MaxWorkers,
		},
		Enabled: func(name string) bool {
			agent, ok := cfg.Agents[name]
			return !ok || agent.Enabled
		},
		Models:   agentModels(cfg.Agents),
		Commands: agentCommands(cfg.Agents),
	})

	// Harden the listener without breaking long-lived SSE turns: set header and
	// idle bounds but deliberately NOT WriteTimeout/ReadTimeout, which would cut
	// off a streaming completion. Turn duration remains governed by the run
	// deadline and client cancellation.
	srv := &http.Server{
		Addr:              cfg.Server.ListenAddr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
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
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.Server.DrainTimeout)
	if err := srv.Shutdown(drainCtx); err != nil {
		slog.Warn("gateway http shutdown", "error", err)
	}
	cancelDrain()
	handler.Close()
	workerCtx, cancelWorkers := context.WithTimeout(context.Background(), cfg.Worker.StopGracePeriod+2*time.Second)
	defer cancelWorkers()
	if err := backend.Supervisor().Close(workerCtx); err != nil {
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
