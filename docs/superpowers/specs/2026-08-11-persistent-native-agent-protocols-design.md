# Persistent Native Agent Protocols Design

**Date:** 2026-08-11

## Objective

Make every supported Agent session own one long-lived native Agent process:

- Claude Code continues to use its bidirectional stream-json process.
- Codex moves from `codex exec` to `codex app-server` JSON-RPC.
- Kimi Code moves from `kimi --prompt` to `kimi acp` JSON-RPC.

A normally completed turn ends only the turn. It must not terminate the Agent
process or Worker. Explicit session close, Gateway shutdown, an unrecoverable
protocol failure, or bounded cancellation escalation may terminate them.

## Decision

Use one persistent Agent protocol process per Gateway session, inside the
existing per-session Worker and nsjail boundary.

Rejected alternatives:

1. Keep Codex and Kimi resume-per-turn. This preserves avoidable startup cost,
   repeatedly initializes native tools and MCP servers, and makes turn abort
   indistinguishable from session teardown.
2. Run one node-wide Codex or Kimi daemon shared by many Gateway sessions. This
   conflicts with the current one-session/one-worker/one-workspace isolation
   model and increases the blast radius of a native process failure.
3. Maintain both persistent and resume-per-turn backends. The project has no
   legacy compatibility requirement, so retaining two implementations would
   double protocol and lifecycle surface without serving the target design.

## Canonical Architecture

```text
OpenAI HTTP/SSE
  -> runtime.ExecutionBackend
  -> persistent per-session Worker (gRPC over Unix socket)
  -> thin adapters/<agent>
  -> persistent agent/<agent> native protocol client
  -> one long-lived Agent CLI process
```

The Agent process may create short-lived tool subprocesses. Those subprocesses
are not the Agent session process and do not change the persistent lifecycle
contract.

Every supported adapter declares `runtime.LifecyclePersistentProcess`. The
`LifecycleResumePerTurn` mode and its Worker special cases are removed once all
three migrations are green.

## Shared JSON-RPC Transport

Create a bounded, concurrency-safe JSON-RPC 2.0 stdio client in
`agent/protocol` for Codex and Kimi. It owns only framing and correlation, never
Agent-specific method or event switches.

Responsibilities:

- start from an existing process stdin/stdout pair;
- emit monotonically increasing request IDs;
- correlate responses to pending requests;
- route notifications and reverse requests to a caller-owned handler;
- serialize writes so frames cannot interleave;
- reject malformed or oversized JSONL frames using the existing 10 MiB bound;
- fail every pending request exactly once when stdout closes or decoding fails;
- support request context cancellation without leaking the pending entry;
- keep stderr outside the protocol channel and include a bounded, redacted tail
  in terminal errors.

The client does not retry protocol requests. Session recovery happens at the
Worker/Gateway lifecycle boundary so a request is never executed twice.

## Codex Native Session

### Launch and handshake

Launch one process:

```text
codex app-server --listen stdio://
```

The implementation targets the generated Codex app-server v2 schema from the
installed CLI and uses the stable sequence:

```text
initialize
initialized
thread/start          (new Gateway session)
thread/resume         (recovering a stored native thread)
turn/start            (each runtime.Send)
turn/interrupt        (runtime.Abort)
```

`thread/start` receives the trusted workspace, configured model, reasoning
effort, approval policy, and sandbox policy. Client request metadata may not
inject paths or native Codex configuration.

### Event projection

Codex notifications are projected into native events before the thin adapter:

- assistant message deltas -> `EventText`;
- command/file/MCP/dynamic tool item start -> `EventToolUse`;
- corresponding completion -> `EventToolResult`;
- token usage -> `EventUsage`;
- thread ID -> `EventNativeSession`;
- completed turn -> exactly one `EventFinish`;
- failed turn or protocol failure -> exactly one terminal `EventError`.

Reasoning notifications remain native telemetry and never enter assistant text.
Native tool events remain internal telemetry and never become OpenAI
`tool_calls`.

### Reverse requests

Command and file-change approvals use the configured permission mode:

- `auto`: approve operations already constrained by the Gateway workspace and
  outer nsjail policy;
- `deny`: reject;
- `ask`: emit `EventPermission`, but because the phase-one OpenAI API has no
  permission-response endpoint, return a bounded rejection instead of hanging.

Interactive user-input requests are not silently approved. They receive a
deterministic cancellation/rejection because the OpenAI model surface cannot
answer them. No reverse request may block a turn indefinitely.

## Kimi Native Session

### Launch and handshake

Launch one process:

```text
kimi acp
```

Use the stable ACP sequence published by Kimi Code:

```text
initialize { protocolVersion: 1, clientCapabilities: {} }
session/new    { cwd, mcpServers: [] }       (new Gateway session)
session/resume { sessionId, cwd, mcpServers: [] } (recovery)
session/prompt { sessionId, prompt }         (each runtime.Send)
session/cancel { sessionId }                 (runtime.Abort notification)
```

The Gateway does not advertise client-side filesystem capabilities, so Kimi
executes filesystem and terminal tools inside its own Worker/nsjail workspace.
The returned ACP session ID is emitted immediately as `EventNativeSession`.

### Event projection

ACP `session/update` notifications are mapped as follows:

- `agent_message_chunk` -> `EventText`;
- tool-call create/update -> one `EventToolUse` plus one terminal
  `EventToolResult` per stable tool-call ID;
