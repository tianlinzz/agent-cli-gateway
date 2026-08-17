# agent-cli-gateway

An OpenAI-compatible gateway that exposes coding agents (Codex, Claude Code,
Kimi) behind a single HTTP API. Clients talk standard OpenAI shapes
(`/v1/models`, `/v1/chat/completions`, SSE streaming); the gateway forks one
**nsjail-isolated worker process per session**, and the worker owns the actual
agent CLI. Every agent executes inside a sandbox — a worker crash or a CLI
meltdown never takes the gateway down and never leaks into another session.

This is a rewrite of cc-connect: the IM platform layer (Feishu/Telegram/Discord
and friends) and the management/event APIs are gone, replaced by a clean
OpenAI-compatible surface and a process-isolation-first execution model.

## Features

- **OpenAI-compatible API** — drop-in for existing OpenAI clients.
- **Per-session process isolation** — one `nsjail`-wrapped worker child per
  session, forked and reaped by a supervisor in the gateway process.
- **Three focused agents** — `codex`, `claude-code`, and `kimi`; native CLI
  execution lives in `agent/` and thin runtime mapping lives in `adapters/`.
- **Controlled workspaces** — clients reference workspaces by opaque
  `workspace_id`; the server resolves them to directories under a configured
  root. No client-controlled path can ever be injected (resolver + nsjail
  mount namespace are the two boundaries).
- **SSE streaming** — canonical runtime events are translated to OpenAI
  `chat.completion.chunk` frames terminated by `data: [DONE]`.
- **Caller isolation** — sessions are scoped by the authenticated caller API key; unknown and
  wrong-owner session ids collapse to the same generic 404.

## Quick Start

```bash
# Build the gateway (API + worker Supervisor) and the worker child.
make build build-worker            # or: go build -o bin/gateway ./cmd/gateway
make test                          # vet + full test suite

# Run with default config (prod profile; nsjail required — run on Linux or
# point isolation.binary_path at a local nsjail build).
./bin/gateway -config config.example.toml
```

Dev on macOS uses the direct Worker development profile because nsjail is
Linux-only:

```bash
make dev
```

> The test profile spawns workers directly (no nsjail). Production and dev
> profiles refuse to start sessions without the sandbox — fail-closed.

## API

All `/v1/*` endpoints require `Authorization: Bearer <token>` unless no token
is configured. Health probes are unauthenticated.

| Method | Path | Description |
|---|---|---|
| GET | `/health/live` | Liveness: the process is up |
| GET | `/health/ready` | Readiness: process up AND the execution backend is ready (nsjail preflight passed); 503 otherwise |
| GET | `/v1/models` | Registered adapters (enabled + discoverable), sorted by id |
| POST | `/v1/chat/completions` | Streaming and non-streaming chat completions |
| POST | `/v1/sessions/{id}/abort` | Cancel the in-flight turn of a session |

### Example

```bash
# Model discovery
curl http://localhost:4096/v1/models \
  -H "Authorization: Bearer TOKEN"

# Streaming completion (SSE, ends with data: [DONE])
curl -N http://localhost:4096/v1/chat/completions \
  -H "Authorization: Bearer TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "codex",
    "stream": true,
    "messages": [{"role": "user", "content": "explain this repo"}],
    "metadata": {"workspace_id": "acme-project"}
  }'

# Abort the in-flight turn of a session (session id from the
# X-Gateway-Session-Id response header of the create request)
curl -X POST http://localhost:4096/v1/sessions/sess_abc/abort \
  -H "Authorization: Bearer TOKEN"
```

Sessions are created implicitly by the first chat request (no session id
supplied) and the generated id is returned in the `X-Gateway-Session-Id`
response header. Resume by sending that header back on the next request; the
gateway reuses the running worker execution.

Every Gateway session owns one persistent Worker and one persistent native
Agent CLI process. Normal turn completion returns that process to idle; it does
not close or restart it. A worker/CLI crash drops only that execution. The next
request starts a new Worker and passes the saved native thread/session ID so
the Agent can resume, but the Gateway never retries a turn whose completion is
unknown.

Each completion is one complete autonomous Agent turn. Claude Code, Codex, and
Kimi execute their own native tools inside the worker; those internal tool
events are not returned as OpenAI `tool_calls`. Emitting them as model tool
requests would make Agent frameworks execute an already-completed tool again
and resubmit the same user message. Display-only process summaries therefore
remain separate from the chat-completions tool-control flow.

Streaming responses expose safe Agent process information through the existing
OpenAI-compatible `delta.reasoning_content` string. Native reasoning is emitted
only when the Agent protocol provides content suitable for display. Each native
tool emits at most one terminal summary after it completes, encoded as:

```text
⏺ Bash
  ⎿ /workspace
```

