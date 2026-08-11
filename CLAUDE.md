# Agent Gateway Development Guide (CLAUDE.md)

This repository is an OpenAI-compatible **Agent Gateway**: coding agents
(Codex, Claude Code, Kimi) are exposed behind a single HTTP API. Clients speak
standard OpenAI shapes (`/v1/models`, `/v1/chat/completions`, SSE streaming);
the gateway forks one **nsjail-isolated worker process per session**, and the
worker owns the agent CLI. This is a rewrite of cc-connect — the IM platform
layer (Feishu/Telegram/Discord and the rest) and the management/event APIs are
gone.

The authoritative developer guide is **AGENTS.md** (loaded into agent context).
This file is a concise orientation; keep it consistent with AGENTS.md.

## Architecture at a glance

```text
OpenAI HTTP/SSE -> runtime.ExecutionBackend -> Worker Supervisor/RPC
-> adapters/<name> -> agent/<name> -> Agent CLI process
```

```
cmd/gateway          API process + worker Supervisor (the only entrypoint)
  ├── api/openai     OpenAI-compatible HTTP layer (models/chat/abort/health)
  ├── runtime        canonical contract: descriptors, events, session store,
  │                  registry, execution backend — the agnostic nucleus
  ├── config         TOML config (config.GatewayConfig)
  └── worker         supervisor + gRPC RPC layer + nsjail profile/preflight
cmd/gateway-worker   one per session; the ONLY process that runs agent CLIs
  └── adapters/{codex,claudecode,kimi}   thin runtime bridges
        └── agent/{codex,claudecode,kimi} native execution + protocol
```

## Non-negotiables

- `runtime/` is the nucleus and stays **name-agnostic**: no hardcoded agent
  names, no adapter/HTTP/gRPC imports. `api/openai/` speaks only the runtime
  contract and must never import `adapters/` or `worker/`. `worker/` never
  imports concrete agents or adapters.
- `agent/<name>` owns CLI process lifecycle, native protocol, resume, usage,
  abort, and reaping. `adapters/<name>` only maps trusted options, prompts,
  native events, and lifecycle calls to `runtime`.
- Adapters register by string name via `runtime.Register(name, factory)` from
  `init()`; the `plugin_agent_*.go` files in `cmd/gateway/` and
  `cmd/gateway-worker/` wire them in (build tag `!no_<agent>`).
- **Isolation is mandatory.** Only the `test` config mode may disable nsjail;
  prod/dev fail closed without the sandbox. Never silently fall back to an
  unsandboxed spawn. The API process never launches an agent CLI itself.
- Workspace access is a security boundary: clients submit opaque
  `workspace_id`s; `workspace/` resolves them under a configured root (no
  client-controlled absolute path can ever be injected).
- Concurrency is **one active turn per session**. Distinct sessions in the
  **same workspace** can run concurrently; callers own filesystem conflicts.
- Permission is part of the canonical contract (`runtime.PermissionRequest`),
  with per-agent mode `auto` / `ask` / `deny`.
- Error handling: wrap errors with context, use `slog` consistently (never
  `log.Printf`/`fmt.Printf` for runtime logs), redact tokens/secrets.

## Testing

- `go test ./...` — full suite (unit + integration).
- `go test -race ./...` — for concurrency-sensitive changes (CI).
- `go test ./integration/ -v` — hermetic E2E (real API → real supervisor →
  stub worker; darwin-runnable, no agent CLI/nsjail needed).
- Cross-platform is a project invariant:
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` and
  `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...` must pass.
- Real nsjail smoke (`docker/nsjail-smoke.sh`) is Linux-CI-only.

## Ownership boundaries

- Native packages cannot import runtime/config/worker/api/adapters.
- Adapters cannot use `os/exec`, manage process groups, or parse raw JSONL.
- API and Worker packages cannot switch on Agent-native event formats.
- Upstream attribution and local changes are documented in
  `adapters/<name>/SOURCE.md`; the licensing evidence is in
  `LICENSES/cc-connect-MIT.txt`.
