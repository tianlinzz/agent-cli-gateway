// Package events defines the canonical native agent event contract shared by
// every agent/<name> package (D1). Before this package existed, codex, kimi,
// and claudecode each carried a byte-identical copy of these types and the
// copies had already started drifting; there is now exactly one native event
// shape and each agent package re-exports it under its own name for
// readability at its call sites.
//
// This is the NATIVE layer of the event pipeline. Adapters
// (adapters/<name>) map these events onto runtime.Event, which is the only
// shape that crosses the API boundary. The package imports only the standard
// library: it must stay transport-agnostic and must never reference runtime,
// adapters, worker, or api (enforced by internal/archtest).
package events

// EventKind identifies one native agent event.
type EventKind string

// The native event kinds. These deliberately mirror the runtime.EventType
// values; the adapter layer maps them one-to-one.
const (
	EventText          EventKind = "text"
	EventReasoning     EventKind = "reasoning"
	EventToolUse       EventKind = "tool_use"
	EventToolResult    EventKind = "tool_result"
	EventPermission    EventKind = "permission"
	EventUsage         EventKind = "usage"
	EventError         EventKind = "error"
	EventFinish        EventKind = "finish"
	EventNativeSession EventKind = "native_session"
)

// Input is one prompt delivered to a persistent native conversation.
type Input struct {
	Prompt string
}

// Usage is native token accounting for one turn. Kimi reports only the total
// (input/output stay zero); codex and claudecode fill all three counters.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// Reasoning carries safe, displayable native reasoning text. Only native
// protocol fields explicitly documented as safe summaries may flow into it.
type Reasoning struct {
	ID   string
	Text string
}

// ToolCall is a native tool invocation or its result.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	Result    string
	IsError   bool
}

// PermissionRequest describes a native permission request. Even when the
// gateway auto-approves, the event surface keeps the ability to ask.
type PermissionRequest struct {
	ID     string
	Action string
	Detail string
}

// Event is emitted by a native agent session on its Events() channel. The
// session owns the channel and closes it at teardown.
type Event struct {
	Kind            EventKind
	Text            string
	Reasoning       *Reasoning
	Tool            *ToolCall
	Permission      *PermissionRequest
	Usage           *Usage
	Err             error
	FinishReason    string
	NativeSessionID string
}