The summary is also carried in `delta.reasoning_content`; it is display and
persistence data, not a request to execute the tool. Clients that already
consume reasoning need no new response-field adapter or Gateway-specific
parser. Successful tools use `⏺`; failed tools use `⨯`. The tool name is
followed by the sanitized, bounded result under `⎿`, with multiline output
indented beneath it. Each summary ends with `\n`, which provides a stable record
delimiter when clients concatenate streamed reasoning chunks. Clients must not
submit a tool-result message for this summary.
The Gateway never emits native tools as `delta.tool_calls`, tool-role messages,
or `finish_reason: "tool_calls"`, so an outer Agent runtime never starts a
second tool loop. Process telemetry is streaming-only in v1; non-streaming
responses remain final assistant text, usage, and finish reason.

The concurrency boundary is explicit: there is **one active turn per session**.
Different sessions in the **same workspace** may run concurrently and may
therefore modify the same files; filesystem conflict policy belongs to the
caller that owns the workspace.

## Agents

| Model id | Adapter | Lifecycle |
|---|---|---|
| `codex` | `adapters/codex` | persistent `codex app-server` JSON-RPC |
| `claude-code` | `adapters/claudecode` | persistent bidirectional `stream-json` |
| `kimi` | `adapters/kimi` | persistent `kimi acp` JSON-RPC |

Adapters are registered by name into the runtime registry. `agent/<name>` owns
native CLI launch, protocol parsing, resume IDs, usage, abort, and process
teardown. `adapters/<name>` only converts trusted configuration, prompts,
events, and lifecycle calls to the canonical `runtime.AgentAdapter` contract.
There is no per-turn CLI fallback.

Caller `system` role messages are **ignored by default**: they are where agent
frameworks dump their own tool/skill libraries, and forwarding them would
pollute the native agent's own tool/skill surface. A deployment that wants to
let callers steer a specific agent sets `inject_system_prompt = true`, which
prepends the caller's system messages (as plain text instructions, never tool
or skill definitions) to each turn's prompt.

Abort is turn-scoped. Codex sends `turn/interrupt`; Kimi sends
`session/cancel`; a successful cancellation leaves the Worker and Agent
process available for the next turn. Claude Code's current stream-json surface
does not provide an equivalent reliable interrupt, so its adapter escalates by
terminating the native process; the dead handle is discarded and the next
request resumes through the saved native session ID.

## Isolation

Production and dev profiles require nsjail:

- The supervisor forks `nsjail -Mo --config <generated profile> -- gateway-worker`
  per session, one jail per session.
- nsjail runs **unprivileged**: user namespace + mount namespace, no
  `CAP_SYS_ADMIN`, no `--privileged`. The container image drops all
  capabilities; the example k8s security context uses `runAsNonRoot: true`
  with `runAsUser/Group: 65532`.
- The controlled workspace is bind-mounted read-write at `/workspace`, the
  per-session agent home at `/agent-home`, and `/tmp` is a per-session tmpfs.
- Phase 1 keeps the network namespace shared so agents can reach their
  providers; egress is controlled by the container/infrastructure.
- Each jail gets its own **PID namespace** (`clone_newpid`, default on): the
  worker is PID 1, so killing the nsjail wrapper reaps the whole tree (even a
  CLI that escaped into its own process group), and a compromised agent cannot
  signal the gateway or sibling sessions. A namespaced `/proc` is mounted.
- **Fail-closed**: a missing/inexecutable nsjail, an unbuildable profile, or a
  spawn failure makes readiness 503 and refuses to start sessions. On
  non-Linux hosts the jailed path fails closed too — it never silently falls
  back to an unsandboxed spawn.

Worker failures are isolated to the session: a crashed worker is reaped by the
supervisor (SIGTERM → SIGKILL escalation, process-group kill) and the gateway
keeps serving. Unknown turn outcomes are never automatically replayed.
`docker/nsjail-smoke.sh` validates the nsjail build on Linux CI in the documented
production security context (non-root `65532`, `cap-drop=ALL`,
`no-new-privileges`, no `--privileged`): `ldd`, a minimal jail, and a per-jail
PID-namespace tree-kill assertion. An arm64 runner runs the same smoke so the
aarch64 seccomp policy is exercised against real arm64 syscalls, not just a
cross-build.

## Configuration

`gateway -config gateway.toml`. Keys absent from the file retain defaults.
See [`config.example.toml`](config.example.toml) for a full annotated example.

