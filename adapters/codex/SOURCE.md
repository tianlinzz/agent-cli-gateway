# Codex source provenance

This adapter is a thin runtime bridge over the native implementation in
`agent/codex/`. The current implementation uses Codex app-server v2 as its
only production protocol.

## Upstream source files and protocol references

- The installed Codex CLI `0.145.0` generated the JSON schemas used to verify
  `initialize`, `thread/start`, `thread/resume`, `turn/start`,
  `turn/interrupt`, notifications, and reverse approval requests.
- Hermes Agent commit
  `9d6c5a920c773f86fad9ea16528212faeaa21815`, especially
  `agent/transports/codex_app_server.py`, was reviewed as an independent
  example of the stable initialize/thread/turn sequence.
- The earlier native adapter selectively migrated `codex exec` behavior from
  [cc-connect](https://github.com/chenhg5/cc-connect) commit
  `3fc360ee6acc9bab13ab1b48ddde3af44062903b`.
  That resume-per-turn implementation has now been removed.

## Migrated behaviors

- One `codex app-server --listen stdio://` process per Gateway session.
- One initialization handshake and one native thread start or resume.
- Repeated turns over the same JSON-RPC connection and process.
- Stable tool telemetry, private reasoning filtering, native usage updates,
  deterministic reverse-request responses, and turn-scoped interrupt.
- Process termination only for explicit session close, app-server failure, or
  failed/timed-out interrupt escalation.

## Material local modifications

- `agent/codex/` depends only on shared `agent/process`, `agent/protocol`, and
  the Go standard library.
- `adapters/codex/` maps trusted Gateway options, prompts, events, and
  lifecycle calls to `runtime`; it contains no process or JSON-RPC code.
- Native tools remain internal telemetry and are never projected as OpenAI
  client-side `tool_calls`.
- Provider switching, quota HTTP calls, session listing/history, attachments,
  and messaging-platform behavior are outside this adapter.

## Local regression tests

- `agent/codex/native_exec_test.go`
- `adapters/codex/adapter_test.go`
- `integration/agent_gateway_test.go`

Hermes Agent is MIT licensed (Nous Research, 2025). The pinned cc-connect
baseline declares MIT in its README but has no standalone license file; see
`LICENSES/cc-connect-MIT.txt` for the recorded evidence and caveat.
