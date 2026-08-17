package runtime

// EventType identifies the kind of a canonical runtime event.
type EventType string

const (
	// EventText carries an incremental text delta.
	EventText EventType = "text"
	// EventToolUse announces a tool call requested by the agent.
	EventToolUse EventType = "tool_use"
	// EventToolResult reports a tool execution result.
	EventToolResult EventType = "tool_result"
	// EventPermission carries a permission request raised by the agent.
	EventPermission EventType = "permission"
	// EventUsage reports token accounting for a turn.
	EventUsage EventType = "usage"
	// EventError reports an agent or execution failure.
	EventError EventType = "error"
	// EventFinish marks the end of a turn with a finish reason.
	EventFinish EventType = "finish"
	// EventStatus reports a session lifecycle change (e.g. "started",
	// "closed").
	EventStatus        EventType = "status"
	EventNativeSession EventType = "native_session"
	// EventReasoning carries safe, displayable reasoning text from the native
	// Agent protocol. It is never model tool-control data.
	EventReasoning EventType = "reasoning"
)

// FinishReasonTimeout is the synthesized finish reason for a turn ended by
// the gateway turn deadline (O-F09b). Native agents never produce it; the
// worker boundary emits it after a deadline abort settled. The OpenAI surface
// must present it as a timeout error — never as a normal completion with
// finish_reason "length" (a deadline is not a token-length limit).
const FinishReasonTimeout = "timeout"

// TurnDeadlineExceeded is the EventError message the worker boundary emits
// when a turn deadline fired and the native abort did NOT settle: the turn's
// true completion outcome is unknown. The API layer matches this sentinel to
// present the unified timeout error and record RunOutcomeUnknown.
const TurnDeadlineExceeded = "runtime: turn deadline exceeded (abort did not settle)"

// Event is the canonical runtime event emitted by sessions and executions.
// It is the only event shape that crosses the API boundary; worker RPC frames
// (a worker-layer concern) are defined elsewhere and converted to and from
// this type at the worker boundary.
type Event struct {
	Type EventType

	// Text is set for EventText.
	Text string

	// Tool is set for EventToolUse and EventToolResult.
	Tool *ToolCall

	// Permission is set for EventPermission.
	Permission *PermissionRequest

	// Usage is set for EventUsage.
	Usage *Usage

	// Reasoning is set for EventReasoning.
	Reasoning *Reasoning

	// Error is the failure description for EventError.
	Error string

	// FinishReason is set for EventFinish, e.g. "end_turn" or "max_tokens".
	FinishReason string

	// Status is set for EventStatus, e.g. "started" or "closed".
	Status string
	// NativeSessionID is emitted when an adapter learns the resumable native
	// conversation/thread identifier.
	NativeSessionID string
}

// Reasoning carries safe, displayable reasoning text from a native Agent.
// Native adapters must not populate it with hidden chain-of-thought or private
// provider metadata.
type Reasoning struct {
	ID   string
	Text string
}

// PermissionRequest is a canonical permission request raised by an agent.
// Phase 1 auto-approves inside the controlled workspace, but the capability
// and the event surface must exist in the contract from the first version.
type PermissionRequest struct {
	// ID is the agent-native request identifier used to respond.
	ID string
	// Action describes what the agent wants to do (e.g. "run_command").
	Action string
	// Detail provides additional context (e.g. the exact command or path).
	Detail string
}

// Usage reports token accounting for a turn.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}