| Section | Key | Default | Meaning |
|---|---|---|---|
| (root) | `mode` | `prod` | `prod` / `dev` / `test`. Only `test` may disable nsjail |
| `[server]` | `listen_addr` | `:4096` | HTTP listen address |
| `[server]` | `drain_timeout` | `30s` | HTTP request drain bound during Gateway shutdown |
| `[server]` | `read_header_timeout` / `idle_timeout` / `max_header_bytes` | `10s` / `60s` / `1048576` | Slow-header / keep-alive / header-size bounds (no `WriteTimeout` so SSE survives) |
| `[server]` | `turn_timeout` | `10m` | Gateway run deadline per turn (`0` = disabled); enforced at the worker boundary and presented uniformly (HTTP 504 / SSE error frame, code `turn_timeout`) |
| `[worker]` | `stop_grace_period` | `10s` | CloseSession/SIGTERM grace before SIGKILL |
| `[worker]` | `heartbeat_interval` / `heartbeat_timeout` / `heartbeat_failures` | `15s` / `3s` / `3` | Runtime Worker failure detection |
| `[sessions]` | `idle_timeout` / `reap_interval` | `2h` / `1m` | Idle Worker reclamation; conversation/native session identity is retained |
| `[auth]` | `callers` | *(empty)* | Bearer token to caller ID mappings |
| `[workspace]` | `root` | `workspaces` | Root all workspace ids resolve under |
| `[isolation]` | `required` | `true` | Must be true outside the test profile |
| `[isolation]` | `nsjail_version` / `nsjail_source` | `3.6` / upstream URL | Pinned build provenance |
| `[isolation]` | `binary_path` | `/usr/local/bin/nsjail` | nsjail executable |
| `[isolation.mounts]` | `workspace_dir` / `agent_home_dir` / `tmp_dir` | `/workspace` / `/agent-home` / `/tmp` | Sandbox mount layout |
| `[isolation]` | `clone_newpid` | `true` | Per-jail PID namespace (PID 1 = worker; a namespaced `/proc` is mounted) |
| `[isolation.user_namespace]` | `enabled`, `uid`, `gid` | `true`, `65532`, `65532` | Unprivileged user namespace |
| `[isolation.seccomp]` | `policy` | `kafel` | `kafel` or `off` (test only); policy arch is selected from `GOARCH` |
| `[agents.<id>]` | `enabled` | `false` | Whether the agent is available (see "Installing and enabling an agent") |
| `[agents.<id>]` | `command` | *(empty)* | CLI command as an argv array (`command = ["codex"]`), executed verbatim (no shell); the executable is probed before the agent is advertised. A plain string is accepted for one deprecation window |
| `[agents.<id>]` | `permission` | `auto` | `auto` / `ask` / `deny` |
| `[agents.<id>]` | `timeout` | *(falls back to `server.turn_timeout`)* | Per-agent turn deadline; honored by every adapter at the worker boundary |
| `[agents.<id>]` | `inject_system_prompt` | `false` | Forward caller `system` role messages into the native prompt each turn |

Agent settings are passed to the single-session worker over the canonical RPC
contract; provider secrets remain worker/container environment configuration.

## Installing and enabling an agent

The base image ships **no agent CLIs**, and every agent defaults to **disabled**,
so `/v1/models` advertises nothing out of the box. To make an agent available:

1. install its CLI, then
2. set `[agents.<id>] enabled = true` and `command` to the installed binary.

Model discovery probes the command's executable (`exec.LookPath` on `argv[0]`)
and only advertises an agent whose CLI is actually installed, so the image can
never claim an agent it cannot run. This is a **presence check**, not a version/health probe: it does
not execute the CLI or validate its version (the API process never launches an
Agent CLI; version/readiness probing is deferred to Phase 5, where Workers
advertise their native CLI versions). The three CLIs are Node packages:

| Agent | npm package | default binary |
|---|---|---|
| Codex | `@openai/codex` | `codex` |
| Claude Code | `@anthropic-ai/claude-code` | `claude` |
| Kimi | *(pinned per deployment; see `adapters/kimi/SOURCE.md`)* | `kimi` |

**Path A — bake the CLIs (product image).** Build the base image, then the
product image on top of it:

```bash
make image-base
make image-product CODEX_VERSION=... CLAUDE_VERSION=...
```

`docker/Dockerfile.agents` layers Node.js and the pinned npm packages onto the
base image, then mount (or let the entrypoint write) a config that enables
them, e.g. `[agents.codex] enabled = true` + `command = ["codex"]`.

> **The provided product image ships two agents (Codex + Claude Code).** Kimi's
> published npm package name is not pinned in this repository, so it is not
> installed by `docker/Dockerfile.agents`. A Kimi deployment must build its own
> derived image (`FROM agent-gateway:…` then `RUN npm install -g <kimi-cli-package>@<pin>`).

