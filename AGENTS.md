# Agent Gateway Development Guide

## Project Overview

`agent-cli-gateway` is an OpenAI-compatible **Agent Gateway** that exposes coding
agents (Codex, Claude Code, Kimi) behind a single HTTP API. Clients talk
standard OpenAI shapes (`/v1/models`, `/v1/chat/completions`, SSE streaming);
the gateway forks one **nsjail-isolated worker process per session** and the
worker owns the actual agent CLI. Every agent executes inside a sandbox — a
worker crash or a CLI meltdown never takes the gateway down and never leaks
into another session.

This is a rewrite of cc-connect: the IM platform layer (Feishu/Telegram/Discord
and friends) and the management/event APIs are gone, replaced by a clean
OpenAI-compatible surface and a process-isolation-first execution model.

## Architecture

Canonical data path:

```text
OpenAI HTTP/SSE -> runtime.ExecutionBackend -> Worker Supervisor/RPC
-> adapters/<name> -> agent/<name> -> Agent CLI process
```

```
HTTP client (any OpenAI client)
        │  /v1/* + SSE (Bearer token, X-User-Id)
        ▼
cmd/gateway  (API process + worker Supervisor)
  ├── api/openai     OpenAI-compatible HTTP layer (models/chat/abort/health)
  ├── runtime        canonical contract: descriptors, events, session store,
  │                  registry, execution backend
  ├── config         TOML config (config.GatewayConfig)
  └── worker
      ├── supervisor   forks/reaps one nsjail-wrapped worker per session
      ├── rpc          gRPC Worker contract over per-session Unix sockets
      └── nsjail       profile generation + preflight
              │  fork: nsjail -Mo --config profile -- gateway-worker
              ▼
cmd/gateway-worker   (one per session; the ONLY process that runs agent CLIs)
  └── adapters/{codex,claudecode,kimi}   runtime/native mapping only
        └── agent/{codex,claudecode,kimi} native CLI + protocol
```

### Key Design Principles

**`runtime/` is the nucleus.** It defines the canonical contract
(`Descriptor`, `AgentAdapter`, `Session`, `ExecutionBackend`, `Event`,
`SessionRecord`, `SessionStore`, `Registry`) that every layer speaks. The
runtime package is deliberately transport-agnostic: it imports only the
standard library and never references HTTP, gRPC, Unix sockets, platform/IM
types, or any concrete adapter.

**Adapters register themselves by string name.** Adapter packages call
`runtime.Register(name, factory)` from their `init()`; the gateway entrypoints
(`cmd/gateway/plugin_agent_*.go`, `cmd/gateway-worker/plugin_agent_*.go`) import
them and names from config are wired into the process-wide registry
(`runtime.DefaultRegistry()`). Nothing in `runtime/` knows any specific agent.

**Dependency direction:**
```
cmd/gateway → config/, runtime/, api/openai/, worker/
cmd/gateway-worker → adapters/*, runtime/
api/openai → runtime/   (never adapters/, worker/, or config/)
adapters/<name> → agent/<name>, runtime/ (never api/, worker/, or another agent)
agent/<name> → agent/events, agent/process, agent/protocol, stdlib only
worker/     → runtime/, workspace/, config/ (never adapters/ or api/)
workspace/  → stdlib only
runtime/    → stdlib only
```

`agent/events` is the single canonical native event contract (D1): every
`agent/<name>` package re-exports it via type aliases instead of carrying its
own copy.

### Core Interfaces

- **`AgentAdapter`** — canonical adapter contract (`Describe`, `Start`). The
  API layer never imports an adapter directly.
- **`Session`** — a running agent session (`Send`, `Events`, `Abort`, `Close`).
- **`ExecutionBackend` / `ExecutionHandle`** — the API layer's only way to
  execute a model request. Transport details (worker processes, gRPC, Unix
  sockets, nsjail) hide behind this interface; a future cross-node deployment
  replaces only it, never the adapter or API contract.
- **`Registry`** — thread-safe registry of adapter factories keyed by string
  name; deliberately name-agnostic.
- **`SessionStore`** — owner-scoped session metadata store (in-memory for
  phase 1); every read returns a deep copy and operations are scoped to the
  owner so one tenant can never observe another's sessions.
- **`Event`** — canonical runtime events: `text`, `tool_use`, `tool_result`,
  `permission`, `usage`, `error`, `finish`, `status`. This is the only event
  shape that crosses the API boundary.

