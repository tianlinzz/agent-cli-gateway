package worker

import (
	"context"
	"fmt"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// LocalExecutionBackend is the API layer's ONLY execution path. It implements
// runtime.ExecutionBackend by turning a canonical runtime.StartRequest into a
// worker RPC start, backed by the Supervisor which forks one nsjail-wrapped
// worker per session. A future cross-node deployment replaces this backend,
// never the adapter or API contract.
type LocalExecutionBackend struct {
	sup *Supervisor
}

// NewLocalExecutionBackend builds a backend around a new Supervisor.
func NewLocalExecutionBackend(cfg Config, opts ...Option) (*LocalExecutionBackend, error) {
	sup, err := NewSupervisor(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &LocalExecutionBackend{sup: sup}, nil
}

// Start maps a runtime.StartRequest to a worker RPC session and returns the
// canonical ExecutionHandle.
func (b *LocalExecutionBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.ExecutionHandle, error) {
	ws, err := b.sup.StartSession(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("backend: %w", err)
	}
	return ws, nil
}

// Preflight verifies the nsjail sandbox boundary. The API layer maps a
// failure to readiness 503.
func (b *LocalExecutionBackend) Preflight(ctx context.Context) error {
	return b.sup.Preflight(ctx)
}

// Supervisor exposes the underlying supervisor for shutdown coordination.
func (b *LocalExecutionBackend) Supervisor() *Supervisor {
	return b.sup
}
