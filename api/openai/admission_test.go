package openai

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func newTestAdmission(limits AdmissionLimits) *AdmissionController {
	return NewAdmissionController(limits, nil, nil)
}

func mustAcquire(t *testing.T, ac *AdmissionController, callerID, workspaceID string) func() {
	t.Helper()
	release, err := ac.Acquire(context.Background(), callerID, workspaceID)
	if err != nil {
		t.Fatalf("acquire %s/%s: %v", callerID, workspaceID, err)
	}
	return release
}

func TestAdmission_PerCallerRunLimit(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{MaxActiveRunsPerCaller: 1})
	ctx := context.Background()

	r1 := mustAcquire(t, ac, "caller-a", "ws-1")
	_, err := ac.Acquire(ctx, "caller-a", "ws-1")
	if err == nil {
		t.Fatal("second acquire from same caller should fail")
	}
	var ae *ErrAdmissionRejected
	if !errors.As(err, &ae) || ae.Scope != ScopeCaller {
		t.Fatalf("expected ScopeCaller rejection, got %v", err)
	}

	// Different caller succeeds.
	r2 := mustAcquire(t, ac, "caller-b", "ws-1")

	// Release and re-acquire.
	r1()
	if _, err := ac.Acquire(ctx, "caller-a", "ws-1"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	r2()
}

func TestAdmission_GlobalRunLimit(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{MaxActiveRuns: 2})
	ctx := context.Background()

	ra := mustAcquire(t, ac, "a", "w")
	rb := mustAcquire(t, ac, "b", "w")
	_, err := ac.Acquire(ctx, "c", "w")
	if err == nil {
		t.Fatal("third acquire should fail (global limit)")
	}
	var ae *ErrAdmissionRejected
	if !errors.As(err, &ae) || ae.Scope != ScopeGlobal {
		t.Fatalf("expected ScopeGlobal rejection, got %v", err)
	}

	ra()
	if _, err := ac.Acquire(ctx, "c", "w"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	rb()
}

// TestAdmission_GlobalRunLimit_ConcurrentNoOversubscription is a regression
// test for the Phase 2 code review: the global active-run check used to run
// outside the commit lock, so concurrent callers could all read the same
// below-cap value and then each commit, exceeding MaxActiveRuns. Many
// concurrent acquirers must never hold more than the cap at once.
func TestAdmission_GlobalRunLimit_ConcurrentNoOversubscription(t *testing.T) {
	const (
		callers    = 16
		goroutines = 8
		maxRuns    = 4
	)
	ac := newTestAdmission(AdmissionLimits{MaxActiveRuns: maxRuns})

	var inFlight atomic.Int64
	var maxSeen atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for c := 0; c < callers; c++ {
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				<-start
				callerID := string(rune('a'+c%26)) + "caller"
				ws := callerID + "-ws"
				release, err := ac.Acquire(context.Background(), callerID, ws)
				if err != nil {
					return
				}
				cur := inFlight.Add(1)
				for {
					old := maxSeen.Load()
					if cur <= old || maxSeen.CompareAndSwap(old, cur) {
						break
					}
				}
				inFlight.Add(-1)
				release()
			}(c)
		}
	}
	close(start)
	wg.Wait()

	if got := maxSeen.Load(); got > maxRuns {
		t.Fatalf("concurrent holders oversubscribed the global cap: max %d > cap %d", got, maxRuns)
	}
	if got := ac.ActiveRuns(); got != 0 {
		t.Fatalf("active runs after all releases = %d, want 0", got)
	}
}

func TestAdmission_PerWorkspaceRunLimit(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{MaxActiveRunsPerWorkspace: 1})
	ctx := context.Background()

	r1 := mustAcquire(t, ac, "a", "ws-1")
	if _, err := ac.Acquire(ctx, "b", "ws-1"); err == nil {
		t.Fatal("second in same workspace should fail")
	}
	// Different workspace is fine.
	r2 := mustAcquire(t, ac, "c", "ws-2")
	r1()
	r2()
}

func TestAdmission_ZeroLimitsMeansUnlimited(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{})

	for i := 0; i < 100; i++ {
		if _, err := ac.Acquire(context.Background(), "caller", "ws"); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
}

// TestAdmission_ReleaseClosureExactOnce is a regression test for the final
// review (P2): the old named Release was not actually idempotent — with two
// holders on the same caller/workspace, a double release stole the other
// holder's count. The per-reservation closure must release exactly once even
// when called repeatedly while another holder is active.
func TestAdmission_ReleaseClosureExactOnce(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{MaxActiveRuns: 4})
	ctx := context.Background()

	r1 := mustAcquire(t, ac, "a", "w")
	r2 := mustAcquire(t, ac, "a", "w")
	if got := ac.ActiveRuns(); got != 2 {
		t.Fatalf("active runs = %d, want 2", got)
	}

	// Double-calling r1 must not touch r2's reservation.
	r1()
	r1()
	if got := ac.ActiveRuns(); got != 1 {
		t.Fatalf("active runs after double release = %d, want 1 (stole another holder)", got)
	}
	// The remaining holder's release still works exactly once.
	r2()
	r2()
	if got := ac.ActiveRuns(); got != 0 {
		t.Fatalf("active runs = %d, want 0", got)
	}
	// Capacity is fully returned.
	if _, err := ac.Acquire(ctx, "a", "w"); err != nil {
		t.Fatalf("acquire after releases: %v", err)
	}
}

// TestAdmission_WorkersPrecheck verifies the max_workers fast precheck
// (O-F10): when the live worker count is at the cap, admission rejects with
// ScopeWorkers before any session/turn state is created.
func TestAdmission_WorkersPrecheck(t *testing.T) {
	var live atomic.Int64
	live.Store(2)
	ac := NewAdmissionController(AdmissionLimits{MaxWorkers: 2}, func() int { return int(live.Load()) }, nil)
	ctx := context.Background()

	_, err := ac.Acquire(ctx, "a", "w")
	if err == nil {
		t.Fatal("acquire at worker cap should fail")
	}
	var ae *ErrAdmissionRejected
	if !errors.As(err, &ae) || ae.Scope != ScopeWorkers || ae.Reason != "max_workers" {
		t.Fatalf("expected ScopeWorkers/max_workers rejection, got %v", err)
	}

	live.Store(1)
	release := mustAcquire(t, ac, "a", "w")
	release()
}

// TestAdmission_WorkersPrecheckSkippedWithoutProbe verifies a nil worker
// probe disables the precheck (the backend capacity error stays the authority).
func TestAdmission_WorkersPrecheckSkippedWithoutProbe(t *testing.T) {
	ac := newTestAdmission(AdmissionLimits{MaxWorkers: 1})
	if _, err := ac.Acquire(context.Background(), "a", "w"); err != nil {
		t.Fatalf("nil workers probe must disable the precheck: %v", err)
	}
}
