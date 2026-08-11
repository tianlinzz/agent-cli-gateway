# Agent Layer Rearchitecture Design

Date: 2026-08-11

## Status

Approved direction. This document defines the final open-source architecture that replaces the temporary `agent_ref` migration layout. Implementation requires a separate reviewed plan.

## Objective

Refactor the repository so native Agent CLI execution is a reusable, independently testable `agent` layer and `adapters` are thin bridges to the canonical Gateway runtime. Remove all cc-connect-era `core` dependencies, build-tagged legacy source, and duplicate implementations from the final tree without changing the public OpenAI-compatible API or the established worker isolation model.

The supported Agents remain Claude Code, Codex, and Kimi only.

## Non-Goals

- Reintroducing cc-connect IM platforms, Engine, daemon, management UI, cron/timer, provider switching, or legacy HTTP APIs.
- Importing cc-connect as a Go module or vendoring the whole upstream repository.
- Changing caller authentication, workspace ownership, session concurrency, OpenAI response shapes, nsjail policy, or cross-node scheduling.
- Adding new Agents during this refactor.

## Upstream Baseline

- Project: `https://github.com/chenhg5/cc-connect`
- Branch: `main`
- Baseline commit: `3fc360ee6acc9bab13ab1b48ddde3af44062903b`
- License: MIT; preserve upstream attribution in `LICENSES/` or package `SOURCE.md` files.

Only proven CLI launch, native protocol parsing, session/resume, permission, usage, abort, and process-reaping behavior may be selectively migrated. Every migrated behavior must identify its upstream source file and corresponding local regression test.

## Final Package Layout

```text
api/openai/                 OpenAI HTTP/SSE protocol only
runtime/                    Canonical transport-neutral contracts and session metadata
agent/process/              Generic CLI process, pipes, process groups, signals, wait/close
agent/protocol/             Generic bounded JSONL/frame decoding and protocol errors
agent/claudecode/           Claude native launch, stream-json, permissions, usage, resume
agent/codex/                Codex native launch, event protocol, usage, thread resume
agent/kimi/                 Kimi native launch, probing, events, usage, resume
adapters/claudecode/        Claude native types to runtime.AgentAdapter/runtime.Session
adapters/codex/             Codex native types to runtime.AgentAdapter/runtime.Session
adapters/kimi/              Kimi native types to runtime.AgentAdapter/runtime.Session
worker/                     Execution backend, Worker RPC, supervisor, nsjail
workspace/                  Caller/workspace path resolution
config/                     Deployment configuration
cmd/                        Gateway and Worker composition roots
```

## Dependency Rules

Allowed dependencies:

```text
agent/process   -> stdlib
agent/protocol  -> stdlib
agent/<name>    -> agent/process, agent/protocol, stdlib
adapters/<name> -> agent/<name>, runtime, stdlib
worker          -> runtime, workspace, config
api/openai      -> runtime
cmd             -> api, runtime, adapters, worker, config
```

Forbidden dependencies:

```text
runtime -> agent | adapters | worker | api
agent   -> runtime | adapters | worker | api | config
adapter -> worker | api
worker  -> concrete agent or adapter package
```

The native `agent` layer exposes its own options, event, usage, permission, and session types. Runtime conversion belongs exclusively to adapters. This prevents the native layer from becoming another Gateway contract implementation.

## Layer Responsibilities

### Runtime

Retain canonical `AgentAdapter`, `Session`, `Input`, `Event`, `Usage`, `PermissionRequest`, `ExecutionBackend`, and session-store contracts. Runtime must not contain concrete Agent names or native protocol details.

### Native Agent Layer

Each native Agent package owns CLI argument construction, environment shaping, process lifecycle, native input encoding, native output parsing, resumable native IDs, permission mechanics, usage extraction, and turn abort semantics. It must be usable without the OpenAI API, Gateway Worker RPC, or runtime registry.

`agent/process` owns shared process mechanics only. Agent-specific launch flags and abort semantics remain in the concrete package. `agent/protocol` owns bounded framing only; it must not contain Claude/Codex/Kimi event switches.

### Adapter Layer

Each adapter owns registration, descriptor/capability declaration, trusted deployment-config conversion, `runtime.StartRequest` conversion, `runtime.Input` conversion, native-event to `runtime.Event` mapping, and runtime-facing session lifecycle. It must not call `exec.Command`, parse raw JSON frames, or manage process groups.

### Worker Layer

