package openai

import (
	"context"
	"errors"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

func newTestAdmission(limits AdmissionLimits) *AdmissionController {
	return NewAdmissionController(struct {
		MaxActiveRuns             int
		MaxActiveRunsPerCaller    int
		MaxActiveRunsPerWorkspace int
		MaxSessionsPerCaller      int
	}{
		MaxActiveRuns:             limits.MaxActiveRuns,
		MaxActiveRunsPerCaller:    limits.MaxActiveRunsPerCaller,
		MaxActiveRunsPerWorkspace: limits.MaxActiveRunsPerWorkspace,
		MaxSessionsPerCaller:      limits.MaxSessionsPerCaller,
	}, runtime.NewMemorySessionStore(), nil)
}

func TestAdmission_PerCallerRunLimit(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{MaxActiveRunsPerCaller: 1})
	ctx := context.Background()

	if err := ac.Acquire(ctx, "caller-a", "ws-1", AdmissionLimits{MaxActiveRunsPerCaller: 1}, false); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	err := ac.Acquire(ctx, "caller-a", "ws-1", AdmissionLimits{MaxActiveRunsPerCaller: 1}, false)
	if err == nil {
		t.Fatal("second acquire from same caller should fail")
	}
	var ae *ErrAdmissionRejected
	if !errors.As(err, &ae) || ae.Scope != ScopeCaller {
		t.Fatalf("expected ScopeCaller rejection, got %v", err)
	}

	// Different caller succeeds.
	if err := ac.Acquire(ctx, "caller-b", "ws-1", AdmissionLimits{MaxActiveRunsPerCaller: 1}, false); err != nil {
		t.Fatalf("different caller: %v", err)
	}

	// Release and re-acquire.
	ac.Release("caller-a", "ws-1")
	if err := ac.Acquire(ctx, "caller-a", "ws-1", AdmissionLimits{MaxActiveRunsPerCaller: 1}, false); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestAdmission_GlobalRunLimit(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{})
	ctx := context.Background()
	limits := AdmissionLimits{MaxActiveRuns: 2}

	ac.Acquire(ctx, "a", "w", limits, false)
	ac.Acquire(ctx, "b", "w", limits, false)
	err := ac.Acquire(ctx, "c", "w", limits, false)
	if err == nil {
		t.Fatal("third acquire should fail (global limit)")
	}
	var ae *ErrAdmissionRejected
	if !errors.As(err, &ae) || ae.Scope != ScopeGlobal {
		t.Fatalf("expected ScopeGlobal, got %v", err)
	}

	ac.Release("a", "w")
	if err := ac.Acquire(ctx, "c", "w", limits, false); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestAdmission_PerWorkspaceRunLimit(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{})
	ctx := context.Background()
	limits := AdmissionLimits{MaxActiveRunsPerWorkspace: 1}

	if err := ac.Acquire(ctx, "a", "ws-1", limits, false); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := ac.Acquire(ctx, "b", "ws-1", limits, false)
	if err == nil {
		t.Fatal("second in same workspace should fail")
	}
	// Different workspace is fine.
	if err := ac.Acquire(ctx, "c", "ws-2", limits, false); err != nil {
		t.Fatalf("different workspace: %v", err)
	}
}

func TestAdmission_ZeroLimitsMeansUnlimited(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{})
	ctx := context.Background()
	limits := AdmissionLimits{} // all zero = unlimited

	for i := 0; i < 100; i++ {
		if err := ac.Acquire(ctx, "caller", "ws", limits, false); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
}

func TestAdmission_ReleaseIsIdempotent(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{})
	ctx := context.Background()
	limits := AdmissionLimits{MaxActiveRuns: 1}

	ac.Acquire(ctx, "a", "w", limits, false)
	ac.Release("a", "w")
	// Double release should not panic or go negative.
	ac.Release("a", "w")
	// Counter should be 0, so a new acquire works.
	if err := ac.Acquire(ctx, "a", "w", limits, false); err != nil {
		t.Fatalf("acquire after double release: %v", err)
	}
}

func TestAdmission_PerCallerSessionLimit(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ac := NewAdmissionController(struct {
		MaxActiveRuns             int
		MaxActiveRunsPerCaller    int
		MaxActiveRunsPerWorkspace int
		MaxSessionsPerCaller      int
	}{MaxSessionsPerCaller: 2}, store, nil)
	ctx := context.Background()
	limits := AdmissionLimits{MaxSessionsPerCaller: 2}

	// Create 2 sessions for caller-a.
	store.Create(ctx, runtime.SessionRecord{ID: "s1", CallerID: "caller-a", Status: runtime.SessionActive})
	store.Create(ctx, runtime.SessionRecord{ID: "s2", CallerID: "caller-a", Status: runtime.SessionActive})

	// A third new session should be rejected.
	err := ac.Acquire(ctx, "caller-a", "ws", limits, true)
	if err == nil {
		t.Fatal("third session should be rejected")
	}
	var ae *ErrAdmissionRejected
	if !errors.As(err, &ae) || ae.Scope != ScopeSessions {
		t.Fatalf("expected ScopeSessions, got %v", err)
	}
}

func TestRateLimitedError_OF10(t *testing.T) {
	ae := rateLimited("caller", "active_runs")
	if ae.Type != errorTypeRateLimit {
		t.Errorf("type = %q, want %q", ae.Type, errorTypeRateLimit)
	}
	if ae.Code != "caller_active_runs" {
		t.Errorf("code = %q", ae.Code)
	}
}