**Path B — derived image or volume.** Extend the image yourself
(`FROM agent-gateway:…` then `RUN npm install -g …`), or bind-mount a directory
containing the CLIs into one of the read-only-mounted paths (`/usr`, `/usr/local`,
`/bin`, `/lib`) and point `command` at it. The jail bind-mounts those paths
read-only, so a CLI installed there is visible to both the gateway probe and the
jailed worker.

## Architecture

The complete data path is:

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
  ├── config         TOML config (gateway.toml)
  └── worker
      ├── supervisor   forks/reaps one nsjail-wrapped worker per session
      ├── rpc          gRPC Worker contract over per-session Unix sockets
      └── nsjail       profile generation + preflight
              │  fork: nsjail -Mo --config profile -- gateway-worker
              ▼
cmd/gateway-worker   (one per session; the ONLY process that runs agent CLIs)
  └── adapters/{codex,claudecode,kimi}   runtime/native mapping only
        │   (shared wrapper + turn-deadline enforcement: adapters/internal/bridge)
        └── agent/{codex,claudecode,kimi} native execution + protocol
              (shared event contract: agent/events; shared JSON-RPC session
               lifecycle: agent/rpcsession)
```

- The API process **never** launches an agent CLI — every execution flows
  through `worker.LocalExecutionBackend` → supervisor → jailed worker.
- Native packages cannot import `runtime`, adapters, Worker, API, or config.
  Adapters cannot launch processes or parse raw Agent protocols. Worker and API
  packages never contain concrete Agent protocol knowledge.
- Per-agent deployment config reaches adapter factories as typed
  `runtime.AdapterConfig` — there is no environment-variable roundtrip.
- Turn deadlines are a three-layer contract: HTTP transport bounds headers/idle
  only (SSE is never cut by a write deadline); the gateway run deadline
  (`server.turn_timeout`, `agents.<id>.timeout`) is enforced at the worker
  boundary (the bridge aborts the native turn) and presented uniformly by the
  API (504 / SSE error frame, code `turn_timeout`); native sessions bound only
  their settle/teardown waits (`CloseTimeout`).

## Docker

```bash
make docker            # docker build -t agent-gateway:dev .

docker run --rm -it \
  --cap-drop=ALL \
  -v "$PWD/workspaces:/srv/workspaces" \
  -p 4096:4096 \
  agent-gateway:dev
```

The image is multi-stage: it compiles `google/nsjail` from a pinned tag
(autoconf/bison/flex/libprotobuf/libnl), builds the Go binaries, and the
runtime stage copies the nsjail binary plus its `ldd`-resolved shared libs and
the agent CLIs the deployment needs. `docker/entrypoint.sh` prepares the
runtime dirs, writes a default config when none is mounted, and `exec`s the
Gateway beneath Tini. Tini runs as PID 1, forwards signals, and reaps orphaned
descendants; the supervisor then
propagates shutdown API → worker(nsjail) → CLI and reaps the process group.
See the `Dockerfile` header for the k8s `securityContext` and Linux-CI
validation hooks.

## Development

```bash
make fmt                 # gofmt across agent api runtime worker adapters config cmd integration
make vet                 # go vet ./...
make test                # vet + go test ./...
make test-race           # go test -race ./...  (CI)
make test-integration    # container-level integration tests only
make build build-worker  # build both binaries into bin/
make generate            # regen worker/proto after editing worker.proto
```

Cross-platform builds (the darwin dev host, Linux production, and Windows dev)
are all supported:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

### Testing

- `worker/` and `adapters/` have unit tests with hermetic stub binaries — no
  real agent CLIs required.
- `integration/agent_gateway_test.go` runs the real HTTP API against the real
  supervisor and a stub worker child end to end: health, model discovery,
  streaming completion, abort, worker-crash recovery, workspace out-of-bounds
  rejection, and nsjail preflight fail-closed.
- Real nsjail smoke (version + ldd + minimal jail) runs on Linux CI via
  `docker/nsjail-smoke.sh`.

## License and provenance

Agent CLI lifecycle and protocol behavior was selectively migrated from
[cc-connect](https://github.com/chenhg5/cc-connect) at commit
`3fc360ee6acc9bab13ab1b48ddde3af44062903b`. See each
`adapters/<name>/SOURCE.md` and `LICENSES/cc-connect-MIT.txt`. The upstream
README declares MIT License, but its linked standalone license file is absent
at that baseline; the notice records this caveat explicitly.

The persistent Codex app-server sequence was independently checked against
Hermes Agent commit `9d6c5a920c773f86fad9ea16528212faeaa21815`
(MIT). Kimi ACP v1 behavior was checked against MoonshotAI Kimi Code commit
`2acf22f66e15361d9804d9014d58ad68a9383caf` (MIT). Exact file-level evidence
is recorded in the corresponding `adapters/<name>/SOURCE.md`.
