# Agent Gateway Optimization Roadmap

> **For agentic workers:** This is the master roadmap, not a single-session
> implementation checklist. Before implementing a phase, create a focused plan
> under `docs/superpowers/plans/` and use
> `superpowers:subagent-driven-development` or `superpowers:executing-plans` to
> execute that phase task by task.

**Goal:** Turn the current single-node Agent Gateway from an architecture-complete
phase-1 implementation into a release-trustworthy, resource-bounded,
observable, and persistable production service without weakening its autonomous
Agent semantics or process-isolation boundary.

**Architecture:** Preserve the canonical path `OpenAI API -> runtime contract ->
ExecutionBackend -> Worker RPC/Supervisor -> adapter -> native Agent`. Fix
confirmed correctness and security gaps first, then make deployment claims
provable, add run-level governance and resource controls, remove configuration
indirection, and only after those foundations introduce durable state and remote
execution.

**Tech Stack:** Go 1.25, `net/http`, gRPC/protobuf, TOML, nsjail/Kafel, Docker,
GitHub Actions, structured logging, OpenTelemetry-compatible metrics/tracing.

**Status:** Proposed master roadmap, based on a full repository review on
2026-08-13. No implementation changes are included in this document.

---

## 1. Product Contract To Preserve

The Gateway exposes autonomous coding Agents through an OpenAI-compatible HTTP
surface. A completion request starts one complete native Agent turn. Codex,
Claude Code, and Kimi execute their own native tools inside the Worker; those
tools are execution telemetry, not OpenAI client-executed tool requests.

The following invariants are non-negotiable throughout this roadmap:

1. Native `tool_use` and `tool_result` events never become OpenAI
   `tool_calls`, tool-role requests, or `finish_reason: "tool_calls"`.
2. The API process never launches an Agent CLI directly.
3. One Gateway session owns at most one live Worker and one persistent native
   Agent process; one turn is active per session.
4. Different sessions in the same workspace may run concurrently. The caller
   owns filesystem coordination policy.
5. Production and development fail closed without nsjail. Only explicit test
   mode may use direct Worker spawn.
6. Clients submit opaque `workspace_id` values, never host paths.
7. Runtime and API code remain Agent-name-agnostic. Behavior differences use
   descriptors or optional capabilities, not type/name switches.
8. An Agent turn with an unknown completion outcome is never automatically
   replayed.
9. Every bug fix includes a regression test that fails on pre-fix behavior.

## 2. Scope And Non-Goals

### In scope

- Close confirmed session and execution-handle resource leaks.
- Prevent client-controlled native-session resume targets.
- Make model discovery reflect actual deployable Agent availability.
- Prove the documented nsjail deployment profile under production-like
  privileges and supported CPU architectures.
- Add HTTP hardening, run identity, resource quotas, metrics, tracing, and
  audit-safe structured logs.
- Replace environment-variable configuration indirection with typed Worker RPC
  configuration.
- Define and implement durable session/run state before remote execution.
- Remove small dead code and low-risk duplication after behavioral boundaries
  are protected by tests.

### Not in scope for early phases

- Mapping native tools into client-executed OpenAI tools.
- Adding legacy cc-connect session, event, IM, or management APIs.
- Adding `/v1/responses` solely to increase the endpoint count.
- Automatic retry of an interrupted or unknown Agent turn.
- Cross-node scheduling before session/run persistence and placement semantics
  are defined.
- Automatic Git conflict resolution between sessions sharing a workspace.
- A management UI, billing system, or general user directory.

## 3. Confirmed Findings And Decision Register

