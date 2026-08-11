# Kimi source provenance

This adapter is a thin runtime bridge over the native implementation in
`agent/kimi/`. The current implementation uses Agent Client Protocol v1 over
`kimi acp` as its only production protocol.

## Upstream source files and protocol reference

- MoonshotAI Kimi Code commit
  `2acf22f66e15361d9804d9014d58ad68a9383caf` was used to verify the ACP
  handshake, session lifecycle, prompt/update shapes, permission response,
  and cancellation behavior.
- Relevant upstream files include `docs/en/reference/kimi-acp.md`,
  `packages/acp-server/src/server.ts`, `events-map.ts`, and the ACP lifecycle
  and end-to-end turn tests.
- The earlier adapter selectively migrated print-mode behavior from
  [cc-connect](https://github.com/chenhg5/cc-connect) commit
  `3fc360ee6acc9bab13ab1b48ddde3af44062903b`.
  That per-turn implementation and its compatibility probe have been removed.

## Migrated behaviors

- One `kimi acp` process and one ACP session per Gateway session.
- One initialize plus session create or resume handshake.
- Repeated asynchronous `session/prompt` calls on the same process.
- Streaming text, stable tool telemetry, usage updates, deterministic
  permission responses, and turn-scoped `session/cancel`.
- Process termination only for explicit close, ACP failure, or failed/timed-out
  cancellation escalation.

## Material local modifications

- `agent/kimi/` depends only on shared `agent/process`, `agent/protocol`, and
  the Go standard library.
- `adapters/kimi/` maps trusted Gateway options, prompts, events, and
  lifecycle calls to `runtime`; it contains no process or JSON-RPC code.
- Native tools remain internal telemetry and never become OpenAI client-side
  `tool_calls`.
- Model/provider management, session listing, attachments, and messaging
  platform behavior are outside this adapter.

## Local regression tests

- `agent/kimi/native_exec_test.go`
- `adapters/kimi/adapter_test.go`
- `integration/agent_gateway_test.go`

MoonshotAI Kimi Code is MIT licensed (Moonshot AI, 2026). The pinned
cc-connect baseline declares MIT in its README but has no standalone license
file; see `LICENSES/cc-connect-MIT.txt` for the recorded evidence and caveat.