- usage, when supplied -> `EventUsage`;
- `session/prompt` response stop reason -> exactly one `EventFinish`;
- JSON-RPC or Agent failure -> exactly one terminal `EventError`.

History replay from `session/load` is not emitted as current-turn text. Recovery
uses `session/resume` where supported specifically to avoid replaying old
assistant messages into a new OpenAI response.

### Reverse requests

`session/request_permission` follows the same `auto`/`deny`/bounded-`ask`
policy as Codex. The initialize capabilities do not advertise unsupported
question UX. Ask-user interactions must be rejected or disabled, never left
pending.

## Turn and Session State

Each native session has these independent concepts:

```text
process: starting -> ready -> failed/closed
turn:    idle -> active -> finishing -> idle
```

Rules:

- only one active turn per Gateway session;
- `Send` while active returns a busy error and never queues a duplicate prompt;
- a normal finish returns the turn to idle and keeps the process ready;
- late events from a completed/aborted turn are ignored by turn identity;
- event channels have one owner and close only when the native session closes;
- `Abort` cancels only the active turn and waits for bounded settlement;
- if native cancellation does not settle, force-kill the Agent process, close
  the session, and let the next Gateway request recover from native session ID;
- `Close` cancels any active turn, terminates the process group, reaps it, and
  closes the event channel exactly once.

## Worker and API Lifecycle Changes

Worker RPC `Abort` means cancel-current-turn for every Agent. It must no longer
close a Worker merely because its descriptor is `persistent_process`.

The API keeps the execution handle after normal completion and after a
successfully settled abort. It drops the handle only when the event channel
closes, liveness reports terminal failure, or cancellation escalates to process
termination.

Client disconnect still cancels the active turn so unattended work does not
continue. With Codex `turn/interrupt` and Kimi `session/cancel`, this no longer
normally sacrifices session process continuity.

Crash recovery remains explicit:

1. store `NativeSessionID` as soon as the native server creates/loads it;
2. drop and reap a terminal Worker handle;
3. on the next request, start a new Worker and Agent process;
4. resume the stored Codex thread or Kimi ACP session;
5. send only the newest user turn.

No automatic replay/retry occurs for a turn whose completion status is
unknown.

## Discovery and Configuration

- Codex discovery requires `codex app-server` support. There is no `exec`
  fallback.
- Kimi discovery requires `kimi acp` support. There is no `--prompt` fallback.
- A CLI that exists but lacks the required persistent protocol is omitted from
  `/v1/models` in auto-discovery and produces a precise startup error when
  explicitly enabled.
- Remove the Codex backend selector and Kimi print/stream-json probing after the
  persistent paths replace them.
- Existing trusted model, reasoning, permission, environment, workspace, and
  Agent-home configuration continues to apply through native protocol fields
  or process environment.

## Source References

- Codex protocol truth comes from `codex app-server generate-json-schema` for
  the supported CLI version. The implementation may selectively migrate the
  proven JSON-RPC lifecycle and event handling from Hermes Agent's Codex
  app-server runtime, with provenance recorded and license obligations kept.
- Kimi protocol truth comes from MoonshotAI `kimi-code`'s `kimi acp`
  documentation and ACP server tests. Do not base the new implementation on the
  winding-down legacy `kimi-cli` print mode.
- No external source is copied without recording the exact upstream commit and
  license in the relevant `SOURCE.md`.

## Testing Strategy

All behavior changes follow RED -> GREEN with hermetic fake protocol servers.

Shared JSON-RPC tests cover request correlation, concurrent notification and
reverse-request delivery, oversized/malformed frames, cancellation, EOF, and
single-owner close.

Codex fake app-server tests cover:

- initialize and thread start/resume ordering;
- two turns on one process PID;
- text, native tools, usage, finish, and reasoning filtering;
- auto/deny/ask approval responses and user-input rejection;
- turn interrupt without process exit;
- cancellation escalation;
- process crash and native thread recovery.

Kimi fake ACP tests cover the equivalent initialize, new/resume, two-turn PID,
session-update, permission, cancel, escalation, crash, and recovery cases.

Worker/API integration tests assert:

- all three descriptors are `persistent_process`;
- a normal second turn reuses one Worker and one Agent PID;
- same-session concurrent turns return conflict;
- disconnect aborts the turn but a settled native cancel keeps the session;
- native internal tools never leak as OpenAI tool calls;
- terminal native failure drops the handle and the next turn resumes once;
- shutdown leaves no Worker, Agent, or tool subprocesses.

Release gates remain `go test ./...`, `go test -race ./...`, `go vet ./...`,
native builds for darwin/linux/windows, architecture tests, real Codex smoke on
macOS, and real nsjail smoke on Linux. Real Kimi smoke is required in an
environment with the current Kimi Code CLI installed and authenticated; its
absence on one development host must be reported rather than replaced with a
false live claim.

## Completion Criteria

The migration is complete only when:

1. Claude, Codex, and Kimi all declare and exhibit persistent-process behavior.
2. Two normal turns reuse the same Agent process for each implementation.
3. Normal finish and successful abort do not close the session process.
4. Terminal cancellation and crash paths reap the full process group and can
   recover using the stored native session ID.
5. Resume-per-turn production code, configuration, tests, and documentation are
   removed rather than retained as compatibility paths.
6. OpenAI responses expose only assistant text/usage/finish, with native tools
   kept inside the Agent turn for all three implementations.