| ID | Finding | Classification | Current evidence | Planned phase |
|---|---|---|---|---|
| F-01 | `SessionStore.Touch`, `Prune`, and `ExpiresAt` are implemented but not wired; successful session records have no expiry and accumulate for process lifetime. | Confirmed resource leak | `runtime/session_store.go`, `api/openai/chat_completions.go`, `cmd/gateway/main.go` | Phase 0 |
| F-02 | Supervisor idle reaping closes Workers but does not proactively remove closed handles from `api/openai.Handler.handles`; cleanup currently waits for a later request. | Confirmed resource retention | `worker/supervisor.go`, `api/openai/types.go`, `api/openai/chat_completions.go` | Phase 0 |
| F-03 | Adapter-specific metadata keys (`codex_thread_id`, `claude_session_id`, `kimi_session_id`) take precedence over the server-owned `native_session_id`. | Confirmed trust-boundary bug | `adapters/*/adapter.go`, `api/openai/chat_completions.go` | Phase 0 |
| F-04 | The runtime image does not install Agent CLIs, while every adapter `Describe` call succeeds without checking its command. `/v1/models` can advertise unusable Agents. | Confirmed deployment/discovery mismatch | `Dockerfile`, `adapters/*/adapter.go`, `api/openai/models.go` | Phase 1 |
| F-05 | Docker/nsjail CI smoke runs with `--privileged`, but documentation claims non-root, `cap-drop=ALL`, no privilege escalation operation. | Unproven security claim | `.github/workflows/ci.yml`, `Dockerfile`, `docker/nsjail-smoke.sh` | Phase 1 |
| F-06 | Kafel policy is declared as `POLICY x86_64`, while CI only proves that Go cross-builds for Linux arm64. | Confirmed architecture mismatch | `worker/nsjail/profile.go`, `.github/workflows/ci.yml` | Phase 1 |
| F-07 | `http.Server` lacks production connection/header timeout and header-size settings. | Confirmed hardening gap | `cmd/gateway/main.go` | Phase 1 |
| F-08 | Agent execution config crosses RPC as typed data, is converted to `CC_GATEWAY_*` process environment, then parsed back by adapters. | Confirmed maintainability issue | `worker/rpc.go`, `cmd/gateway-worker/main.go`, `adapters/*/adapter.go` | Phase 3 |
| F-09 | Configured per-Agent turn timeout is only translated for Kimi; the other adapters silently ignore the same config field. | Confirmed semantic inconsistency | `cmd/gateway-worker/main.go`, `agent/*/options.go` | Phase 0/3 |
| F-10 | There is no caller/global session admission policy, workspace quota, log cap, or overload response contract. | Production governance gap | `config/gateway.go`, `worker/supervisor.go`, `api/openai` | Phase 2 |
| F-11 | The service has structured logs but no stable run ID, metrics surface, distributed trace propagation, or explicit unknown-outcome counter. | Production observability gap | `api/openai`, `worker`, `cmd/gateway` | Phase 2 |
| F-12 | Gateway session/native-session mapping is memory-only, so restart loses ownership and resume metadata; multiple API replicas cannot coordinate. | Declared phase-1 limitation | `runtime/session_store.go`, `cmd/gateway/main.go` | Phase 4 |
| F-13 | The three adapter session bridges repeat prompt selection and event/tool mapping logic. | Maintainability issue | `adapters/*/session.go` | Phase 3 |
| F-14 | Adapter commands are tokenized with `strings.Fields`, so quoted arguments and paths containing spaces are not representable. | Confirmed configuration limitation | `adapters/*/adapter.go` | Phase 3 |
| F-15 | `constantTimeEqual` is unused; model validation repeatedly resolves/describes adapters. | Low-risk cleanup/performance | `api/openai/types.go`, `api/openai/models.go` | Phase 3 |

### Required decisions

The roadmap adopts these decisions so implementation phases do not reinterpret
the same problem differently:

- Worker idle timeout and session-record retention are separate controls.
  Reclaiming an expensive process must not immediately destroy resumable
  conversation identity.
- Native session IDs are server-owned state. Public metadata cannot select an
  arbitrary native thread/session.
- `Describe` remains static. Runtime availability is exposed through a separate
  optional readiness capability and cached catalog state.
