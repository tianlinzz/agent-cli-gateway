package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/config"
	"github.com/tianlinzz/agent-cli-gateway/core"
	"github.com/tianlinzz/agent-cli-gateway/server"
)

var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	port := flag.Int("port", 4096, "HTTP listen port")
	token := flag.String("token", "", "Bearer token for API authentication (empty = no auth)")
	dataDir := flag.String("data-dir", "", "Data directory for agent transcripts (empty = default)")
	corsOrigins := flag.String("cors", "", "Comma-separated CORS origins (empty = allow all)")
	userIDHeader := flag.String("user-id-header", "X-User-Id", "HTTP header name carrying caller identity")
	identityMode := flag.String("identity-mode", "strict", "identity mode: strict (missing header → 401) or anonymous (missing → default user)")
	maxSessionsPerUser := flag.Int("max-sessions-per-user", 5, "max concurrent sessions per caller (0 = unlimited)")
	sessionIdleTTL := flag.Duration("session-idle-ttl", 2*time.Hour, "idle session TTL before reaper evicts (0 = unlimited)")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		slog.Info("agent-cli-gateway", "version", version, "commit", commit, "buildTime", buildTime)
		return
	}

	// Token env fallback for containerized deployment.
	// Priority: CLI flag (-token) > env var (GATEWAY_TOKEN) > empty (no auth).
	// Lets the cloud platform inject the token via env config without overriding CMD.
	if *token == "" {
		*token = os.Getenv("GATEWAY_TOKEN")
	}

	// Identity-mode env fallback for containerized deployment.
	// GATEWAY_ANONYMOUS_IDENTITY=1 is a shorthand for identity-mode=anonymous.
	if *identityMode == "strict" && os.Getenv("GATEWAY_ANONYMOUS_IDENTITY") == "1" {
		*identityMode = "anonymous"
		slog.Info("identity mode: anonymous (GATEWAY_ANONYMOUS_IDENTITY=1)")
	}

	// Agent drivers are registered via blank imports in plugin_agent_*.go
	agentNames := core.ListRegisteredAgents()
	slog.Info("registered agents", "agents", agentNames)

	// Build agent singletons from registered factories.
	agents := make(map[string]core.Agent)
	for _, name := range agentNames {
		agent, err := core.CreateAgent(name, map[string]any{})
		if err != nil {
			slog.Warn("failed to create agent, skipping", "agent", name, "error", err)
			continue
		}
		agents[name] = agent
		slog.Info("agent ready", "name", name)
	}

	if len(agents) == 0 {
		slog.Error("no agents available, exiting")
		os.Exit(1)
	}

	cfg := config.GatewayConfig{
		Port:               *port,
		Token:              *token,
		DataDir:            *dataDir,
		CORSOrigins:        parseCORS(*corsOrigins),
		UserIDHeader:       *userIDHeader,
		IdentityMode:       *identityMode,
		MaxSessionsPerUser: *maxSessionsPerUser,
		SessionIdleTTL:     *sessionIdleTTL,
	}

	store := server.NewSessionStoreWithLimits(agents, cfg.MaxSessionsPerUser, cfg.SessionIdleTTL)
	srv := server.NewServer(cfg, store)

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	slog.Info("stopped")
}

func parseCORS(s string) []string {
	if s == "" {
		return nil
	}
	var origins []string
	for _, o := range strings.Split(s, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			origins = append(origins, o)
		}
	}
	return origins
}
