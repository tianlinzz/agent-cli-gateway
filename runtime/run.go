package runtime

import "time"

// RunStatus is the lifecycle state of one gateway run (a single Agent turn).
// Every terminal path maps to exactly one status, including the
// outcome-unknown case where the completion result cannot be confirmed.
type RunStatus string

const (
	// RunStarting means the run has been created but execution has not begun.
	RunStarting RunStatus = "starting"
	// RunRunning means the native Agent is actively processing the turn.
	RunRunning RunStatus = "running"
	// RunSucceeded means the turn completed normally with a finish marker.
	RunSucceeded RunStatus = "succeeded"
	// RunFailed means the native Agent reported a confirmed error.
	RunFailed RunStatus = "failed"
	// RunCancelled means the client cancelled and the interrupt was confirmed.
	RunCancelled RunStatus = "cancelled"
	// RunTimedOut means the turn exceeded its deadline and was interrupted.
	RunTimedOut RunStatus = "timed_out"
	// RunOutcomeUnknown means the connection was lost or the worker crashed
	// before the completion could be confirmed.
	RunOutcomeUnknown RunStatus = "outcome_unknown"
)

// Terminal reports whether the status is a terminal (final) state.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunOutcomeUnknown:
		return true
	}
	return false
}

// Valid reports whether s is a known RunStatus.
func (s RunStatus) Valid() bool {
	switch s {
	case RunStarting, RunRunning, RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunOutcomeUnknown:
		return true
	}
	return false
}

// RunRecord is the metadata of one gateway run (a single Agent turn within a
// session). Like SessionRecord it is pure data: only strings, a time.Time, a
// status enum, and a value Usage — a value copy is a deep copy, so the store
// hands out records without leaking internal state.
type RunRecord struct {
	// ID is the gateway-assigned run identifier (returned as X-Gateway-Run-Id).
	ID string
	// SessionID is the gateway session this run belongs to.
	SessionID string
	// CallerID is the tenant/user that owns the run.
	CallerID string
	// WorkspaceID is the opaque workspace identifier for this run.
	WorkspaceID string
	// ModelID is the PUBLIC model/agent identifier exactly as the caller
	// requested it (it may differ from the adapter id when models are
	// mapped); observability metrics key by the bounded adapter id instead.
	ModelID string

	// Status is the run lifecycle state.
	Status RunStatus

	// StartedAt is when the run was created; immutable after Create.
	StartedAt time.Time
	// FinishedAt is stamped when the run reaches a terminal status; zero means
	// the run is still in flight.
	FinishedAt time.Time

	// ErrorCode is a short machine-readable error code for failed/unknown runs.
	ErrorCode string
	// Usage is the token accounting for this run (may be zero if the agent did
	// not report usage before terminating).
	Usage Usage
}
