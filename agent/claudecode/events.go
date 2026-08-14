package claudecode

// The native event contract is shared across all agents (agent/events, D1).
// These aliases re-export it under the package-local names the Claude Code
// session and its tests already use; there is deliberately no
// Claude-specific copy.
import "github.com/tianlinzz/agent-cli-gateway/agent/events"

type (
	EventKind         = events.EventKind
	Input             = events.Input
	Usage             = events.Usage
	Reasoning         = events.Reasoning
	ToolCall          = events.ToolCall
	PermissionRequest = events.PermissionRequest
	Event             = events.Event
)

const (
	EventText          = events.EventText
	EventReasoning     = events.EventReasoning
	EventToolUse       = events.EventToolUse
	EventToolResult    = events.EventToolResult
	EventPermission    = events.EventPermission
	EventUsage         = events.EventUsage
	EventError         = events.EventError
	EventFinish        = events.EventFinish
	EventNativeSession = events.EventNativeSession
)
