// Command gateway-worker is the per-session worker child process. The
// gateway's worker Supervisor forks one of these per session (wrapped in
// nsjail in prod/dev) over a per-session Unix socket. It serves the Worker
// gRPC contract by wiring the requested model's adapter (from the process-wide
// runtime registry, populated by the plugin_agent_*.go files in this package)
// into a runtime.Session.
//
// This binary is the ONLY place a concrete agent CLI is launched; the API
// process (cmd/gateway) never starts an agent CLI itself.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
)

func main() {
	socket := os.Getenv("GW_WORKER_SOCKET")
	if socket == "" {
		fmt.Fprintln(os.Stderr, "gateway-worker: GW_WORKER_SOCKET is not set")
		os.Exit(2)
	}

	// The adapter registry is process-wide: adapters register from init() via
	// the plugin_agent_*.go blank imports in this package.
	reg := runtime.DefaultRegistry()

	h := &adapterHandler{reg: reg}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	slog.Info("gateway-worker serving", "socket", socket, "adapters", reg.List())
	if err := worker.Serve(ctx, worker.UnixSocket(socket), h); err != nil {
		slog.Error("gateway-worker serve", "error", err)
		os.Exit(1)
	}
}

// adapterHandler implements worker.Handler over a single runtime.Session. One
// worker process serves exactly one session, so the handler owns one session
// at a time.
type adapterHandler struct {
	reg *runtime.Registry

	mu        sync.Mutex
	session   runtime.Session
	lifecycle string
}

func (h *adapterHandler) Health(context.Context) (string, error) {
	return "gateway-worker-1.0", nil
}

func (h *adapterHandler) StartSession(ctx context.Context, req worker.StartSessionReq) (string, error) {
	agentConfig := req.AgentConfig
	if req.ProviderModel != "" {
		agentConfig.DefaultModel = req.ProviderModel
	}
	applyAgentConfig(req.ModelID, agentConfig)
	adapter, err := h.reg.Resolve(ctx, req.ModelID)
	if err != nil {
		return "", fmt.Errorf("gateway-worker: resolve %q: %w", req.ModelID, err)
	}
	desc, err := adapter.Describe(ctx)
	if err != nil {
		return "", fmt.Errorf("gateway-worker: describe %q: %w", req.ModelID, err)
	}
	sess, err := adapter.Start(ctx, runtime.StartRequest{
		ModelID:     req.ModelID,
		SessionID:   req.SessionID,
		CallerID:    req.CallerID,
		WorkspaceID: req.WorkspaceID,
		Metadata:    req.Metadata,
		FirstInput:  req.FirstInput,
	})
	if err != nil {
		return "", fmt.Errorf("gateway-worker: start %q: %w", req.ModelID, err)
	}
	h.mu.Lock()
	h.session = sess
	h.lifecycle = desc.LifecycleMode
	h.mu.Unlock()
	return desc.LifecycleMode, nil
}

// applyAgentConfig bridges the transport-neutral deployment config to the
// adapter factories. A worker is single-session, so setting these variables
// here is isolated to this worker process and cannot leak across sessions.
func applyAgentConfig(model string, c runtime.AgentExecutionConfig) {
	key := strings.ToLower(model)
	if key == "claude-code" {
		key = "claude"
	}
	prefix := "CC_GATEWAY_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
	if c.Command != "" {
		_ = os.Setenv(prefix+"_COMMAND", c.Command)
	}
	if c.DefaultModel != "" {
		_ = os.Setenv(prefix+"_MODEL", c.DefaultModel)
	}
	if c.Permission != "" {
		_ = os.Setenv(prefix+"_PERMISSION", c.Permission)
	}
	if c.TurnTimeout > 0 && model == "kimi" {
		_ = os.Setenv(prefix+"_TIMEOUT_SECS", strconv.FormatInt(int64(c.TurnTimeout/time.Second), 10))
	}
	for k, v := range c.Env {
		_ = os.Setenv(k, v)
	}
}

func (h *adapterHandler) SendInput(ctx context.Context, input runtime.Input) error {
	h.mu.Lock()
	s := h.session
	h.mu.Unlock()
	if s == nil {
		return fmt.Errorf("gateway-worker: no session started")
	}
	return s.Send(ctx, input)
}

func (h *adapterHandler) Events() <-chan runtime.Event {
	h.mu.Lock()
	s := h.session
	h.mu.Unlock()
	if s == nil {
		// A closed channel signals the session ended.
		ch := make(chan runtime.Event)
		close(ch)
		return ch
	}
	return s.Events()
}

func (h *adapterHandler) Abort(ctx context.Context) error {
	h.mu.Lock()
	s := h.session
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Abort(ctx)
}

func (h *adapterHandler) Close(ctx context.Context) error {
	h.mu.Lock()
	s := h.session
	h.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close(ctx)
}
