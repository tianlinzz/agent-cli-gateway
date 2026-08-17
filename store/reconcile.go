package store

import (
	"context"
	"fmt"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// RestartErrorCode is the ErrorCode stamped on runs whose completion outcome
// became unknown because the gateway process restarted mid-turn.
const RestartErrorCode = "gateway_restart"

// ReconcileReport summarizes a startup reconciliation pass.
type ReconcileReport struct {
	// SessionsReset is how many turn_active sessions were reset to active.
	SessionsReset int
	// RunsMarkedUnknown is how many starting/running runs were marked
	// outcome_unknown.
	RunsMarkedUnknown int
}

// Reconcile repairs persisted session/run metadata after a gateway restart.
// It is deliberately fail-closed: a store that cannot enumerate its records
// (does not implement the optional runtime.SessionEnumerator /
// runtime.RunEnumerator extension) is an error, never silently skipped.
//
// Rules (ROADMAP §4.3):
//   - runs in starting/running can no longer have a completion in flight after
//     a restart, so they are marked outcome_unknown with
//     ErrorCode="gateway_restart" and FinishedAt=now. They are NEVER replayed.
//   - sessions stuck in turn_active are reset to active (no turn survives a
//     process restart); NativeSessionID is preserved so the next request
//     resumes the native agent session.
//
// Both the in-memory runtime stores and BoltStore implement the enumerator
// extensions, so this one routine handles every backend. (For the memory
// driver a fresh process starts with empty stores, so Reconcile is a no-op
// there — but running it uniformly keeps the startup path backend-agnostic.)
func Reconcile(ctx context.Context, sessions runtime.SessionStore, runs runtime.RunStore, now time.Time) (ReconcileReport, error) {
	var report ReconcileReport
	se, ok := sessions.(runtime.SessionEnumerator)
	if !ok {
		return report, fmt.Errorf("store: reconcile: session store %T cannot enumerate records (implements no runtime.SessionEnumerator)", sessions)
	}
	re, ok := runs.(runtime.RunEnumerator)
	if !ok {
		return report, fmt.Errorf("store: reconcile: run store %T cannot enumerate records (implements no runtime.RunEnumerator)", runs)
	}

	allSessions, err := se.ListAllSessions(ctx)
	if err != nil {
		return report, fmt.Errorf("store: reconcile: list sessions: %w", err)
	}
	for _, rec := range allSessions {
		if rec.Status != runtime.SessionTurnActive {
			continue
		}
		if _, err := sessions.Update(ctx, rec.ID, rec.CallerID, func(r *runtime.SessionRecord) {
			// Re-check inside the mutation: only reset a session that is still
			// stuck in turn_active.
			if r.Status == runtime.SessionTurnActive {
				r.Status = runtime.SessionActive
			}
		}); err != nil {
			return report, fmt.Errorf("store: reconcile: reset session %q: %w", rec.ID, err)
		}
		report.SessionsReset++
	}

	allRuns, err := re.ListAllRuns(ctx)
	if err != nil {
		return report, fmt.Errorf("store: reconcile: list runs: %w", err)
	}
	for _, rec := range allRuns {
		if rec.Status != runtime.RunStarting && rec.Status != runtime.RunRunning {
			continue
		}
		if _, err := runs.Update(ctx, rec.ID, func(r *runtime.RunRecord) {
			if r.Status == runtime.RunStarting || r.Status == runtime.RunRunning {
				r.Status = runtime.RunOutcomeUnknown
				r.FinishedAt = now
				r.ErrorCode = RestartErrorCode
			}
		}); err != nil {
			return report, fmt.Errorf("store: reconcile: mark run %q outcome unknown: %w", rec.ID, err)
		}
		report.RunsMarkedUnknown++
	}
	return report, nil
}