The Worker remains the only process that instantiates a concrete adapter and launches an Agent through it. The Supervisor owns the outer Worker process, isolation, RPC, session concurrency, cleanup, and node-local execution. It must remain agnostic to Agent names and native protocols.

## Behavioral Contracts To Preserve

- `GET /v1/models` discovers configured public Agent/model routes.
- `POST /v1/chat/completions` supports stream and non-stream responses.
- Caller-provided session IDs are created on first use and reused by the same caller.
- Session identity, workspace identity, and caller identity remain separate.
- One active turn per session; different sessions in one workspace may run concurrently.
- Claude uses persistent native execution; Codex and Kimi retain their declared native lifecycle behavior.
- Client disconnect, timeout, and explicit abort always settle the Gateway turn and leave no unowned process.
- Unattended Claude execution disables `AskUserQuestion` at launch and denies it defensively at protocol level.
- macOS test-mode direct Workers preserve host CLI credentials while mapping the controlled workspace.
- Linux production/dev remains fail-closed behind nsjail.

## Migration Strategy

### Phase 1: Characterization

Freeze current public and native behavior with tests before moving source. Add missing characterization tests for launch argv, environment, event ordering, native session persistence, abort, CLI exit, malformed/oversized frames, and stderr redaction. No package movement occurs before this gate passes.

### Phase 2: Shared Foundations

Create `agent/process` from the already-tested process helpers in current adapters and the useful upstream cc-connect process-group code. Create `agent/protocol` only for duplicated framing behavior proven common to at least two Agents. Do not introduce a speculative framework.

### Phase 3: Agent-By-Agent Extraction

Extract in this order:

1. Claude Code, because persistent-process lifecycle, control requests, and abort are the highest-risk path.
2. Codex, preserving per-turn process and native thread resume behavior.
3. Kimi, preserving CLI capability probing and resume behavior.

For each Agent, move native code into `agent/<name>`, convert `adapters/<name>` to a thin bridge, run package tests, integration tests, race tests, and real local smoke where credentials are available. Do not start the next Agent until the current vertical path passes.

### Phase 4: Legacy Removal

After all three adapters depend on the new native packages:

- Delete every `//go:build agent_ref` source and test.
- Delete every import of the removed `core` package.
- Remove `agent_ref` documentation and `.dockerignore` exceptions.
- Rewrite adapter provenance to cite the upstream URL/commit directly.
- Ensure there is exactly one implementation of each native protocol.

No deprecated compatibility package, alias, forwarding wrapper, or hidden legacy tree remains.

## Error And Cancellation Semantics

- Native errors retain Agent context but redact tokens, authorization headers, and configured secrets.
- Process startup errors include executable, safe argv summary, workspace, and stderr without secrets.
- Native protocol failure emits one terminal native error; adapters map it to one runtime terminal error.
- Abort is lifecycle-aware. A native protocol that cannot cancel a turn must terminate its execution; an adapter must never report successful abort while work remains active.
- Every event channel has one documented owner and is closed exactly once.
- Every spawned process is reaped; tests assert no surviving process group after close, timeout, crash, or abort.

## Testing And Release Gates

Required on every extraction phase:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

Additional gates:

- Package-boundary test or static check enforces the dependency rules.
- No source imports `github.com/tianlinzz/agent-cli-gateway/core`.
- No source contains the `agent_ref` build tag.
- No `exec.Command` or raw protocol decoder remains under `adapters/`.
- OpenAI integration tests cover model discovery, caller-provided session reuse, same-workspace session isolation, stream/non-stream, disconnect, timeout, abort, and Worker crash.
- Native fake-CLI tests cover all three Agents without requiring credentials.
- Linux/nsjail smoke remains mandatory before release.

## Open-Source Quality Requirements

- Public package comments explain stable exported APIs; keep most native implementation internal to each package.
- Preserve MIT attribution for migrated upstream code and document material modifications.
- README architecture matches the code tree and contains no deleted command/package references.
- CI runs formatting, tests, race tests, vet, cross-platform builds, dependency-boundary checks, and secret scanning.
- Final repository contains no intentionally uncompilable source.

## Completion Criteria

The rearchitecture is complete only when all three Agent integrations pass through `agent/<name> -> adapters/<name> -> runtime -> worker -> OpenAI API`, all release gates pass, the old `agent_ref` tree and `core` imports are gone, adapters contain no native process/protocol implementation, and real Claude/Codex local smoke plus Linux nsjail smoke demonstrate the same externally observable behavior as before the refactor.