- A `Run` identifies one autonomous Agent turn. A `Session` identifies the
  longer-lived owner/model/workspace/native-conversation relationship.
- Agent command configuration becomes an argv array. No shell parsing is
  performed by the Gateway.
- A timeout must have one documented owner and meaning. Phase 0 rejects or
  documents unsupported per-Agent timeout values; Phase 3 introduces a typed,
  consistent turn deadline contract.
- Remote execution is blocked until durable session/run state and placement
  semantics exist.

## 4. Delivery Overview

```text
Phase 0  Correctness and trust-boundary closure
   |
Phase 1  Release and sandbox claims become provable
   |
Phase 2  Run identity, observability, admission, and quotas
   |
Phase 3  Typed configuration and maintainability cleanup
   |
Phase 4  Durable session/run governance and idempotency
   |
Phase 5  Remote execution and horizontal scale
```

Each phase must be independently releasable. A later phase may refine an
interface introduced earlier, but it must not be required to make an earlier
security or correctness fix safe.

## 5. Phase 0: Correctness And Trust-Boundary Closure

**Objective:** Fix current leaks and unsafe resume/config behavior without
changing the external autonomous-Agent contract.

### 0.1 Define separate retention controls

Add explicit configuration instead of overloading `sessions.idle_timeout`:

```toml
[sessions]
idle_timeout = "2h"          # disposable Worker/CLI process
reap_interval = "1m"
record_ttl = "168h"          # resumable SessionRecord identity
record_prune_interval = "5m"
```

Rules:

- `idle_timeout` closes only the live execution.
- `record_ttl` starts/extends after successful activity and may be zero to keep
  records for process lifetime.
- Active turns and closing sessions are never pruned.
- Record expiry closes any remaining execution, removes API telemetry ledgers,
  and then deletes metadata.
- Expiry is owner-independent internal maintenance; public APIs still use
  owner-scoped operations.

**Primary files:**

- `config/gateway.go`
- `config/gateway_test.go`
- `runtime/session_store.go`
- `runtime/session_store_test.go`
- `api/openai/types.go`
- `api/openai/chat_completions.go`
- `api/openai/handlers_test.go`
- `cmd/gateway/main.go`

**Required tests:**

- Successful turns extend record expiry.
- Active turns cannot be pruned.
- Expired records are physically removed.
- Wrong-owner probes remain indistinguishable from unknown sessions.
- Reaper shutdown does not leak a goroutine.
- Race test covers `BeginTurn`, `Touch`, and `Prune` interleavings.

### 0.2 Proactively remove terminal execution handles

Make terminal notification a canonical execution-lifecycle property instead of
relying on the next request to discover a dead handle. The preferred contract
is:

```go
type ExecutionHandle interface {
	Send(context.Context, Input) error
	Events() <-chan Event
	Abort(context.Context) error
	Close(context.Context) error
	Done() <-chan struct{}
}
```

After registering a handle, the Handler watches `Done()` and conditionally
removes that exact handle. Conditional removal is required so a stale watcher
cannot delete a replacement execution for the same session. The watcher also
clears bounded per-execution state that is no longer needed; session-level
native-tool replay state remains until session expiry/deletion.

**Primary files:**

- `runtime/contract.go`
- `api/openai/types.go`
- `api/openai/handlers_test.go`
- `worker/supervisor.go`
- test/fake execution handles across `api/openai` and `integration`

**Required tests:**

- Idle-reaped Worker disappears from the API handle map without another HTTP
  request.
- Crash-triggered terminal notification removes the handle.
- A late terminal signal from an old handle cannot remove a replacement.
- Handler/supervisor shutdown completes with no watcher leak under `-race`.

### 0.3 Make native resume identity server-owned

Remove adapter-specific public metadata precedence. Adapters resume only from
the canonical server-supplied `native_session_id`. If backward-compatible
native key aliases are still needed internally, normalize them before the
public API boundary and ensure the server value always wins.

