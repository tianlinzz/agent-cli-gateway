# Persistent Native Agent Protocols Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Codex and Kimi resume-per-turn execution with persistent native JSON-RPC processes while preserving the existing Worker isolation and OpenAI API boundary.

**Architecture:** Add one Agent-agnostic JSON-RPC stdio client under `agent/protocol`, then build separate Codex app-server and Kimi ACP state machines on it. Keep one native process per Gateway session, make abort cancel only the current turn, and remove the resume-per-turn production mode after all adapters are persistent.

**Tech Stack:** Go 1.24, JSON-RPC 2.0 over JSONL stdio, Codex app-server v2, Agent Client Protocol v1, gRPC Worker transport, hermetic fake CLI subprocesses.

---

### Task 1: Shared Bounded JSON-RPC Client

**Files:**
- Create: `agent/protocol/jsonrpc.go`
- Create: `agent/protocol/jsonrpc_test.go`
- Reuse: `agent/protocol/jsonl.go`

- [ ] **Step 1: Write failing correlation and notification tests**

Define tests that connect the client to pipes, send two concurrent requests,
return responses out of order, and inject a notification and reverse request.
Assert request IDs correlate correctly and caller handlers receive method plus
raw params.

- [ ] **Step 2: Run tests to verify RED**

Run: `go test ./agent/protocol -run TestJSONRPC -count=1 -v`

Expected: FAIL because `NewJSONRPCClient`, `Call`, `Notify`, and handler types do
not exist.

- [ ] **Step 3: Implement the minimal transport**

Implement these public concepts:

```go
type RPCMessage struct {
    JSONRPC string          `json:"jsonrpc"`
    ID      json.RawMessage `json:"id,omitempty"`
    Method  string          `json:"method,omitempty"`
    Params  json.RawMessage `json:"params,omitempty"`
    Result  json.RawMessage `json:"result,omitempty"`
    Error   *RPCError       `json:"error,omitempty"`
}

type ReverseHandler func(context.Context, RPCMessage) (any, *RPCError)
type NotificationHandler func(RPCMessage)

func NewJSONRPCClient(stdin io.WriteCloser, stdout io.Reader, maxFrame int, reverse ReverseHandler, notify NotificationHandler) *JSONRPCClient
func (c *JSONRPCClient) Call(ctx context.Context, method string, params, result any) error
func (c *JSONRPCClient) Notify(ctx context.Context, method string, params any) error
func (c *JSONRPCClient) Close() error
func (c *JSONRPCClient) Done() <-chan struct{}
func (c *JSONRPCClient) Err() error
```

Use the existing JSONL decoder, one write mutex, one pending-request map, and a
single read-loop owner. EOF or malformed frames settle every pending call once.

- [ ] **Step 4: Add cancellation, malformed frame, oversized frame, EOF, and close tests**

Assert canceled calls are removed, late responses are ignored, all pending
calls fail on EOF, reverse responses are serialized, and `Close` is idempotent.

- [ ] **Step 5: Run GREEN and race verification**

Run: `go test -race ./agent/protocol -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent/protocol/jsonrpc.go agent/protocol/jsonrpc_test.go
git commit -m "feat: add bounded native json-rpc transport"
```

### Task 2: Persistent Codex App-Server Native Session

**Files:**
- Rewrite: `agent/codex/launch.go`
- Rewrite: `agent/codex/session.go`
- Rewrite: `agent/codex/protocol.go`
- Modify: `agent/codex/options.go`
- Replace tests: `agent/codex/native_exec_test.go`
- Modify: `adapters/codex/adapter.go`
- Modify: `adapters/codex/adapter_test.go`
- Modify: `adapters/codex/SOURCE.md`

- [ ] **Step 1: Write a fake app-server characterization test**

The helper subprocess must accept `initialize`, `initialized`, `thread/start`
or `thread/resume`, and repeated `turn/start` requests. It records its PID and
emits assistant deltas, command tool events, usage, and `turn/completed`.

Test two `Send` calls and assert one PID, one thread, two turns, two finishes,
and no reasoning in `EventText`.

- [ ] **Step 2: Run Codex tests to verify RED**

Run: `go test ./agent/codex ./adapters/codex -count=1 -v`

Expected: FAIL because current code launches `codex exec` once per turn and the
adapter declares `resume_per_turn`.

- [ ] **Step 3: Implement app-server launch and handshake**

Build only:

```text
codex app-server --listen stdio://
```

Start the process once, create the shared JSON-RPC client, call `initialize`,
send `initialized`, then call `thread/start` or `thread/resume`. Store and emit
the returned thread ID before `Start` returns.

- [ ] **Step 4: Implement turn event projection**

