package main

import (
	"context"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/store"
)

// TestReconcileStoresWrapper exercises the startup wrapper over the in-memory
// stores: stuck records are repaired and the report reflects the counts.
// (Backend-parity and bbolt-reopen coverage lives in store/reconcile_test.go;
// this pins the gateway wiring path.)
func TestReconcileStoresWrapper(t *testing.T) {
	ctx := context.Background()
	sessions := runtime.NewMemorySessionStore()
	runs := runtime.NewMemoryRunStore()

	if err := sessions.Create(ctx, runtime.SessionRecord{ID: "s1", CallerID: "alice", ModelID: "codex",
		Status: runtime.SessionTurnActive, NativeSessionID: "native-1"}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := runs.Create(ctx, runtime.RunRecord{ID: "r1", SessionID: "s1", CallerID: "alice", Status: runtime.RunRunning}); err != nil {
		t.Fatalf("Create run: %v", err)
	}

	report, err := reconcileStores(ctx, sessions, runs, time.Now())
	if err != nil {
		t.Fatalf("reconcileStores: %v", err)
	}
	if report.SessionsReset != 1 || report.RunsMarkedUnknown != 1 {
		t.Fatalf("report = %+v, want 1/1", report)
	}

	got, err := sessions.Get(ctx, "s1", "alice")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.Status != runtime.SessionActive || got.NativeSessionID != "native-1" {
		t.Fatalf("session after reconcile: %+v", got)
	}
	run, err := runs.Get(ctx, "r1")
	if err != nil {
		t.Fatalf("Get run: %v", err)
	}
	if run.Status != runtime.RunOutcomeUnknown || run.ErrorCode != store.RestartErrorCode {
		t.Fatalf("run after reconcile: %+v", run)
	}
}

// TestReconcileStoresEmptyIsNoOp: a fresh boot over empty stores reports zero
// and no error (the normal first-start path).
func TestReconcileStoresEmptyIsNoOp(t *testing.T) {
	report, err := reconcileStores(context.Background(), runtime.NewMemorySessionStore(), runtime.NewMemoryRunStore(), time.Now())
	if err != nil {
		t.Fatalf("reconcileStores: %v", err)
	}
	if report.SessionsReset != 0 || report.RunsMarkedUnknown != 0 {
		t.Fatalf("report = %+v, want zero", report)
	}
}