Also strip reserved keys from public metadata:

```text
native_session_id
codex_thread_id
claude_session_id
kimi_session_id
gateway_session_id
caller_id
```

Unknown ordinary metadata remains pass-through string metadata.

**Primary files:**

- `api/openai/chat_completions.go`
- `adapters/codex/adapter.go`
- `adapters/claudecode/adapter.go`
- `adapters/kimi/adapter.go`
- corresponding adapter and API tests

**Required tests:**

- Client metadata cannot override a stored native session ID.
- A new session cannot inject any native resume ID.
- Fresh and recovered sessions still resume with the ID emitted by their own
  Worker.
- Wrong-owner behavior remains a generic 404.

### 0.4 Stop silently ignoring timeout configuration

Before a unified deadline contract is introduced in Phase 3, configuration
validation must not imply that all adapters honor the same field.

Choose one narrow transitional behavior in the phase implementation plan:

- Preferred: treat `agents.<id>.timeout` as a canonical turn timeout enforced
  by the Worker session boundary for every adapter.
- Acceptable temporary alternative: rename the Kimi-only option and reject the
  generic field for Codex/Claude Code.

Silent acceptance is not allowed.

### Phase 0 exit criteria

- All F-01, F-02, F-03, and the silent portion of F-09 have regression tests.
- `go test ./...` and `go test -race ./...` pass.
- A stress test creates, idles, and expires thousands of sessions without
  monotonic growth in store records, live handles, goroutines, or Workers.
- Public request/response shapes remain compatible.

## 6. Phase 1: Release And Sandbox Truth

**Objective:** Ensure every advertised Agent and security/deployment claim is
verified in the same environment in which it will run.

### 1.1 Separate static description from Agent availability

Introduce an optional runtime capability:

```go
type AvailabilityProbe interface {
	CheckAvailability(context.Context) error
}
```

The Worker-side implementation validates the configured argv executable and
any cheap native protocol/version preconditions. Do not launch a persistent
Agent merely to serve `/v1/models`.

The Gateway maintains a short-lived, concurrency-safe availability cache with
state such as `ready`, `unavailable`, and `unknown`. `/v1/models` advertises
only enabled and available routes. `/health/ready` reports a bounded summary
without exposing secrets or host paths.

Decide and document the image contract:

- Base image: contains Gateway + Worker + nsjail only and ships with all Agents
  disabled; or
- Product image: installs pinned CLI versions for all enabled Agents.

The repository must not advertise an enabled CLI that the shipped image does
not contain.

### 1.2 Validate the documented non-root sandbox profile

Add Linux CI scenarios that run the actual Gateway -> Supervisor -> nsjail ->
stub Worker path with the documented runtime restrictions:

- `--user 65532:65532`
- `--cap-drop=ALL`
- `--security-opt=no-new-privileges`
- read-only root filesystem where feasible
- writable mounts only for workspace and runtime directories
- no `--privileged` in the production-equivalent gate

The existing privileged smoke may remain as a diagnostic compatibility job,
but it cannot be the release gate proving the production security claim.

Validate:

- Worker socket creation and handshake.
- Workspace read/write and out-of-bounds denial.
- `/agent-home` and `/tmp` isolation.
- Network access matches the documented shared-network policy.
- SIGTERM/SIGKILL process-group cleanup leaves no Worker or stub Agent.
- Readiness is 503 when any required namespace/profile step fails.

If the target host/container runtime cannot support unprivileged user
namespaces with those restrictions, revise the deployment contract explicitly;
do not silently add privileges.

### 1.3 Make seccomp architecture-aware

Generate/select Kafel policy by supported host architecture. At minimum:

- `amd64` -> `POLICY x86_64`
- `arm64` -> validated AArch64 policy syntax
- unsupported architecture -> fail closed with a clear preflight error

