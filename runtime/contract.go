// Package runtime defines the canonical agent-gateway contract: the types
// exchanged between the API layer, the adapter registry, and the execution
// backends. It is deliberately transport-agnostic — it imports only the
// standard library and never references HTTP, gRPC, Unix sockets, platform/IM
// types, or any specific adapter implementation. Transport (worker RPC over
// per-session Unix sockets) is a worker-layer concern and stays out of this
// package.
package runtime

import "context"

// Descriptor is the static description of an adapter as seen by the model
// registry and the public /v1/models discovery endpoint.
type Descriptor struct {
	// ModelID is the public model identifier (an adapter name, e.g. "alpha").
	ModelID string
	// DisplayName is a human-readable name for the agent.
	DisplayName string
	// Description is an optional one-line summary.
	Description string
	// LifecycleMode declares how the adapter drives its CLI across turns:
	// LifecyclePersistentProcess or LifecycleResumePerTurn.
	LifecycleMode string
	// Capabilities advertises the optional features this adapter supports.
	Capabilities Capabilities
}

// Lifecycle modes for Descriptor.LifecycleMode. Workers must not assume every
// agent uses the same session model.
const (
	// LifecyclePersistentProcess keeps one CLI process alive across turns.
	LifecyclePersistentProcess = "persistent_process"
	// LifecycleResumePerTurn restarts the CLI for each turn, resuming via a
	// native session ID.
	LifecycleResumePerTurn = "resume_per_turn"
)

// Capabilities describes optional adapter features. Permission is a canonical
// capability: even in phase 1 where permission is auto-approved, the event
// surface must keep the ability to request permission.
type Capabilities struct {
	Streaming  bool
	ToolCalls  bool
	Reasoning  bool
	Permission bool
	Resume     bool
	MultiTurn  bool
}

// StartRequest carries everything needed to start a new agent session. The
// client supplies only an opaque WorkspaceID; the server resolves it to a
// controlled directory. An absolute workDir can never be injected through the
// public contract.
type StartRequest struct {
	// ModelID is the public model (agent) identifier requested by the client.
	ModelID string
	// SessionID is the gateway-assigned session identifier.
	SessionID string
	// OwnerID identifies the tenant/user that owns the session.
	OwnerID string
	// WorkspaceID is an opaque client-supplied identifier. The server maps it
	// to a controlled directory; clients can never inject an absolute path.
	WorkspaceID string
	// Metadata carries opaque request metadata (e.g. gateway session headers).
	Metadata map[string]string
	// FirstInput, when non-nil, is delivered as the first turn.
	FirstInput *Input
}

// Input is a single turn delivered to an agent session.
type Input struct {
	// Messages is the conversation for this turn.
	Messages []Message
	// Tools lists tool definitions available to the agent.
	Tools []Tool
	// Metadata carries per-turn opaque metadata.
	Metadata map[string]string
}

// Message is a single canonical conversation message.
type Message struct {
	Role       string // "system", "user", "assistant", or "tool"
	Content    string
	Name       string // optional sender/function name
	ToolCalls  []ToolCall
	ToolCallID string // set on tool-result messages
}

// Tool describes a callable tool exposed to an agent.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema fragment describing accepted arguments.
	Parameters map[string]any
}

// ToolCall describes either a tool invocation requested by the agent
// (EventToolUse) or the result of one (EventToolResult).
type ToolCall struct {
	// ID correlates a tool result with the original call.
	ID string
	// Name is the tool being invoked.
	Name string
	// Arguments carries the tool call arguments (map form, JSON Schema based).
	Arguments map[string]any
	// Result and IsError are set on tool-result events.
	Result string
	// IsError reports whether the tool execution failed.
	IsError bool
}

// AgentAdapter is the canonical adapter contract. Adapters are registered by
// string name in a Registry and created through their factory.
type AgentAdapter interface {
	// Describe returns the adapter's static descriptor.
	Describe(ctx context.Context) (Descriptor, error)
	// Start begins a new session for the given request.
	Start(ctx context.Context, req StartRequest) (Session, error)
}

// Session is a running agent session.
type Session interface {
	// Send delivers a turn to the session.
	Send(ctx context.Context, input Input) error
	// Events returns the stream of canonical runtime events for this session.
	Events() <-chan Event
	// Abort cancels the in-flight turn.
	Abort(ctx context.Context) error
	// Close tears the session down and releases all resources.
	Close(ctx context.Context) error
}

// ExecutionBackend is the API layer's only way to execute a model request.
// Transport details (worker processes, gRPC, Unix sockets, nsjail) are hidden
// behind the backend; a future cross-node deployment replaces only this
// interface, never the adapter or API contract.
type ExecutionBackend interface {
	// Start launches an execution for the request and returns its handle.
	Start(ctx context.Context, req StartRequest) (ExecutionHandle, error)
}

// ExecutionHandle is the API layer's handle to a running execution. It
// mirrors the canonical Session method set so the API layer can drive a
// session end-to-end without knowing how the execution is transported.
type ExecutionHandle interface {
	Send(ctx context.Context, input Input) error
	Events() <-chan Event
	Abort(ctx context.Context) error
	Close(ctx context.Context) error
}