Track the active turn ID. Map v2 agent-message deltas, item start/completion,
usage, turn completion, and turn failure into native events. Deduplicate tool
start/result by stable item ID and emit exactly one terminal event per turn.

- [ ] **Step 5: Implement approvals and bounded user-input rejection**

Respond to command/file approval reverse requests according to
`auto|ask|deny`. `ask` emits `EventPermission` and rejects because the current
OpenAI surface has no response endpoint. Reject `tool/requestUserInput`
deterministically.

- [ ] **Step 6: Implement turn-only abort and escalation tests**

`Abort` sends `turn/interrupt` and waits for the turn completion notification.
If it does not settle within the configured close timeout, kill/reap the process
and close the session. A successful interrupt must leave the same PID ready for
the next `Send`.

- [ ] **Step 7: Remove the exec backend**

Delete `Backend`, `normalizeBackend`, `codex exec` argv construction, rollout
usage lookup tied to exec output, and every resume-per-turn test. Change the
descriptor to `LifecyclePersistentProcess` and description to app-server.

- [ ] **Step 8: Run GREEN and race verification**

Run: `go test -race ./agent/codex ./adapters/codex -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add agent/codex adapters/codex
git commit -m "feat: run codex through persistent app server"
```

### Task 3: Persistent Kimi ACP Native Session

**Files:**
- Rewrite: `agent/kimi/launch.go`
- Rewrite: `agent/kimi/session.go`
- Rewrite: `agent/kimi/protocol.go`
- Modify: `agent/kimi/options.go`
- Remove: `agent/kimi/probe.go`
- Replace tests: `agent/kimi/native_exec_test.go`
- Modify: `adapters/kimi/adapter.go`
- Modify: `adapters/kimi/adapter_test.go`
- Modify: `adapters/kimi/SOURCE.md`

- [ ] **Step 1: Write a fake ACP server characterization test**

The helper accepts ACP v1 `initialize`, `session/new` or `session/resume`, two
`session/prompt` requests, emits `session/update` text/tool notifications, and
returns stop reasons. Assert one PID, one ACP session, and two turns.

- [ ] **Step 2: Run Kimi tests to verify RED**

Run: `go test ./agent/kimi ./adapters/kimi -count=1 -v`

Expected: FAIL because current code invokes `--prompt` once per turn.

- [ ] **Step 3: Implement ACP launch and handshake**

Launch only `kimi acp`, initialize with protocol version 1 and empty client
capabilities, then call `session/new` or `session/resume`. Emit the returned
session ID immediately.

- [ ] **Step 4: Implement prompt/update projection**

Send the newest user text as an ACP text content block. Map
`agent_message_chunk`, stable tool call updates, prompt stop reason, and errors
to native events. Never emit history replay as current-turn text.

- [ ] **Step 5: Implement permission reverse RPC and cancellation**

Respond to `session/request_permission` using the configured mode.
`Abort` sends `session/cancel`, waits for prompt settlement, and escalates only
on timeout. Prove a successful cancel preserves the process for another turn.

- [ ] **Step 6: Remove print-mode code**

Delete flag probing, `--prompt`, `--print`, `--output-format stream-json`,
stderr resume extraction, and resume-per-turn tests. Declare
`LifecyclePersistentProcess`.

- [ ] **Step 7: Run GREEN and race verification**