Run a real minimal jail and stub Worker test on every architecture claimed as
production-supported. A cross-build alone is not sufficient.

### 1.4 Harden the HTTP listener

Add configurable, safe defaults for:

- `ReadHeaderTimeout`
- request-body read timeout or equivalent bounded body read
- `IdleTimeout`
- `MaxHeaderBytes`
- maximum message count and per-message content size
- maximum metadata keys/key length/value length

Do not apply a short global `WriteTimeout` that kills valid long-lived SSE
turns. Streaming turn duration remains governed by the run/turn deadline and
client cancellation.

Use `http.MaxBytesReader` so oversized bodies produce an explicit 413 and do
not accept a valid JSON prefix followed by ignored trailing bytes. Reject
multiple top-level JSON values.

### Phase 1 exit criteria

- `/v1/models` does not advertise a missing or unusable Agent command.
- The release image contract names exactly which CLIs and versions it contains.
- A production-equivalent, non-privileged Linux container test passes.
- Every claimed production CPU architecture runs a real nsjail stub-worker
  smoke, not only a Go build.
- Slow-header, oversized-body, oversized-metadata, and malformed trailing JSON
  tests pass.
- Documentation and CI use the same security context.

## 7. Phase 2: Run Governance, Observability, And Resource Limits

**Objective:** Make autonomous turns identifiable, diagnosable, bounded, and
safe under multi-caller load before adding durable or distributed execution.

### 2.1 Introduce a first-class Run identity

Define a `RunRecord` for one Agent turn:

```go
type RunStatus string

const (
	RunStarting       RunStatus = "starting"
	RunRunning        RunStatus = "running"
	RunSucceeded      RunStatus = "succeeded"
	RunFailed         RunStatus = "failed"
	RunCancelled      RunStatus = "cancelled"
	RunTimedOut       RunStatus = "timed_out"
	RunOutcomeUnknown RunStatus = "outcome_unknown"
)

type RunRecord struct {
	ID          string
	SessionID   string
	CallerID    string
	WorkspaceID string
	ModelID     string
	Status      RunStatus
	StartedAt   time.Time
	FinishedAt  time.Time
	ErrorCode   string
	Usage       Usage
}
```

Return `X-Gateway-Run-Id` on every completion response, including streaming
responses before the first SSE frame. Do not expose internal Worker PIDs as
public identity.

The status model must distinguish:

- confirmed native failure;
- client cancellation with confirmed interrupt;
- timeout with confirmed interrupt;
- connection loss or Worker crash where completion outcome is unknown.

### 2.2 Add correlated structured logs, metrics, and traces

Every request/run log should include safe identifiers:

```text
request_id, run_id, session_id, caller_id, workspace_key,
adapter_id, provider_model, worker_pid, event, duration_ms, error_code
```

Use a hashed or otherwise non-sensitive workspace key in logs. Never log
Bearer tokens, provider secrets, complete prompts, or unsanitized tool output.

Minimum metrics:

```text
gateway_http_requests_total
gateway_http_request_duration_seconds
gateway_runs_active
gateway_runs_total{status,adapter}
gateway_run_duration_seconds{adapter,status}
gateway_unknown_outcomes_total{adapter}
gateway_workers_active{adapter}
gateway_worker_spawns_total{adapter,status}
gateway_worker_crashes_total{adapter}
gateway_worker_heartbeat_failures_total{adapter}
gateway_worker_reclaims_total{reason}
gateway_agent_tokens_total{adapter,direction}
gateway_admission_rejections_total{scope,reason}
```

Keep metric labels bounded. Never use session ID, run ID, caller ID, workspace
ID, or error text as metric labels.

Trace context should propagate HTTP -> backend -> Worker RPC. Native protocol
messages may be represented as bounded spans/events without recording prompt
or tool-result bodies.

### 2.3 Add admission control and explicit overload behavior

Add configuration for:

```toml
[limits]
max_workers = 100
max_active_runs = 100
max_sessions_per_caller = 50
max_active_runs_per_caller = 10
max_active_runs_per_workspace = 4
max_request_messages = 200
max_message_bytes = 262144
max_metadata_bytes = 32768
max_worker_log_bytes = 67108864
```

Rules:

- Limits are admission controls, not unbounded in-memory queues.
- Reject excess work with a stable OpenAI-shaped `429` error and optional
  `Retry-After`.
- Readiness returns 503 only for service incapacity, not normal caller quota
  exhaustion.
- Slot acquisition and release cover start failure, Worker crash, cancellation,
  normal completion, idle reclaim, and shutdown exactly once.
- Per-Agent `max_concurrency` remains an adapter capacity limit and composes
  with global/caller/workspace limits.

### 2.4 Bound filesystem and log consumption

- Implement rotating or size-capped per-Worker logs.
- Emit a clear truncation marker when the cap is reached.
- Add workspace usage measurement and a configurable admission threshold.
- Prefer infrastructure-enforced filesystem quota for hard isolation; the
  Gateway's measurement is an early rejection/visibility layer, not a defense
  against a process racing past a soft quota.
- Document cleanup ownership for session runtime directories, Agent homes,
  logs, and workspace data separately.

### Phase 2 exit criteria

- Every completion has stable request, session, and run correlation.
- All terminal paths map to one explicit Run status, including unknown outcome.
- Metrics expose Worker/run health without unbounded labels.
- Load tests prove configured limits and stable 429 behavior.
- Repeated spawn/crash/cancel cycles do not leak slots, goroutines, logs, or
  runtime directories.

## 8. Phase 3: Typed Configuration And Maintainability

**Objective:** Remove accidental environment/name coupling and consolidate only
the duplication whose semantics are truly shared.

### 3.1 Pass typed adapter configuration directly

Replace this path:

```text
TOML -> AgentExecutionConfig -> protobuf -> os.Setenv -> adapter env parser
```

with:

```text
TOML -> validated AgentExecutionConfig -> protobuf -> adapter factory options
```

Extend the registry factory contract so the Worker can resolve an adapter with
trusted typed configuration. Keep provider credentials in the Worker/container
environment or a future secret provider; do not serialize raw secrets into
logs or session records.

The runtime config remains Agent-name-agnostic. Agent-specific optional fields
belong in a validated typed extension or adapter-owned config decoder at the
Worker composition boundary, not in `runtime` switches.

### 3.2 Define consistent timeout ownership

Use three distinct deadlines:

- HTTP/request cancellation: client transport lifetime.
- Gateway run deadline: maximum autonomous turn duration, consistent for every
  adapter.
- Native operation deadlines: handshake, interrupt, and close protocol bounds.

Do not reuse a close timeout as a turn timeout. Document which event/status is
emitted when each deadline fires and whether the execution remains resumable.

### 3.3 Represent commands as argv

Change Agent command configuration from a shell-like string to an argv array:

```toml
command = ["/opt/agent tools/codex", "app-server"]
```

Do not invoke a shell. For migration, accept the old string form for one
release only if strict TOML decoding can distinguish it, emit a deprecation
warning, and interpret it as one executable path rather than shell syntax.

### 3.4 Extract shared adapter mapping helpers carefully

Only extract behavior that is contractually identical across all three
adapters:

- last non-empty user-message selection;
- optional system-message injection composition;
- canonical bounded tool summary formatting helpers;
- simple canonical event constructors.

Keep native event decoding, native permission behavior, usage semantics,
interrupt behavior, and resume extraction inside each `agent/<name>` or
adapter package. Place shared helpers under an internal package such as
`adapters/internal/bridge` so they cannot become another public runtime layer.

Before extraction, add table-driven parity tests that feed equivalent native
fixtures through each adapter and assert the intended canonical differences as
well as shared behavior.

