package claudecode

// EventKind identifies a Claude-native event.
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

// Input is one prompt delivered to the persistent native conversation.
type Input struct {
	Prompt string
}

// Usage is Claude token accounting for one turn.
type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

// ToolCall is a Claude-native tool invocation or result.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	Result    string
	IsError   bool
}

// PermissionRequest describes a native can_use_tool request.
type PermissionRequest struct {
	ID     string
	Action string
	Detail string
}

// Event is emitted by a native Claude Code session.
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