Run: `go test -race ./agent/kimi ./adapters/kimi -count=1`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add agent/kimi adapters/kimi
git commit -m "feat: run kimi through persistent acp"
```

### Task 4: Make Worker Abort Turn-Scoped

**Files:**
- Modify: `worker/supervisor.go`
- Modify: `worker/supervisor_test.go`
- Modify: `worker/rpc.go`
- Modify: `cmd/gateway-worker/main.go`
- Modify: `cmd/gateway-worker/main_test.go`

- [ ] **Step 1: Write the failing persistent-abort reuse test**

Start a persistent stub Worker, begin a blocking turn, call `Abort`, then send a
second turn through the same Worker handle. Assert the Worker PID is unchanged.

- [ ] **Step 2: Verify RED**

Run: `go test ./worker ./cmd/gateway-worker -run 'Abort|Persistent' -count=1 -v`

Expected: FAIL because Supervisor currently closes persistent Workers on abort.

- [ ] **Step 3: Separate Abort from Close**

Worker RPC Abort delegates only to `runtime.Session.Abort`. Remove lifecycle
branches that call session close or terminate the Worker after a successful
abort. Terminal native sessions still report closed through liveness/event
closure and are reaped by the existing monitor.

- [ ] **Step 4: Run GREEN and race verification**

Run: `go test -race ./worker ./cmd/gateway-worker -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add worker/supervisor.go worker/supervisor_test.go worker/rpc.go cmd/gateway-worker/main.go cmd/gateway-worker/main_test.go
git commit -m "fix: keep persistent workers alive after turn abort"
```

### Task 5: Remove Resume-Per-Turn Contract and Configuration

**Files:**
- Modify: `runtime/contract.go`
- Modify: `api/openai/integration_test.go`
- Modify: `worker/rpc.go`
- Modify: `worker/proto/worker.proto`
- Regenerate: `worker/proto/worker.pb.go`
- Regenerate: `worker/proto/worker_grpc.pb.go` only if the RPC shape changes
- Modify: `config/gateway.go`
- Modify: `config/gateway_test.go`
- Modify: `config.example.toml`
- Modify: `scripts/dev-agents.sh`
- Modify: `scripts/dev_agents_test.go`

- [ ] **Step 1: Add architecture assertions**

Assert active Go/config/docs contain no `LifecycleResumePerTurn`,
`resume_per_turn`, Codex backend selector, or Kimi print-mode probe.

- [ ] **Step 2: Verify RED**

Run: `go test ./internal/archtest -count=1 -v`

Expected: FAIL with the remaining compatibility symbols.

- [ ] **Step 3: Remove the obsolete mode and configuration**

Keep only `LifecyclePersistentProcess`. Preserve native session IDs for crash
recovery; removing resume-per-turn does not remove native resume metadata.
Update fake descriptors and protocol comments.

- [ ] **Step 4: Run targeted verification**

Run: `go test ./runtime ./config ./scripts ./api/openai ./worker ./internal/archtest -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runtime config scripts api/openai worker internal/archtest
git commit -m "refactor: require persistent native agent sessions"
```

### Task 6: Complete OpenAI and Cross-Agent Regression Coverage

**Files:**
- Modify: `api/openai/handlers_test.go`
- Modify: `integration/agent_gateway_test.go`
- Modify: `adapters/codex/adapter_test.go`
- Modify: `adapters/kimi/adapter_test.go`

- [ ] **Step 1: Preserve the existing three-Agent tool-isolation matrix**

Keep the current uncommitted tests that run non-streaming and SSE behavior for
`claude-code`, `codex`, and `kimi`. Keep Codex tool telemetry mapping coverage.

- [ ] **Step 2: Add two-turn persistent integration coverage**

For every adapter descriptor, assert the second turn reuses one execution and
that internal native tools never appear as OpenAI `tool_calls`.

- [ ] **Step 3: Add disconnect/cancel continuity coverage**

Prove a settled turn cancellation returns the Gateway session to idle and lets
the next request use the same live handle. Keep terminal cancellation recovery
as a separate test.

- [ ] **Step 4: Run API/integration race tests**

Run: `go test -race ./api/openai ./integration ./adapters/... -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add api/openai/handlers_test.go integration/agent_gateway_test.go adapters/codex/adapter_test.go adapters/kimi/adapter_test.go
git commit -m "test: verify persistent lifecycle for every agent"
```

### Task 7: Documentation, Provenance, and Release Gates

**Files:**
- Modify: `AGENTS.md`
- Modify: `README.md`
- Modify: `config.example.toml`
- Modify: `adapters/codex/SOURCE.md`
- Modify: `adapters/kimi/SOURCE.md`
- Modify: `internal/archtest/dependencies_test.go`

- [ ] **Step 1: Update architecture and lifecycle documentation**

Document all three persistent protocols, turn-scoped abort, crash recovery,
and lack of resume-per-turn fallback. Remove stale lifecycle tables.

- [ ] **Step 2: Record upstream provenance**

Record Hermes Agent commit `9d6c5a920c773f86fad9ea16528212faeaa21815`
for selectively referenced Codex app-server behavior and MoonshotAI kimi-code
commit `2acf22f66e15361d9804d9014d58ad68a9383caf` for ACP behavior. Verify and
record the applicable licenses before copying any implementation text.

- [ ] **Step 3: Run full release verification**

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
go test ./internal/archtest -count=1 -v
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 4: Run real protocol smoke where available**

Run two turns through the locally installed Codex app-server and confirm one
Agent PID. Run Claude two-turn smoke unchanged. Run Kimi ACP smoke only where a
current authenticated `kimi` binary exists; otherwise report it as unavailable.

- [ ] **Step 5: Commit final documentation**

```bash
git add AGENTS.md README.md config.example.toml adapters/codex/SOURCE.md adapters/kimi/SOURCE.md internal/archtest/dependencies_test.go
git commit -m "docs: publish persistent agent lifecycle contract"
```
