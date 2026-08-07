// Command gatewayd is the NEW agent-gateway entrypoint of the rewrite. It
// coexists with the legacy cmd/gateway binary (and its config.GatewayConfig in
// config/gateway.go), which remains the active entrypoint until the old
// implementation is removed (task 7).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tianlinzz/agent-cli-gateway/config"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to the gateway TOML config (empty = defaults)")
	showVersion := flag.Bool("version", false, "print version and exit")
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
	// this directory). No adapters are registered yet — task 4/6 add them.
	reg := runtime.DefaultRegistry()

	slog.Info("gatewayd starting",
		"version", version,
		"mode", cfg.Mode,
		"listen_addr", cfg.Server.ListenAddr,
		"workspace_root", cfg.Workspace.Root,
		"isolation_required", cfg.Isolation.Required,
		"nsjail_version", cfg.Isolation.NsjailVersion,
		"nsjail_source", cfg.Isolation.NsjailSource,
		"adapters", reg.List(),
	)

	// TODO(task5): wire the HTTP server (/health/*, /v1/models,
	// /v1/chat/completions) here. This task only establishes the entrypoint
	// and the empty architecture, so the process blocks until shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	slog.Info("gatewayd shutting down")
}
