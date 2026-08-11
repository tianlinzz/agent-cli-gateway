# Codex source provenance

This adapter is a thin runtime bridge over the native implementation in
`agent/codex/`. The native implementation selectively migrates proven Codex
CLI behavior from [cc-connect](https://github.com/chenhg5/cc-connect) at
upstream baseline `3fc360ee6acc9bab13ab1b48ddde3af44062903b`.

## Upstream source files

- `agent/codex/codex.go`: command defaults and Agent construction.
- `agent/codex/session.go`: `codex exec --json`, native thread resume, event
  ordering, and per-turn process lifecycle.
- `agent/codex/context_usage.go`: token usage parsing.
- `agent/codex/proc_unix.go` and `agent/codex/proc_windows.go`: process-tree
  termination behavior.

## Migrated behaviors

- Fresh `codex exec --json` turns and `codex exec resume` continuity.
- Native thread IDs, ordered item/tool/text events, and usage accounting.
- Per-turn abort/reap while preserving the logical session for later resume.

## Material local modifications

- Native code now lives in `agent/codex/` and depends only on shared
  `agent/process`, `agent/protocol`, and the Go standard library.
- `adapters/codex/` only maps trusted Gateway options, prompts, events, and
  lifecycle calls to `runtime`; it contains no process or JSONL code.
- The unsupported app-server backend is rejected during adapter construction;
  the robust exec backend is the only production path.
- cc-connect provider switching, quota HTTP calls, session listing/history,
  skills, attachments, and IM-specific phase metadata were not migrated.

## Local regression tests

- `agent/codex/native_exec_test.go`
- `adapters/codex/adapter_test.go`
- `integration/agent_gateway_test.go`

The upstream README declares MIT License at the pinned baseline. See
`LICENSES/cc-connect-MIT.txt` for the exact evidence and the missing-license
file caveat.
