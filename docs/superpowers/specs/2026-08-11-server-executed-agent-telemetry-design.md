# Server-Executed Agent Telemetry Design

## Status

Approved design for implementation planning. This document covers only
`agent-cli-gateway`. The corresponding f1-web consumer, persistence, and UI
work is a separate follow-up task.

## Problem

The Gateway exposes autonomous coding agents through an OpenAI-compatible Chat
Completions API. Codex, Claude Code, and Kimi execute their own native tools
inside their persistent Agent runtimes. The current OpenAI projection correctly
hides native `tool_use` and `tool_result` events because exposing them as
standard OpenAI `tool_calls` transfers execution ownership to the caller.

An outer Agent runtime such as LangGraph treats `AIMessage.tool_calls` as a
control signal: it executes a ToolNode, appends a tool-result message, and calls
the model again. If the Gateway exposed an already-executed native tool this
way, the outer runtime could repeat a side effect and start a second Agent loop.
Detecting the returned tool message at the Gateway would be too late because
the outer tool execution has already happened.

The Gateway nevertheless needs to make safe, user-visible progress available
to clients. The missing contract is display-only telemetry for reasoning and
completed native tool executions.

## Ownership Rule

The process that chooses and executes a tool owns the entire tool lifecycle.

- Codex, Claude Code, and Kimi own their native tool loops.
- An OpenAI client calling the Gateway does not execute those native tools.
- Native tools never become OpenAI `tool_calls`, tool-role messages, or
  `finish_reason: "tool_calls"`.
- The Gateway may expose native tool activity only as display-only telemetry.
- The outer client remains free to define its own tools for other model
  providers, but those tools are not part of a Gateway Agent turn.

This keeps the cloud Agent runtime isolated from any client-side Agent runtime.

## Public Wire Contract

The Gateway retains the standard Chat Completions SSE envelope. Standard text,
usage, errors, completion IDs, session headers, and finish reasons keep their
existing behavior.

### Assistant Text

Assistant text remains standard OpenAI content:

```json
{
  "choices": [
    {
      "index": 0,
      "delta": { "content": "The workspace is ready." },
      "finish_reason": null
    }
  ]
}
```

### Unified Process Channel

All Gateway-owned process telemetry uses the established OpenAI-compatible
`reasoning_content` delta field. This includes safe reasoning summaries and
terminal summaries for native tool executions. The client already has one
consumer path for this field, so adding a new `tool_execution` wire field is
not required.

Reasoning example:

```json
{
  "choices": [
    {
      "index": 0,
      "delta": { "reasoning_content": "Checking the workspace state." },
      "finish_reason": null
    }
  ]
}
```

```text
Checking the workspace state.
```

This field carries only native protocol content explicitly suitable for user
display, such as a reasoning summary. The Gateway must not synthesize hidden
chain-of-thought, signatures, encrypted blocks, or private provider metadata.
An Agent that has no safe reasoning event emits none.

### Terminal Native Tool Summary

Each native tool produces at most one terminal display summary, encoded as a
string in the same `reasoning_content` field:

```json
{
  "choices": [
    {
      "index": 0,
      "delta": {
        "reasoning_content": "gateway.tool_execution.v1:{\"id\":\"tool-123\",\"name\":\"Bash\",\"status\":\"completed\",\"is_error\":false,\"result\":\"/workspace\"}"
      },
      "finish_reason": null
    }
  ]
}
```

The payload after the stable prefix is JSON with these fields:

```go
type ToolExecutionSummary struct {
    ID      string `json:"id"`
    Name    string `json:"name,omitempty"`
    Status  string `json:"status"` // always "completed" in v1
    IsError bool   `json:"is_error"`
    Result  string `json:"result,omitempty"`
}
```

The `gateway.tool_execution.v1:` prefix is stable and makes the message
recognizable to consumers that want a dedicated tool card later. Consumers that
do not recognize it still receive an ordinary reasoning string and do not enter
any tool loop. This is an application-level convention carried through an
already-consumed compatibility field, not a new OpenAI control field.

The Gateway does not stream `started` or incremental tool updates in v1. It
keeps native start data internally and emits one event only when the matching
result arrives. Multiple tools yield one terminal event per tool, in completion
order.

### Turn Completion

Native tool execution never changes the OpenAI finish reason to `tool_calls`.
A successful autonomous Agent turn ends with the existing final chunk:

```json
{
  "choices": [
    {
      "index": 0,
      "delta": {},
      "finish_reason": "stop"
    }
  ]
}
```

`max_tokens` and timeout behavior continue to map to `length` as they do now.

## Runtime Contract

The existing canonical tool events remain unchanged:

- `runtime.EventToolUse` announces a native tool start.
- `runtime.EventToolResult` carries the native result and error state.

The canonical runtime adds a reasoning event that is independent of any Agent
name:

```go
const EventReasoning EventType = "reasoning"

type Reasoning struct {
    ID   string
    Text string
}
```

`runtime.Event` gains `Reasoning *Reasoning`. Reasoning events are ordered text
deltas or complete safe summaries. The OpenAI streaming layer projects each
non-empty `Text` value as `delta.reasoning_content`. A separate phase state is
not needed in v1: clients infer reasoning completion when assistant content or
turn completion arrives.

The runtime package stays transport-agnostic and Agent-name-agnostic. It does
not reference OpenAI field names or switch on `codex`, `claude-code`, or `kimi`.

## Native Agent Mapping

### Codex

The Codex app-server layer maps safe reasoning summary notifications to native
reasoning events. Command, file-change, MCP, and dynamic-tool item starts and
completions continue to map to native tool-use and tool-result events.

Raw hidden reasoning and provider-private item data remain inside the Codex
protocol boundary.

