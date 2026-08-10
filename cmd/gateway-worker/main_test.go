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
	if err := reg.Register("probe", func(context.Context, string) (runtime.AgentAdapter, error) { return contextProbeAdapter{}, nil }); err != nil {
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
