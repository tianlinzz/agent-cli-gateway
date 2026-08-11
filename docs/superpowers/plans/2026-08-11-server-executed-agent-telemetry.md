# Server-Executed Agent Telemetry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Codex, Claude Code, and Kimi project safe reasoning and terminal native-tool summaries through the existing `reasoning_content` channel without emitting client-executable `tool_calls`.

**Architecture:** Native Agent parsers emit safe reasoning plus existing tool lifecycle events. Adapters map them to a name-agnostic runtime contract. The OpenAI streaming layer correlates tool starts/results, serializes one terminal `gateway.tool_execution.v1:` summary as a reasoning string, and keeps standard tool control fields suppressed. f1-web remains a separate follow-up consumer task.

**Tech Stack:** Go, runtime contracts, native protocol fixtures, adapter tests, `api/openai` SSE handlers, integration tests, `go test`, race detector, and cross-platform builds.

---

## File Map

- Modify `runtime/events.go` and `runtime/contract.go` for the canonical reasoning event.
- Modify `agent/codex/*`, `agent/claudecode/*`, and `agent/kimi/*` only where native parsers expose safe reasoning; preserve native tool lifecycle behavior.
- Modify `adapters/codex/session.go`, `adapters/claudecode/session.go`, and `adapters/kimi/session.go` for native-to-runtime reasoning mapping.
- Modify `api/openai/stream.go` and `api/openai/types.go` for per-turn tool correlation and `reasoning_content` projection.
- Modify `api/openai/chat_completions.go` only for defensive replay rejection if the existing session state can prove a tool ID was server-executed.
- Extend native, adapter, API, and integration tests beside their implementations.
- Modify `README.md` to document the existing-field compatibility contract.

## Task 1: Add the canonical runtime reasoning event

**Files:** `runtime/events.go`, `runtime/contract.go`, `runtime/events_test.go`

- [ ] **Step 1: Write a failing contract test.** Construct `runtime.Event{Type: runtime.EventReasoning}` with an ID and text; assert the type is `"reasoning"`, the payload survives construction, and existing `EventToolUse`/`EventToolResult` remain unchanged.
- [ ] **Step 2: Run the focused test.** Run `go test ./runtime -run 'TestReasoningEventContract|TestToolEventContract' -v`; expect a compile failure because the new type is absent.
- [ ] **Step 3: Implement the minimal contract.** Add `EventReasoning`, `Reasoning{ID, Text}`, and `Event.Reasoning *Reasoning`. Keep runtime independent of OpenAI field names and Agent names.
- [ ] **Step 4: Verify.** Run `go test ./runtime`; expect PASS.
- [ ] **Step 5: Commit.** Run `git add runtime/events.go runtime/contract.go runtime/events_test.go && git commit -m "feat: add canonical reasoning runtime event"`.

## Task 2: Map safe reasoning across all three Agents

**Files:** native event/session files under `agent/codex`, `agent/claudecode`, `agent/kimi`; adapter session files; corresponding native and adapter tests

- [ ] **Step 1: Write failing native fixture tests.** Make each hermetic fixture emit one safe reasoning update and one tool start/result pair; assert private reasoning never becomes `EventText`, tool IDs remain stable, and Kimi with no reasoning still succeeds.
- [ ] **Step 2: Run focused tests.** Run `go test ./agent/codex ./agent/claudecode ./agent/kimi ./adapters/codex ./adapters/claudecode ./adapters/kimi -run 'Reasoning|Tool|Event' -v`; expect failures for missing reasoning mappings.
- [ ] **Step 3: Implement native mappings.** Add native reasoning event kinds/payloads only for protocol data explicitly safe for display. Keep hidden reasoning, signatures, and private metadata inside native packages; retain existing tool start/result deduplication.
- [ ] **Step 4: Implement adapter mappings.** Map each native reasoning event to `runtime.Event{Type: runtime.EventReasoning, Reasoning: &runtime.Reasoning{...}}`; do not add Agent-name branches to runtime or API.
- [ ] **Step 5: Verify and commit.** Run the six package tests without `-run`, then commit with `git add agent adapters && git commit -m "feat: map safe reasoning across agent adapters"`.

## Task 3: Project tool terminal summaries through `reasoning_content`

**Files:** `api/openai/types.go`, `api/openai/stream.go`, `api/openai/handlers_test.go`

