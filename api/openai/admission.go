package openai

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// AdmissionScope identifies which limit was exceeded.
type AdmissionScope string

const (
	ScopeGlobal    AdmissionScope = "global"
	ScopeCaller    AdmissionScope = "caller"
	ScopeWorkspace AdmissionScope = "workspace"
	ScopeWorkers   AdmissionScope = "workers"
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

// AdmissionController enforces global, per-caller, and per-workspace limits on
// concurrently active runs. It is the API-layer admission gate before BeginTurn
// (O-F10). Per-agent max_concurrency remains an adapter capacity limit at the
// supervisor level and composes with these limits.
type AdmissionController struct {
	maxActiveRuns             int
	maxActiveRunsPerCaller    int
	maxActiveRunsPerWorkspace int

	activeRuns atomic.Int64

	mu            sync.Mutex
	callerRuns    map[string]int
	workspaceRuns map[string]int

	store  runtime.SessionStore
	metric func(name string, value int64, labels map[string]string)
}

// NewAdmissionController creates an admission controller. Zero-valued limits
// mean unlimited. The store is used for session-count-per-caller checks. The
// metric callback (may be nil) records admission rejections.
func NewAdmissionController(limits struct {
	MaxActiveRuns             int
	MaxActiveRunsPerCaller    int
	MaxActiveRunsPerWorkspace int
	MaxSessionsPerCaller      int
}, store runtime.SessionStore, metric func(string, int64, map[string]string)) *AdmissionController {
	return &AdmissionController{
		maxActiveRuns:             limits.MaxActiveRuns,
		maxActiveRunsPerCaller:    limits.MaxActiveRunsPerCaller,
		maxActiveRunsPerWorkspace: limits.MaxActiveRunsPerWorkspace,
		callerRuns:                make(map[string]int),
		workspaceRuns:             make(map[string]int),
		store:                     store,
		metric:                    metric,
	}
}

// AdmissionLimits holds all configurable admission limits. Zero-valued fields
// are unlimited.
type AdmissionLimits struct {
	MaxActiveRuns             int
	MaxActiveRunsPerCaller    int
	MaxActiveRunsPerWorkspace int
	MaxSessionsPerCaller      int
	MaxWorkers                int
}

// Acquire checks all active-run limits and increments the counters. Callers
// MUST call Release on the same callerID/workspaceID when the turn ends.
// sessionIsNew indicates whether this request creates a new session (for the
// per-caller session-count check).
func (a *AdmissionController) Acquire(ctx context.Context, callerID, workspaceID string, limits AdmissionLimits, sessionIsNew bool) error {
	// Check per-caller session count first (only for new sessions).
	if sessionIsNew && limits.MaxSessionsPerCaller > 0 && a.store != nil {
		sessions, err := a.store.List(ctx, callerID)
		if err == nil && len(sessions) >= limits.MaxSessionsPerCaller {
			a.reject(ScopeSessions, "per_caller")
			return &ErrAdmissionRejected{Scope: ScopeSessions, Reason: "per_caller"}
		}
	}

	// Global active runs.
	if limits.MaxActiveRuns > 0 && a.activeRuns.Load() >= int64(limits.MaxActiveRuns) {
		a.reject(ScopeGlobal, "active_runs")
		return &ErrAdmissionRejected{Scope: ScopeGlobal, Reason: "active_runs"}
	}

	a.mu.Lock()
	// Per-caller active runs.
	if limits.MaxActiveRunsPerCaller > 0 && a.callerRuns[callerID] >= limits.MaxActiveRunsPerCaller {
		a.mu.Unlock()
		a.reject(ScopeCaller, "active_runs")
		return &ErrAdmissionRejected{Scope: ScopeCaller, Reason: "active_runs"}
	}
	// Per-workspace active runs.
	if limits.MaxActiveRunsPerWorkspace > 0 && a.workspaceRuns[workspaceID] >= limits.MaxActiveRunsPerWorkspace {
		a.mu.Unlock()
		a.reject(ScopeWorkspace, "active_runs")
		return &ErrAdmissionRejected{Scope: ScopeWorkspace, Reason: "active_runs"}
	}

	// All checks passed — commit.
	a.activeRuns.Add(1)
	a.callerRuns[callerID]++
	a.workspaceRuns[workspaceID]++
	a.mu.Unlock()
	return nil
}

// Release decrements the counters. Safe to call unconditionally (e.g., in a
// defer) even when Acquire was not called or failed.
func (a *AdmissionController) Release(callerID, workspaceID string) {
	a.activeRuns.Add(-1)
	a.mu.Lock()
	defer a.mu.Unlock()
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

func (a *AdmissionController) reject(scope AdmissionScope, reason string) {
	if a.metric != nil {
		a.metric("gateway_admission_rejections_total", 1, map[string]string{
			"scope":  string(scope),
			"reason": string(reason),
		})
	}
}
