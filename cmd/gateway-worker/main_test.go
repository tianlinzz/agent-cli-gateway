package main

import (
	"context"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
)

type contextProbeAdapter struct{}
type contextProbeSession struct {
	ctx    context.Context
	events chan runtime.Event
}

func (contextProbeAdapter) Describe(context.Context) (runtime.Descriptor, error) {
	return runtime.Descriptor{ModelID: "probe", LifecycleMode: runtime.LifecyclePersistentProcess}, nil
}
func (contextProbeAdapter) Start(ctx context.Context, _ runtime.StartRequest) (runtime.Session, error) {
	return &contextProbeSession{ctx: ctx, events: make(chan runtime.Event)}, nil
}
func (s *contextProbeSession) Send(context.Context, runtime.Input) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
		return nil
	}
}
func (s *contextProbeSession) Events() <-chan runtime.Event { return s.events }
func (s *contextProbeSession) Abort(context.Context) error  { return nil }
func (s *contextProbeSession) Close(context.Context) error  { close(s.events); return nil }

func TestAdapterHandlerStartSession_DoesNotBindSessionToRPCContext(t *testing.T) {
	reg := runtime.NewRegistry()
	if err := reg.Register("probe", func(context.Context, string, runtime.AdapterConfig) (runtime.AgentAdapter, error) {
		return contextProbeAdapter{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	h := &adapterHandler{reg: reg}
	rpcCtx, cancel := context.WithCancel(context.Background())
	if _, err := h.StartSession(rpcCtx, worker.StartSessionReq{ModelID: "probe", SessionID: "s", CallerID: "c", WorkspaceID: "w"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := h.SendInput(context.Background(), runtime.Input{}); err != nil {
		t.Fatalf("session inherited canceled RPC context: %v", err)
	}
}

// TestStartSessionPassesTypedConfigToFactory_OF08 pins the O-F08 contract:
// the worker hands the deployment config straight to the adapter factory via
// runtime.AdapterConfig — the old typed-config -> os.Setenv -> re-parse
// roundtrip is gone. The provider model must override the agent's
// configured default model.
func TestStartSessionPassesTypedConfigToFactory_OF08(t *testing.T) {
	var got runtime.AdapterConfig
	reg := runtime.NewRegistry()
	if err := reg.Register("probe", func(_ context.Context, _ string, cfg runtime.AdapterConfig) (runtime.AgentAdapter, error) {
		got = cfg
		return contextProbeAdapter{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	h := &adapterHandler{reg: reg}
	req := worker.StartSessionReq{
		ModelID:     "probe",
		SessionID:   "s",
		CallerID:    "c",
		WorkspaceID: "w",
		AgentConfig: runtime.AgentExecutionConfig{
			Command:      []string{"/opt/tools/probe"},
			DefaultModel: "base-model",
			Permission:   "deny",
			Env:          map[string]string{"PROBE_FLAG": "1"},
		},
		ProviderModel: "provider-model",
	}
	if _, err := h.StartSession(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !slicesEqual(got.Execution.Command, []string{"/opt/tools/probe"}) || got.Execution.Permission != "deny" {
		t.Fatalf("execution config not delivered: %#v", got.Execution)
	}
	if got.Execution.DefaultModel != "provider-model" {
		t.Fatalf("default model = %q, want provider model override", got.Execution.DefaultModel)
	}
	if got.Execution.Env["PROBE_FLAG"] != "1" {
		t.Fatalf("env map not delivered: %#v", got.Execution.Env)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
