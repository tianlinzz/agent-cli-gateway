# Claude Code source provenance

This adapter is a thin runtime bridge over the native implementation in
`agent/claudecode/`. The native implementation selectively migrates proven
Claude Code CLI behavior from [cc-connect](https://github.com/chenhg5/cc-connect)
at upstream baseline `3fc360ee6acc9bab13ab1b48ddde3af44062903b`.

## Upstream source files

- `agent/claudecode/claudecode.go`: command construction and Agent defaults.
- `agent/claudecode/session.go`: persistent stream-json lifecycle, event
  parsing, native session IDs, usage, compaction, and permission flow.
- `agent/claudecode/proc_unix.go` and `agent/claudecode/proc_windows.go`:
  process-tree termination behavior.

## Migrated behaviors

- One persistent `claude` stream-json process per native session.
- Native session resume, ordered text/tool/result events, usage accounting,
  compaction handling, and permission events.
- Process-tree teardown and bounded close behavior.

## Material local modifications

- Native code now lives in `agent/claudecode/` and depends only on shared
  `agent/process`, `agent/protocol`, and the Go standard library.
- `adapters/claudecode/` only maps trusted Gateway options, prompts, events,
  and lifecycle calls to the transport-neutral `runtime` contract.
- `AskUserQuestion` is disabled in argv and denied at the protocol boundary so
  a non-interactive OpenAI request cannot hang waiting for terminal input.
- cc-connect IM, provider switching, history/listing, skills, management UI,
  and platform-specific behavior were intentionally not migrated.

## Local regression tests

- `agent/claudecode/native_test.go`
- `adapters/claudecode/adapter_test.go`
- `integration/agent_gateway_test.go`

The upstream README declares MIT License at the pinned baseline. See
`LICENSES/cc-connect-MIT.txt` for the exact evidence and the missing-license
file caveat.
