package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/store"
)

// reconcileStores is the gateway startup's restart-recovery pass over the
// metadata stores (ROADMAP §4.3). The core rules live in store.Reconcile so
// the integration suite can drive them directly; this wrapper adds the
// process-level logging. It runs once, before the API starts serving, and its
// error is fatal at the call site: starting half-reconciled would let a
// session stuck in turn_active reject every new turn (ErrSessionBusy) and
// leave phantom starting/running runs behind.
func reconcileStores(ctx context.Context, sessions runtime.SessionStore, runs runtime.RunStore, now time.Time) (store.ReconcileReport, error) {
	report, err := store.Reconcile(ctx, sessions, runs, now)
	if err != nil {
		return report, err
	}
	if report.SessionsReset > 0 || report.RunsMarkedUnknown > 0 {
		slog.Warn("reconciled metadata after restart",
			"sessions_reset", report.SessionsReset,
			"runs_marked_unknown", report.RunsMarkedUnknown,
		)
	}
	return report, nil
}
