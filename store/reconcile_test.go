package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/store"
)

// reconcileBackends pairs a session store and run store sharing one backend.
type reconcileBackends struct {
	name     string
	sessions runtime.SessionStore
	runs     runtime.RunStore
	close    func()
}

func newReconcileBackends(t *testing.T) []reconcileBackends {
	t.Helper()
	boltPath := t.TempDir() + "/gateway.db"
	bs, err := store.OpenBolt(boltPath, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	return []reconcileBackends{
		{
			name:     "memory",
			sessions: runtime.NewMemorySessionStore(),
			runs:     runtime.NewMemoryRunStore(),
			close:    func() {},
		},
		{
			name:     "bbolt",
			sessions: bs,
			runs:     bs.Runs(),
			close:    func() { _ = bs.Close() },
		},
	}
}

// TestReconcileRestartRecovery is the restart-recovery contract: runs stuck
// in starting/running become outcome_unknown (never replayed), sessions stuck
// in turn_active return to active with their native session id preserved, and
// terminal/idle records are untouched. Runs against both backends.
func TestReconcileRestartRecovery(t *testing.T) {
	for _, b := range newReconcileBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			defer b.close()
			ctx := context.Background()

			// Sessions: one stuck mid-turn, one idle, one closing.
			stuck := runtime.SessionRecord{ID: "s-stuck", CallerID: "alice", ModelID: "codex",
				Status: runtime.SessionTurnActive, NativeSessionID: "native-9"}
			idle := runtime.SessionRecord{ID: "s-idle", CallerID: "alice", ModelID: "codex",
				Status: runtime.SessionActive}
			closing := runtime.SessionRecord{ID: "s-closing", CallerID: "alice", ModelID: "codex",
				Status: runtime.SessionClosing}
			for _, rec := range []runtime.SessionRecord{stuck, idle, closing} {
				if err := b.sessions.Create(ctx, rec); err != nil {
					t.Fatalf("Create session %s: %v", rec.ID, err)
				}
			}

			// Runs: one starting, one running, one already terminal.
			runs := []runtime.RunRecord{
				{ID: "r-starting", SessionID: "s-stuck", CallerID: "alice", Status: runtime.RunStarting},
				{ID: "r-running", SessionID: "s-stuck", CallerID: "alice", Status: runtime.RunRunning},
				{ID: "r-done", SessionID: "s-idle", CallerID: "alice", Status: runtime.RunSucceeded, FinishedAt: time.Now()},
			}
			for _, rec := range runs {
				if err := b.runs.Create(ctx, rec); err != nil {
					t.Fatalf("Create run %s: %v", rec.ID, err)
				}
			}

			now := time.Now()
			report, err := store.Reconcile(ctx, b.sessions, b.runs, now)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if report.SessionsReset != 1 {
				t.Fatalf("SessionsReset = %d, want 1", report.SessionsReset)
			}
			if report.RunsMarkedUnknown != 2 {
				t.Fatalf("RunsMarkedUnknown = %d, want 2", report.RunsMarkedUnknown)
			}

			// The stuck session is active again, native session id preserved
			// (the next request resumes the native agent session), and a new
			// turn may begin immediately.
			got, err := b.sessions.Get(ctx, "s-stuck", "alice")
			if err != nil {
				t.Fatalf("Get stuck session: %v", err)
			}
			if got.Status != runtime.SessionActive {
				t.Fatalf("stuck session status = %q, want %q", got.Status, runtime.SessionActive)
			}
			if got.NativeSessionID != "native-9" {
				t.Fatalf("NativeSessionID = %q, want preserved %q", got.NativeSessionID, "native-9")
			}
			if err := b.sessions.BeginTurn(ctx, "s-stuck", "alice"); err != nil {
				t.Fatalf("BeginTurn after reconcile: %v", err)
			}
			if err := b.sessions.EndTurn(ctx, "s-stuck", "alice"); err != nil {
				t.Fatalf("EndTurn: %v", err)
			}

			for _, rec := range []runtime.SessionRecord{idle, closing} {
				got, err := b.sessions.Get(ctx, rec.ID, rec.CallerID)
				if err != nil {
					t.Fatalf("Get %s: %v", rec.ID, err)
				}
				if got.Status != rec.Status {
					t.Fatalf("%s status = %q, want untouched %q", rec.ID, got.Status, rec.Status)
				}
			}

			// Non-terminal runs are marked outcome_unknown with the restart
			// error code and a finish stamp; the terminal run is untouched.
			for _, id := range []string{"r-starting", "r-running"} {
				got, err := b.runs.Get(ctx, id)
				if err != nil {
					t.Fatalf("Get run %s: %v", id, err)
				}
				if got.Status != runtime.RunOutcomeUnknown {
					t.Fatalf("run %s status = %q, want %q", id, got.Status, runtime.RunOutcomeUnknown)
				}
				if got.ErrorCode != store.RestartErrorCode {
					t.Fatalf("run %s ErrorCode = %q, want %q", id, got.ErrorCode, store.RestartErrorCode)
				}
				if got.FinishedAt.IsZero() {
					t.Fatalf("run %s FinishedAt not stamped", id)
				}
				if !got.Status.Terminal() {
					t.Fatalf("run %s must be terminal after reconcile", id)
				}
			}
			done, err := b.runs.Get(ctx, "r-done")
			if err != nil {
				t.Fatalf("Get r-done: %v", err)
			}
			if done.Status != runtime.RunSucceeded || done.ErrorCode != "" {
				t.Fatalf("terminal run was touched: %+v", done)
			}

			// A second pass is a no-op (idempotent).
			again, err := store.Reconcile(ctx, b.sessions, b.runs, now)
			if err != nil {
				t.Fatalf("second Reconcile: %v", err)
			}
			if again.SessionsReset != 0 || again.RunsMarkedUnknown != 0 {
				t.Fatalf("second Reconcile = %+v, want zero", again)
			}
		})
	}
}

