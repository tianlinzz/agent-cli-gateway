# Kimi source provenance

This adapter is a thin runtime bridge over the native implementation in
`agent/kimi/`. The native implementation selectively migrates proven Kimi CLI
behavior from [cc-connect](https://github.com/chenhg5/cc-connect) at upstream
baseline `3fc360ee6acc9bab13ab1b48ddde3af44062903b`.

## Upstream source files

- `agent/kimi/kimi.go`: Agent defaults and command configuration.
- `agent/kimi/session.go`: stream-json parsing, resume trailer extraction,
  ordered events, and per-turn process lifecycle.
- `agent/kimi/probe.go`: installed CLI flag-surface probing.
- `agent/codex/proc_unix.go` and `agent/codex/proc_windows.go`: the shared
  process-tree termination pattern used by the upstream Kimi path.

## Migrated behaviors

- One Kimi CLI process per turn with native session resume.
- Conservative `--print` compatibility probing across Kimi CLI generations.
- Ordered assistant/tool/result events, usage, timeout, abort, and reaping.

## Material local modifications

- Native code now lives in `agent/kimi/` and depends only on shared
  `agent/process`, `agent/protocol`, and the Go standard library.
- `adapters/kimi/` probes once at construction and only maps trusted options,
  prompts, native events, and lifecycle calls to `runtime`.
- cc-connect provider switching, session listing/history, skills, management
  modes, attachments, and IM-facing behavior were intentionally not migrated.

## Local regression tests

- `agent/kimi/native_exec_test.go`
- `adapters/kimi/adapter_test.go`
- `integration/agent_gateway_test.go`

The upstream README declares MIT License at the pinned baseline. See
`LICENSES/cc-connect-MIT.txt` for the exact evidence and the missing-license
file caveat.