Native `tool_use` and `tool_result` events are execution telemetry, not OpenAI
model tool requests. The Agent CLI already executes those tools. Never map them
to `message.tool_calls`, `delta.tool_calls`, or `finish_reason: tool_calls` on
the OpenAI surface; doing so creates a second upstream Agent loop and repeats
the same user turn. Expose native tool progress through a separate optional
telemetry protocol if needed.

### Persistent Native Lifecycle

Every Gateway session owns one Worker process and one persistent Agent CLI:

- Claude Code: bidirectional `stream-json`.
- Codex: `codex app-server --listen stdio://` JSON-RPC v2.
- Kimi Code: `kimi acp` JSON-RPC / ACP v1.

Normal turn completion only returns the native session to idle. There is no
per-turn process fallback. `Abort` cancels only the active turn when the native
protocol supports it; cancellation failure or a protocol without a reliable
interrupt may escalate to process teardown. A crashed execution is replaced
on the next request using the saved native session ID. Never automatically
retry a turn with an unknown completion outcome.

## Development Rules

### 1. No Hardcoded Agent Names in Runtime

`runtime/` must stay name-agnostic. Never write `if name == "codex"` in
runtime, and never import `adapters/*` from `api/` or `worker/`. Use the
registry and capability-based checks instead:

```go
// BAD — hardcodes an agent in runtime
if req.ModelID == "codex" && supportsPermission(req) {

// GOOD — capability-based check
if d.Capabilities.Permission {
```

### 2. Prefer Interfaces Over Type Switches

When behavior differs across adapters/backends, define an optional interface in
`runtime/` and let implementations opt in. Query via the interface and fall
back gracefully:

```go
if handle, ok := someBackend.(OptionalCapability); ok {
    handle.OptionalBehavior()
}
```

### 3. Configuration Over Code

- Features that may vary per deployment are configurable in `gateway.toml`
  (`config.GatewayConfig` — server/auth/workspace/isolation/per-agent).
- Add new config fields with sensible defaults so existing configs don't break.
- The `test` runtime mode is the ONLY mode that may disable nsjail isolation;
  prod and dev fail closed without the sandbox.

### 4. High Cohesion, Low Coupling

- Each `agent/X/` package owns native CLI launch, protocol parsing, resume,
  usage, abort, and teardown without importing Gateway layers.
- Each `adapters/X/` package is a thin bridge for trusted options, prompt/event
  conversion, and lifecycle delegation. It must not launch a process, manage a
  process group, decode raw JSONL, or switch on raw Agent-native messages.
- `worker/` owns everything about process isolation: the supervisor, the gRPC
  RPC layer, and `worker/nsjail/` profile generation + preflight.
- `api/openai/` translates OpenAI shapes ↔ runtime contract and holds no
  agent-specific logic.
- Cross-cutting concerns live in their own packages (`api/openai` auth,
  `workspace/` resolution, `runtime/` session store).

### 5. Isolation Is Mandatory

- Production and dev profiles refuse to start sessions without nsjail
  (fail-closed). Only the `test` profile may spawn workers directly.
- Never add a code path that silently falls back to an unsandboxed spawn; on
  non-Linux hosts the jailed path fails closed too.
- The worker layer spawns one jailed `gateway-worker` per session; the API
  process must never launch an agent CLI itself.
- Workspace access is a security boundary: clients submit opaque
  `workspace_id`s; `workspace/` resolves them to directories under a
  configured root, and the directory is bind-mounted into the sandbox. No
  client-controlled absolute path can ever be injected.

### 6. Permission Is Part of the Contract

- `runtime.PermissionRequest` is a canonical event. Even in phase 1 (permission
  auto-approved inside the controlled workspace), the event surface must keep
  the ability to request permission.
- Agent config selects the mode: `auto` / `ask` / `deny`.
- Reverse permission/user-input requests must always receive a deterministic
  response; an unattended OpenAI request must never wait for terminal input.

### 7. Error Handling

- Always wrap errors with context: `fmt.Errorf("worker: spawn: %w", err)`
- Never silently swallow errors; at minimum log them with `slog.Error` /
  `slog.Warn`.
- Use `slog` (structured logging) consistently; never `log.Printf` or
  `fmt.Printf` for runtime logs.
- Redact tokens/secrets in error messages and logs.

### 8. Concurrency Safety

