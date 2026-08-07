# Source / provenance

The Kimi adapter in this directory is a **selective migration** of the mature,
proven Kimi protocol code that already lived in this repository at
`agent/kimi/` — which itself originated in **cc-connect** (MIT License
© chenhg5, see repo `LICENSE`).

## Upstream baseline

- Upstream project: cc-connect (https://github.com/chenhg5/cc-connect), MIT License.
- In-repo migration baseline: `agent/kimi/` as of commit
  `cd7e24b033904ef2acc36a1ad4064daf3db386c4` (2026-08-07), the HEAD of the
  gateway-rewrite work when this task ran. The rewrite Tasks 1–5 landed on
  `main` before this adapter was extracted.
- Migration rule (per the plan's 决策对齐 #3): port the protocol, not the IM
  surface. The old `core`/IM types were replaced with the new `runtime`
  contract; nothing of cc-connect's core, Platform, IM, cron/timer/relay or
  management code was copied.

## Source → new file mapping

| New file                      | Migrated from                      | What was cut / adapted                                                                              |
|-------------------------------|------------------------------------|-----------------------------------------------------------------------------------------------------|
| `adapters/kimi/adapter.go`    | `agent/kimi/kimi.go`               | `core.Agent`/`RegisterAgent` → `runtime.AgentAdapter`/`runtime.Register`; dropped providers, session listing (`listKimiSessions`/`DeleteSession`), skill dirs, memory files, `PermissionModes` UI, `ModeSwitcher`, `SetSessionEnv`/provider-switch plumbing. Added `Describe`/`Start`/`Options` per the runtime contract. |
| `adapters/kimi/session.go`    | `agent/kimi/session.go`            | `core.AgentSession`/`core.Event*` → `runtime.Session`/`runtime.Event`; `Send(prompt, images, files)` → `Send(ctx, runtime.Input)`; added `Abort` (per-turn process-group kill) and idempotent `Close(ctx)`; dropped image/file attachment staging; `EventThinking` folded into `EventText`. |
| `adapters/kimi/protocol.go`   | `agent/kimi/probe.go` + `session.go`/`kimi.go` helpers | Pure helpers: `probeKimiFlags`/`parseKimiHelpFlags` (from `probe.go`), `extractResumeSessionID`, `normalizeMode`, `truncate`, prompt building, `mergeEnv`/`redactArgs`. `core.*` deps cut. |
| `adapters/kimi/proc_unix.go`  | `agent/codex/proc_unix.go` pattern | Verbatim port of the process-group kill helper.                                                     |
| `adapters/kimi/proc_windows.go` | `agent/codex/proc_windows.go` pattern | Verbatim port of the process-group kill helper.                                                     |
| `adapters/kimi/adapter_test.go` | `agent/kimi/session_test.go`, `kimi_test.go`, `probe_test.go` | Adapted to the `runtime` contract (fake-kimi scripts, `runtime.Input` sends, `EventFinish` instead of `core.EventResult`). Dropped IM/provider tests (`provider_resume_test.go` — provider env restore is cc-connect engine logic; `TestAgentStartSession` — old `StartSession(ctx,id)` surface; `TestAgentMemoryAndSkill`, `TestAgentProviderSwitcher`, `TestAgentPermissionModes`). |

## Old `core`/IM dependencies cut

Every `github.com/tianlinzz/agent-cli-gateway/core` import is gone; the package
imports stdlib + `github.com/tianlinzz/agent-cli-gateway/runtime` only. No
`net/http`, no `server/`, no Platform/IM types. Specific `core.*` symbols that
no longer appear: `core.Agent`, `core.AgentSession`, `core.Event*`,
`core.ProviderConfig`, `core.ModelOption`, `core.ParseCmdOpts`,
`core.ParseConfigEnv`, `core.GetProviderModel`, `core.MergeEnv`,
`core.RedactArgs`, `core.PermissionModeInfo`, `core.AgentSessionInfo`,
`core.ContinueSession`, `core.SaveFilesToDisk`.

## Deliberately NOT migrated (YAGNI, deleted in task 7)

- Provider switching / `provider_resume_test.go` (multi-provider session-resume
  restore is cc-connect engine/IM logic).
- Session listing / `listKimiSessions` / `findKimiSessionDir` / `DeleteSession`.
- Skill dirs / memory files (`AGENTS.md`).
- `PermissionModes` / `ModeSwitcher` management UI.
- Image/file attachment staging (the runtime contract carries text content only).
- Kimi CLI auto-approves tool calls in non-interactive mode, so no native
  permission-request surface exists; the canonical permission event path is
  preserved in the runtime contract regardless.
