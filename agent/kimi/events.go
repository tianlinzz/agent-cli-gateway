package kimi

type EventKind string

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

type Input struct{ Prompt string }
type Usage struct{ InputTokens, OutputTokens, TotalTokens int }
type Reasoning struct{ ID, Text string }
type ToolCall struct {
	ID, Name  string
	Arguments map[string]any
	Result    string
	IsError   bool
}
type PermissionRequest struct{ ID, Action, Detail string }
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