### 3.5 Clean low-risk dead and repeated work

- Remove unused `constantTimeEqual`.
- Cache static descriptors and availability results with explicit invalidation
  rather than resolving/describing on every chat request.
- Keep constant-time token verification. Do not replace it with a plain map
  lookup solely as a micro-optimization; authentication correctness and token
  rotation matter more than a tiny configured token set.
- Audit stale cc-connect documentation such as `CONTRIBUTING.md` and
  `CHANGELOG.md`; archive or rewrite it so active project documentation does
  not advertise removed IM-era behavior.

### Phase 3 exit criteria

- No `CC_GATEWAY_<AGENT>_*` environment round-trip remains for ordinary typed
  configuration.
- All enabled adapters honor one documented run deadline contract.
- Quoted/space-containing executable paths work without shell evaluation.
- Shared adapter code decreases without moving native protocol logic into a
  generic package.
- Architecture tests continue to enforce the original dependency direction.

## 9. Phase 4: Durable Sessions, Runs, And Idempotency

**Objective:** Preserve ownership, resume identity, and run outcomes across
Gateway restart, and establish the state model required for safe horizontal
scaling.

### 4.1 Define persistence contracts before choosing deployment topology

Extend persistence behind interfaces rather than importing a database into the
API layer:

```go
type SessionStore interface { /* owner-scoped session operations */ }
type RunStore interface { /* run create, transition, query, prune */ }
```

Persist at minimum:

- Gateway session ID, caller ID, workspace ID, public model route;
- native session ID and its adapter;
- created/updated/expiry timestamps;
- last known execution placement/generation;
- run ID, request fingerprint, status, usage, timestamps, error code;
- whether completion outcome is known.

State transitions use compare-and-swap/version checks so two Gateway replicas
cannot both acquire one session turn.

### 4.2 Add request idempotency without automatic replay

Support `Idempotency-Key` scoped by caller and endpoint. Store a canonical
request fingerprint excluding transport-only fields.

Behavior:

- Same key + same fingerprint + completed run: return stored terminal result or
  stable status reference.
- Same key + same fingerprint + active run: return conflict/in-progress status;
  do not start another turn.
- Same key + different fingerprint: return 409 idempotency conflict.
- Unknown outcome: return an explicit unknown status; never rerun implicitly.
- Keys expire under a documented retention policy.

Do not claim exact-once Agent side effects. The guarantee is duplicate
admission prevention and outcome lookup, not transactional filesystem changes.

### 4.3 Recover safely after restart

On Gateway startup:

- reconcile runs left in `starting` or `running`;
- mark non-recoverable turns `outcome_unknown`;
- preserve session/native ID for a later explicit next turn;
- do not replay the abandoned prompt;
- clean stale local runtime/socket directories only after checking ownership
  and process liveness.

### 4.4 Select and implement one durable backend

Choose PostgreSQL when durable audit/querying and transactional state
transitions are primary. Choose Redis only if deployment already treats it as
durable and its persistence/failover guarantees are acceptable. Do not
introduce both in this phase.

Keep the in-memory implementation for tests and explicit development mode.

### Phase 4 exit criteria

- Restart preserves session ownership and native resume identity.
- An interrupted in-flight run becomes `outcome_unknown`, never auto-replayed.
- Two API replicas cannot concurrently acquire one session turn.
- Idempotency tests cover duplicate active, duplicate completed, mismatched
  payload, expiry, and unknown outcome.
- Store migration, backup, and rollback procedures are documented and tested.

## 10. Phase 5: Remote Execution And Horizontal Scale

**Objective:** Replace local process placement with a remote-capable backend
without changing the OpenAI or adapter contracts.

This phase starts only after Phase 4 exits successfully.

### 5.1 Define Worker registration and compatibility

Workers advertise:

- node/worker ID;
- supported adapters and native CLI versions;
- runtime/protobuf protocol version;
- architecture and sandbox capabilities;
- capacity and current load;
- heartbeat/lease expiry.