### Claude Code

The Claude stream-json layer maps displayable thinking deltas or summaries to
native reasoning events. `tool_use` and correlated `tool_result` blocks retain
their existing native mapping. Thinking signatures and private metadata are not
forwarded.

### Kimi

The Kimi ACP layer maps a protocol-defined displayable thought update to native
reasoning when the negotiated ACP version supplies one. Tool call starts and
results retain their existing native mapping. If the ACP stream has no stable,
safe reasoning event, Kimi emits no reasoning rather than synthesizing it.

Supporting the common contract does not require every Agent to emit every
optional event.

## OpenAI Streaming Projection

The streaming projection maintains per-turn tool correlation state:

```go
started   map[string]runtime.ToolCall
completed map[string]struct{}
```

Projection rules are:

1. `EventText` writes `delta.content` as today.
2. `EventReasoning` writes `delta.reasoning_content` when text is non-empty.
3. `EventToolUse` records the tool by stable ID and writes no client frame.
4. `EventToolResult` merges missing name data from the recorded start, applies
   redaction and output bounds, serializes `ToolExecutionSummary` with the
   `gateway.tool_execution.v1:` prefix, and writes it as one
   `delta.reasoning_content` frame.
5. A repeated result ID is ignored and logged as a structured warning.
6. A result without a prior start is still emitted using the available result
   data; this preserves protocols that report only terminal updates.
7. A start without a result does not become a successful terminal event. A
   later Agent error or process failure follows the existing error path.
8. Events received after a terminal finish are ignored under the existing
   turn-settlement rules.

The non-streaming Chat Completions response remains final text, usage, and
finish reason only in v1. Time-ordered telemetry is a streaming capability.

## Defensive Input Handling

Normal operation does not require a second request containing a tool result.
The caller never receives native `tool_calls`, so it has nothing to execute or
return.

As defense in depth, the Gateway should reject an inbound tool-role message or
assistant tool call that claims an ID previously used by a server-executed
native tool in the same Gateway session. It must not pass that message to the
native Agent or replay the preceding user prompt. The response is a typed
invalid-request error explaining that server-executed tool results must not be
submitted by the client.

This check is an error guard, not a normal continuation mechanism. Arbitrary
client tool messages that cannot be proven to refer to a Gateway-native tool
retain their current request normalization behavior until client-owned tools
receive a separate design.

## Security And Size Limits

Tool telemetry can contain commands, paths, file-change summaries, and remote
tool output. Before writing a terminal event, the OpenAI projection must:

- enforce a bounded result length;
- reuse or centralize existing native result bounds rather than expanding them;
- redact authorization headers, cookies, tokens, API keys, and known secret
  field names recursively;
- never include the complete process environment;
- omit values that cannot be serialized safely and log a structured warning;
- avoid logging the unredacted result when serialization or writing fails.

Arguments are intentionally absent from the v1 terminal wire event. The UI can
show the tool name and bounded result without exposing commands, write payloads,
or secrets. A later arguments-preview feature requires its own threat review.

## Compatibility

- Existing standard content, reasoning, and finish chunks remain unchanged.
- `delta.tool_calls`, assistant `message.tool_calls`, tool-role response
  messages, and `finish_reason: "tool_calls"` are never emitted for native
  Agent tools.
- Clients already consuming `reasoning_content` receive both reasoning and
  terminal tool summaries without a new response-field adapter.
- Clients that do not understand the stable prefix still receive harmless
  reasoning text and never enter a tool loop.
- Missing telemetry is not an execution failure.
- The API and runtime layers contain no Agent-specific branches.

The initial implementation emits these process summaries only on streaming
responses. No client capability negotiation is required because the wire field
already exists and the new value remains a string.

## Testing

### Native packages

Each supported Agent package needs fixture coverage proving:

- safe reasoning is mapped when the native protocol supplies it;
- private reasoning metadata is not exposed;
- tool start/result IDs remain stable;
- each native tool result is emitted at most once;
- missing optional reasoning is accepted.

### Adapters

Codex, Claude Code, and Kimi adapter tests must verify exact mapping of native
reasoning, tool-use, tool-result, error, usage, and finish events into the
canonical runtime contract.

### OpenAI API

Handler tests must verify:

- reasoning becomes `delta.reasoning_content`;
- a tool start produces no SSE frame;
- a tool result produces one terminal `delta.reasoning_content` frame with the
  `gateway.tool_execution.v1:` prefix;
- missing result fields are correlated from the start event;
- duplicate results are suppressed;
- no response contains `delta.tool_calls` or `finish_reason: "tool_calls"`;
- final assistant content still streams after internal tools;
- non-streaming responses remain unchanged;
- write failure and cancellation still settle the turn;
- defensive replay of a known server-executed tool ID is rejected.

### Integration

The HTTP integration fixture must exercise this ordering:

```text
reasoning_content
internal tool start (no frame)
reasoning_content(tool_execution completed)
assistant content
finish_reason stop
[DONE]
```

It must also prove that the request results in exactly one backend turn and
that no client tool-result continuation is required.

Concurrency-sensitive changes must pass the race detector. API/runtime/worker
changes require the existing integration suite and cross-platform builds from
the repository pre-commit checklist.

## Deferred Work

The following work is deliberately outside this Gateway implementation:

- f1-web parsing of the `gateway.tool_execution.v1:` reasoning payload;
- conversion to f1-web `ChatStreamProtocol` display events and dedicated UI;
- UI rendering and historical persistence;
- client-owned OpenAI function tools;
- incremental tool progress or argument previews;
- non-streaming telemetry aggregation;
- a general tracing or management API.

These items may build on this contract but must not weaken the execution
ownership rule.