// TestReconcileAfterBoltReopen proves the reconcile path against a database
// that was closed and reopened — the real restart shape: records written by
// the previous process are repaired after reopen.
func TestReconcileAfterBoltReopen(t *testing.T) {
	path := t.TempDir() + "/gateway.db"
	ctx := context.Background()

	bs, err := store.OpenBolt(path, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	if err := bs.Create(ctx, runtime.SessionRecord{ID: "s1", CallerID: "alice", ModelID: "codex",
		Status: runtime.SessionTurnActive, NativeSessionID: "native-1"}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := bs.Runs().Create(ctx, runtime.RunRecord{ID: "r1", SessionID: "s1", CallerID: "alice", Status: runtime.RunRunning}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.OpenBolt(path, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	report, err := store.Reconcile(ctx, reopened, reopened.Runs(), time.Now())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report.SessionsReset != 1 || report.RunsMarkedUnknown != 1 {
		t.Fatalf("report = %+v, want 1 session reset / 1 run unknown", report)
	}
	got, err := reopened.Get(ctx, "s1", "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != runtime.SessionActive || got.NativeSessionID != "native-1" {
		t.Fatalf("session after reopen+reconcile: %+v", got)
	}
}

// nonEnumeratingSessionStore wraps a SessionStore without the enumerator
// extension (interface embedding promotes only the interface's own methods).
type nonEnumeratingSessionStore struct{ runtime.SessionStore }

type nonEnumeratingRunStore struct{ runtime.RunStore }

// TestReconcileFailsClosedWithoutEnumeration: a backend that cannot enumerate
// its records must fail closed at startup, never silently skip repair.
func TestReconcileFailsClosedWithoutEnumeration(t *testing.T) {
	ctx := context.Background()
	mem := runtime.NewMemorySessionStore()
	memRuns := runtime.NewMemoryRunStore()

	if _, err := store.Reconcile(ctx, nonEnumeratingSessionStore{mem}, memRuns, time.Now()); err == nil {
		t.Fatal("Reconcile must fail for a session store without enumeration")
	}
	if _, err := store.Reconcile(ctx, mem, nonEnumeratingRunStore{memRuns}, time.Now()); err == nil {
		t.Fatal("Reconcile must fail for a run store without enumeration")
	}
	// Sanity: the wrapped stores were not mutated by the failed passes.
	if _, err := mem.(runtime.SessionEnumerator).ListAllSessions(ctx); err != nil {
		t.Fatalf("underlying store broken: %v", err)
	}
}