- Sessions, the registry, and the session store are accessed from multiple
  goroutines; protect shared state with `sync.Mutex` or `atomic` types.
- Use `context.Context` for cancellation propagation, especially across the
  API → supervisor → worker(nsjail) → CLI boundary.
- Channels should have clear ownership; document who closes them.
- The concurrency rule is **one active turn per session**. Different sessions
  in the **same workspace** may execute concurrently; the caller owns any
  resulting filesystem coordination policy.

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use `strings.EqualFold` for case-insensitive comparisons
- Keep functions focused; extract helpers when a function exceeds ~80 lines
- Naming: `New()` for constructors, avoid stuttering (`worker.WorkerConfig` →
  `worker.Config`)

## Testing

### Requirements

- All new features must include unit tests.
- **All bug fixes MUST include a regression test in the same PR.** A bug fix
  PR without a test that fails on the pre-fix code and passes on the fixed
  code will not be merged. Name regression tests so the bug is searchable
  later.
- Tests must pass before committing: `go test ./...`.
- Concurrency-sensitive changes must pass `go test -race ./...`.

### Running Tests

```bash
# Full unit + integration test suite
go test ./...

# Race detector (CI)
go test -race ./...

# A specific package
go test ./runtime/ -v

# Container-level integration tests (hermetic stub worker; darwin-runnable)
go test ./integration/ -v
```

### Test Patterns

- `worker/` and `adapters/` unit tests use hermetic stub binaries (the
  `worker/testworker` child, fake-agent scripts) — no real agent CLIs required.
- `integration/agent_gateway_test.go` runs the real HTTP API against the real
  supervisor and a stub worker child end to end: health, model discovery,
  streaming completion, abort, worker-crash recovery, workspace
  out-of-bounds rejection, and nsjail preflight fail-closed.
- Cross-platform: the module builds and vets on darwin, linux, and windows
  (`GOOS=linux go build ./...`, `GOOS=windows go build ./...`). Keep build-tag
  splits (`_unix`/`_windows`, `_linux`/`_other`) minimal and fail-closed on
  non-Linux.
- Real nsjail smoke (version + ldd + minimal jail) is Linux-CI-only via
  `docker/nsjail-smoke.sh`; it cannot run on the darwin dev host.

### Agent Wiring

- Adapters are wired into both binaries via `plugin_agent_*.go` files with a
  `//go:build !no_<agent>` tag (e.g. `!no_codex`).
- The supported native packages are `agent/claudecode`, `agent/codex`, and
  `agent/kimi`. There are no compatibility implementations or duplicate Agent
  trees. Provenance lives in `adapters/<name>/SOURCE.md`.

## Pre-Commit Checklist

1. **Build passes**: `go build ./...`
2. **Cross-platform build passes**:
   `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` and
   `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`
3. **Tests pass**: `go test ./...`
4. **Race tests pass** (for concurrency changes): `go test -race ./...`
5. **Integration tests pass** (for api/runtime/worker changes):
   `go test ./integration/ -v`
6. **Bug fix has a regression test**: a new test in this PR that fails on the
   pre-fix code and passes on the fix.
7. **No hardcoded agent names in runtime**: grep for adapter names in
   `runtime/*.go` and `api/openai/*.go`.
8. **No secrets in code**: no API keys, tokens, or credentials in source files.

## Adding a New Agent

1. Create `agent/newagent/` for native process, protocol, event, and lifecycle
   behavior. It must not import Gateway runtime or transport packages.
2. Create `adapters/newagent/` as the thin `runtime.AgentAdapter` bridge and
   register it with `runtime.Register("newagent", factory)`.
3. Create `cmd/gateway/plugin_agent_newagent.go` AND
   `cmd/gateway-worker/plugin_agent_newagent.go`, both with
   `//go:build !no_newagent`.
4. Add config example in `config.example.toml` under `[agents.newagent]`.
5. Add native fake-CLI tests and adapter mapping tests separately.

## Adding a New Endpoint

1. Add the route in `api/openai/` (e.g. `chat_completions.go` for
   `/v1/chat/completions`); the HTTP handler layer lives there.
2. Speak only the `runtime` contract — never import `adapters/` or `worker/`
   in the API layer.
3. Add an integration case in `integration/agent_gateway_test.go` so the
   API → supervisor → worker path is covered end to end.
4. Document the endpoint in `README.md`.
