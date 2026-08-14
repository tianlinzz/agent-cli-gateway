package openai

import (
	"context"
	"fmt"
	"sync"
)

// AdmissionScope identifies which limit was exceeded.
type AdmissionScope string

const (
	ScopeGlobal    AdmissionScope = "global"
	ScopeCaller    AdmissionScope = "caller"
	ScopeWorkspace AdmissionScope = "workspace"
	ScopeWorkers   AdmissionScope = "workers"
	ScopeAgent     AdmissionScope = "agent"
	ScopeSessions  AdmissionScope = "sessions"
)

// ErrAdmissionRejected is returned when a request exceeds a configured limit.
type ErrAdmissionRejected struct {
	Scope  AdmissionScope
	Reason string
}

func (e *ErrAdmissionRejected) Error() string {
	return fmt.Sprintf("admission rejected: %s %s", e.Scope, e.Reason)
}

// AdmissionLimits holds all configurable admission limits. Zero-valued fields
// are unlimited. Session record caps (global and per-caller) are NOT admission
// limits: they are enforced atomically by the SessionStore's Create (see
// runtime.ErrSessionCapacity) because the check must share a lock with the
// insert. MaxWorkers is a fast precheck here; the supervisor remains the
// authoritative owner of the worker cap and reports capacity through
// runtime.ErrCapacityExceeded.
type AdmissionLimits struct {
	MaxActiveRuns             int
	MaxActiveRunsPerCaller    int
	MaxActiveRunsPerWorkspace int
	MaxWorkers                int
}

// AdmissionController enforces global, per-caller, and per-workspace limits on
// concurrently active runs. It is the API-layer admission gate before BeginTurn
// (O-F10). Per-agent max_concurrency remains an adapter capacity limit at the
// supervisor level and composes with these limits.
//
// The limits are fixed at construction; Acquire/Release are the only mutating
// operations and every counter is guarded by mu so the limit check and the
// counter commit are one atomic step (concurrent acquires can never
// oversubscribe the global cap).
type AdmissionController struct {
	limits AdmissionLimits
	// workers reports the number of live worker processes for the MaxWorkers
	// precheck. May be nil (no precheck); the backend's capacity error then
	// remains the authoritative rejection.
	workers func() int

	mu            sync.Mutex
	activeRuns    int
	callerRuns    map[string]int
	workspaceRuns map[string]int

	metric func(name string, value int64, labels map[string]string)
}

// NewAdmissionController creates an admission controller. Zero-valued limits
// mean unlimited. workers (may be nil) reports the live worker count for the
// MaxWorkers fast precheck. The metric callback (may be nil) records admission
// rejections.
func NewAdmissionController(limits AdmissionLimits, workers func() int, metric func(string, int64, map[string]string)) *AdmissionController {
	return &AdmissionController{
		limits:        limits,
		workers:       workers,
		callerRuns:    make(map[string]int),
		workspaceRuns: make(map[string]int),
		metric:        metric,
	}
}

// Acquire checks all active-run limits and, on success, reserves one slot and
// returns a release closure for exactly that reservation. The closure is safe
// to call more than once (and to defer immediately) — each reservation
// releases exactly once. A plain named Release keyed by caller/workspace could
// not be idempotent: with two concurrent holders on the same key, a double
// release would steal the other holder's count.
func (a *AdmissionController) Acquire(ctx context.Context, callerID, workspaceID string) (func(), error) {
	// Worker-cap precheck: reject at the admission boundary, before any
	// session/turn state is created. This is a fast path only — the
	// supervisor re-checks under its own lock and wraps
	// runtime.ErrCapacityExceeded, which the API maps to 429 as well.
	if a.limits.MaxWorkers > 0 && a.workers != nil && a.workers() >= a.limits.MaxWorkers {
		a.reject(ScopeWorkers, "max_workers")
		return nil, &ErrAdmissionRejected{Scope: ScopeWorkers, Reason: "max_workers"}
	}

	a.mu.Lock()
	// Global active runs: checked under the same lock as the commit. A
	// lock-free pre-check here allowed two concurrent callers to read the
	// same below-cap value and both commit, oversubscribing the cap.
	if a.limits.MaxActiveRuns > 0 && a.activeRuns >= a.limits.MaxActiveRuns {
		a.mu.Unlock()
		a.reject(ScopeGlobal, "active_runs")
		return nil, &ErrAdmissionRejected{Scope: ScopeGlobal, Reason: "active_runs"}
	}
	// Per-caller active runs.
	if a.limits.MaxActiveRunsPerCaller > 0 && a.callerRuns[callerID] >= a.limits.MaxActiveRunsPerCaller {
		a.mu.Unlock()
		a.reject(ScopeCaller, "active_runs")
		return nil, &ErrAdmissionRejected{Scope: ScopeCaller, Reason: "active_runs"}
	}
	// Per-workspace active runs.
	if a.limits.MaxActiveRunsPerWorkspace > 0 && a.workspaceRuns[workspaceID] >= a.limits.MaxActiveRunsPerWorkspace {
		a.mu.Unlock()
		a.reject(ScopeWorkspace, "active_runs")
		return nil, &ErrAdmissionRejected{Scope: ScopeWorkspace, Reason: "active_runs"}
	}

	// All checks passed — commit.
	a.activeRuns++
	a.callerRuns[callerID]++
	a.workspaceRuns[workspaceID]++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { a.release(callerID, workspaceID) })
	}, nil
}

// release decrements the counters of ONE committed reservation. Counters
// never go negative.
func (a *AdmissionController) release(callerID, workspaceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeRuns > 0 {
		a.activeRuns--
	}
	if a.callerRuns[callerID] > 0 {
		a.callerRuns[callerID]--
		if a.callerRuns[callerID] == 0 {
			delete(a.callerRuns, callerID)
		}
	}
	if a.workspaceRuns[workspaceID] > 0 {
		a.workspaceRuns[workspaceID]--
		if a.workspaceRuns[workspaceID] == 0 {
			delete(a.workspaceRuns, workspaceID)
		}
	}
}

// ActiveRuns reports the number of currently admitted (in-flight) runs.
func (a *AdmissionController) ActiveRuns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeRuns
}

func (a *AdmissionController) reject(scope AdmissionScope, reason string) {
	if a.metric != nil {
		a.metric("gateway_admission_rejections_total", 1, map[string]string{
			"scope":  string(scope),
			"reason": string(reason),
		})
	}
}