- [ ] **Step 1: Write failing SSE tests.** Feed a fake execution handle with `EventReasoning`, `EventToolUse(id=tool-1,name=Bash)`, `EventToolResult(id=tool-1,result=/workspace)`, `EventText`, and `EventFinish`. Assert frames contain reasoning text, exactly one reasoning string beginning `gateway.tool_execution.v1:`, final `stop`, and `[DONE]`; assert no `delta.tool_calls`. Add result-only and duplicate-result cases.
- [ ] **Step 2: Run focused tests.** Run `go test ./api/openai -run 'Test.*Telemetry|Test.*Reasoning|Test.*ToolExecution' -v`; expect failures because the projection does not exist.
- [ ] **Step 3: Add the serialization helper.** Define an API-local `ToolExecutionSummary{ID, Name, Status, IsError, Result}` and a helper that emits `gateway.tool_execution.v1:` followed by compact JSON. Bound and redact the result before serialization; omit arguments from v1.
- [ ] **Step 4: Add per-turn correlation.** In `streamTurn`, maintain `started map[string]runtime.ToolCall` and `completed map[string]struct{}`. `EventToolUse` records only; `EventToolResult` merges missing name data, suppresses repeated IDs, serializes one terminal summary as `delta.reasoning_content`, and logs structured warnings for malformed data. `EventReasoning` continues to project plain `reasoning_content`.
- [ ] **Step 5: Preserve existing control behavior.** Do not emit `delta.tool_calls`, tool-role messages, or `finish_reason: tool_calls`; retain existing text, usage, timeout, abort, error, and `[DONE]` paths. Non-streaming responses remain unchanged.
- [ ] **Step 6: Verify and commit.** Run `go test ./api/openai`; commit `api/openai/types.go api/openai/stream.go api/openai/handlers_test.go` with `feat: project native tool summaries through reasoning`.

## Task 4: Add defensive replay handling

**Files:** `api/openai/chat_completions.go`, existing session state only if required, `api/openai/handlers_test.go`

- [ ] **Step 1: Write the replay regression test.** Complete a fake turn containing native tool ID `tool-1`, then submit an assistant `tool_calls` plus a `tool` message referencing `tool-1`; assert a typed 400 invalid-request response and zero additional native turns.
- [ ] **Step 2: Run it to confirm failure.** Run `go test ./api/openai -run TestRejectServerExecutedToolReplay -v`; expect the current normalizer to accept the replay.
- [ ] **Step 3: Implement the smallest guard.** Record completed server-tool IDs in bounded session-owned turn state. During request validation, reject only IDs proven to be server-executed; do not replay the prior user prompt and do not alter unrelated client-owned tool messages.
- [ ] **Step 4: Verify.** Run `go test ./api/openai`; ensure ownership, session resume, and existing normalization tests pass.
- [ ] **Step 5: Commit.** Run `git add api/openai/chat_completions.go api/openai/handlers_test.go runtime && git commit -m "fix: reject replay of server-executed tool results"`.

## Task 5: Cover the full HTTP/SSE path

**Files:** `integration/agent_gateway_test.go`, existing hermetic worker fixture files only when needed

- [ ] **Step 1: Write the failing integration case.** Make the stub worker emit reasoning, tool start, tool result, text, and finish; assert the HTTP stream order is `reasoning_content`, `reasoning_content(gateway.tool_execution.v1:...)`, content, `finish_reason=stop`, `[DONE]`, with exactly one backend turn and no `tool_calls`.
- [ ] **Step 2: Run it and confirm failure.** Run `go test ./integration -run Test.*Telemetry -v`; expect failure until all layers are connected.
- [ ] **Step 3: Extend only the existing stub fixture.** Do not add real Agent CLI dependencies; preserve current worker crash, abort, workspace, and session recovery fixtures.
- [ ] **Step 4: Verify and commit.** Run `go test ./integration -v`, then commit `git add integration worker/testworker && git commit -m "test: cover reasoning-channel agent telemetry"`.

## Task 6: Document and perform full verification

**Files:** `README.md`, no other planned files

- [ ] **Step 1: Document compatibility.** Explain that native tools remain cloud-side, terminal summaries are encoded in existing `reasoning_content` with the `gateway.tool_execution.v1:` prefix, clients must not submit tool results for them, and non-streaming responses omit process telemetry.
- [ ] **Step 2: Run documentation consistency checks.** Run `rg -n 'gateway\.tool_execution\.v1|reasoning_content|tool_calls|stream=true' README.md docs/superpowers/specs/2026-08-11-server-executed-agent-telemetry-design.md`; ensure field names and ownership language match.
- [ ] **Step 3: Commit documentation.** Run `git add README.md && git commit -m "docs: explain reasoning-channel agent telemetry"`.
- [ ] **Step 4: Run required verification.** Run `go test ./...`, `go test -race ./...`, `go test ./integration/ -v`, `go build ./...`, `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`, and `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`; expect all commands to exit 0.
- [ ] **Step 5: Run the negative contract audit.** Run `! rg -n 'delta\.tool_calls|finish_reason.*tool_calls' api/openai` and `git diff --check`; expect no native-tool projection to client-executable tool calls and no whitespace errors.