Gateway scheduling rejects incompatible Worker versions before starting a run.

### 5.2 Add placement and session affinity

- New sessions are placed on a compatible healthy Worker node.
- Active persistent native processes retain node affinity.
- Idle-reaped sessions may resume on another node using durable native session
  identity and a workspace accessible there.
- Lease/CAS ownership prevents two nodes from running one session turn.
- Node loss marks the active run outcome unknown unless the native protocol and
  Worker provide definitive completion evidence.

### 5.3 Define workspace strategy explicitly

Select one supported strategy rather than assuming a host directory is shared:

- shared persistent filesystem;
- per-run checkout/materialization from Git/object storage;
- caller-managed external workspace volume.

Document consistency, locking, cleanup, credential, and performance behavior.
The opaque public `workspace_id` contract remains unchanged.

### Phase 5 exit criteria

- The same API and runtime adapter contracts work with local and remote
  `ExecutionBackend` implementations.
- Node drain, node crash, lease expiry, and incompatible-version tests pass.
- No unknown turn is automatically replayed after failover.
- Workspace behavior is proven for the selected storage strategy.

## 11. Cross-Phase Test And Release Gates

Every phase implementation plan must include these gates where applicable:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
go test ./integration/ -v
git diff --check
```

Additional rules:

- Changes to API/runtime/worker require an integration regression.
- Concurrency changes require race tests and repeated stress execution.
- Sandbox/profile changes require real Linux jail execution.
- Security fixes include negative tests proving the rejected path.
- Config changes preserve defaults where safe, use strict TOML decoding, and
  include migration/rejection tests for old keys.
- Generated protobuf files are updated only through `make generate` and checked
  for a clean regeneration diff.
- Logs and error responses are scanned for secrets and host paths.
- Each phase updates README/config examples and active architecture docs in the
  same release.

## 12. Recommended Implementation Plan Split

Do not execute this roadmap as one giant branch. Create focused plans and
reviewable changes in this order:

1. `session-record-and-handle-reclamation`
2. `server-owned-native-resume-id`
3. `uniform-turn-timeout-contract`
4. `agent-availability-and-model-discovery`
5. `production-nsjail-security-matrix`
6. `http-input-hardening`
7. `run-identity-and-observability`
8. `admission-control-and-resource-limits`
9. `typed-agent-configuration`
10. `adapter-bridge-deduplication-and-command-argv`
11. `durable-session-run-store-and-idempotency`
12. `remote-execution-backend`

Items 1-3 form Phase 0 and should land before broader refactoring. Items 4-6
form the release-trust gate. Items 7-8 should land before external multi-caller
traffic. Items 9-10 are controlled maintainability work. Items 11-12 require
separate design approval because they introduce infrastructure and deployment
choices.

## 13. Definition Of Production-Ready For This Project

The project may be described as production-ready for its documented single-node
scope only when all of the following are true:

- Every advertised Agent CLI is present, versioned, authenticated to its
  provider, and passes a native protocol smoke.
- The documented non-root nsjail security context passes a full
  Gateway-to-stub-Agent integration test on every supported production
  architecture.
- Session records, handles, Workers, goroutines, logs, and runtime directories
  have bounded, tested lifecycles.
- Native resume identity cannot be selected by untrusted request metadata.
- HTTP input and connection resources are bounded without breaking valid SSE.
- Runs have stable identity, explicit terminal/unknown status, safe logs, and
  operational metrics.
- Caller/global/workspace admission controls fail predictably under overload.
- Restore/restart semantics are documented honestly: before Phase 4 the service
  is explicitly single-node and restart loses resumability; after Phase 4 the
  durable guarantees are proven by integration tests.

Until then, documentation should call the repository a single-node phase-1
Agent Gateway with production-oriented isolation, not an already complete
multi-tenant distributed platform.
