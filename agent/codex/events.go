package codex

type EventKind string

const (
	EventText          EventKind = "text"
	EventToolUse       EventKind = "tool_use"
	EventToolResult    EventKind = "tool_result"
	EventPermission    EventKind = "permission"
	EventUsage         EventKind = "usage"
	EventError         EventKind = "error"
	EventFinish        EventKind = "finish"
	EventNativeSession EventKind = "native_session"
)

type Input struct{ Prompt string }

type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	Result    string
	IsError   bool
}

type PermissionRequest struct {
	ID     string
	Action string
	Detail string
}

type Event struct {
	Kind            EventKind
	Text            string
	Tool            *ToolCall
	Permission      *PermissionRequest
	Usage           *Usage
	Err             error
	FinishReason    string
	NativeSessionID string
}
