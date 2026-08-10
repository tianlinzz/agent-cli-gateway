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
- **Three first-generation agents** — `codex`, `claude-code`, `kimi`, each a
  self-contained adapter (`adapters/`) driving the agent's native CLI protocol.
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

Dev on macOS (no nsjail): use the `test` profile, which disables isolation:

```toml
mode = "test"
[workspace]
root = "./workspaces"
```

```bash
./bin/gateway -config /dev/stdin <<'EOF'
mode = "test"
[workspace]
root = "./workspaces"
EOF
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

## Agents

| Model id | Adapter | Lifecycle |
|---|---|---|
| `codex` | `adapters/codex` | resume per turn |
| `claude-code` | `adapters/claudecode` | persistent process |
| `kimi` | `adapters/kimi` | resume per turn |

Adapters are registered by name into a process-wide runtime registry
(`runtime/`). Each adapter implements the canonical `runtime.AgentAdapter`
contract and drives its agent's native CLI (exec stream-json / JSON-RPC),
converting CLI output into canonical runtime events. The gateway API layer is
adapter-agnostic — it only speaks the canonical contract.

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
- **Fail-closed**: a missing/inexecutable nsjail, an unbuildable profile, or a
  spawn failure makes readiness 503 and refuses to start sessions. On
  non-Linux hosts the jailed path fails closed too — it never silently falls
  back to an unsandboxed spawn.

Worker failures are isolated to the session: a crashed worker is reaped by the
supervisor (SIGTERM → SIGKILL escalation, process-group kill) and the gateway
keeps serving. `docker/nsjail-smoke.sh` validates the nsjail build on Linux CI
(`--version`, `ldd`, minimal jail).

## Configuration

`gateway -config gateway.toml`. Keys absent from the file retain defaults.
See [`config.example.toml`](config.example.toml) for a full annotated example.

| Section | Key | Default | Meaning |
|---|---|---|---|
| (root) | `mode` | `prod` | `prod` / `dev` / `test`. Only `test` may disable nsjail |
| `[server]` | `listen_addr` | `:4096` | HTTP listen address |
| `[server]` | `shutdown_timeout` | `10s` | Graceful shutdown bound |
| `[auth]` | `callers` | *(empty)* | Bearer token to caller ID mappings |
| `[workspace]` | `root` | `workspaces` | Root all workspace ids resolve under |
| `[isolation]` | `required` | `true` | Must be true outside the test profile |
| `[isolation]` | `nsjail_version` / `nsjail_source` | `3.6` / upstream URL | Pinned build provenance |
| `[isolation]` | `binary_path` | `/usr/local/bin/nsjail` | nsjail executable |
| `[isolation.mounts]` | `workspace_dir` / `agent_home_dir` / `tmp_dir` | `/workspace` / `/agent-home` / `/tmp` | Sandbox mount layout |
| `[isolation.user_namespace]` | `enabled`, `uid`, `gid` | `true`, `1000`, `1000` | Unprivileged user namespace |
| `[isolation.seccomp]` | `policy` | `kafel` | `kafel` or `off` (test only) |
| `[agents.<id>]` | `enabled` | `true` | Whether the agent is available |
| `[agents.<id>]` | `permission` | `auto` | `auto` / `ask` / `deny` |

Agent settings are passed to the single-session worker over the canonical RPC
contract; provider secrets remain worker/container environment configuration.

## Architecture

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
  └── adapters/{codex,claudecode,kimi}   native CLI process mgmt + parsing
```

- The API process **never** launches an agent CLI — every execution flows
  through `worker.LocalExecutionBackend` → supervisor → jailed worker.
- `core/`, `server/`, `platform/`, `daemon/`, `web/`, `npm/` (the IM-gateway
  layer) were removed in this rewrite. `agent/{codex,claudecode,kimi}/` are
  kept as the migration reference the `adapters/*/SOURCE.md` files cite; they
  are compiled out behind the `agent_ref` build tag.

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
gateway so it is PID 1 and receives SIGTERM directly; the supervisor then
propagates shutdown API → worker(nsjail) → CLI and reaps the process group.
See the `Dockerfile` header for the k8s `securityContext` and Linux-CI
validation hooks.

## Development

```bash
make fmt                 # gofmt across api runtime worker adapters config cmd integration
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
GOOS=linux go build ./...
GOOS=darwin go build ./...
GOOS=windows go build ./...
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

## License

Forked from cc-connect. See upstream for license details.
