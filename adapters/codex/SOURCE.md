# Source / provenance

The Codex adapter in this directory is a **selective migration** of the mature,
proven Codex protocol code that already lived in this repository at
`agent/codex/` — which itself originated in **cc-connect** (MIT License
© chenhg5, see repo `LICENSE`).

## Upstream baseline

- Upstream project: cc-connect (https://github.com/chenhg5/cc-connect), MIT License.
- In-repo migration baseline: `agent/codex/` as of commit
  `d863bc0d946289c47b4e88416ba59cbbb0a176e0` (2026-08-07), the HEAD of the
  gateway-rewrite work when this task ran. The rewrite Tasks 1–3 landed on
  `main` before this adapter was extracted.
- Migration rule (per the plan's 决策对齐 #3): port the protocol, not the IM
  surface. The old `core`/IM types were replaced with the new `runtime`
  contract; nothing of cc-connect's core, Platform, IM, cron/timer/relay or
  management code was copied.

## Source → new file mapping

| New file                          | Migrated from                                     | What was cut / adapted                                                        |
|-----------------------------------|---------------------------------------------------|------------------------------------------------------------------------------|
| `adapters/codex/adapter.go`       | `agent/codex/codex.go`                            | `core.Agent`/`RegisterAgent` → `runtime.AgentAdapter`/`runtime.Register`; dropped providers, model catalog fetching (net/http), skills, memory files, workspace listing, history, `PermissionModes`. Added `Describe`/`Start`/`Options` per the runtime contract. |
| `adapters/codex/session.go`       | `agent/codex/session.go` (+ proc_unix/proc_windows) | `core.AgentSession`/`core.Event*` → `runtime.Session`/`runtime.Event`; `Send(prompt, images, files)` → `Send(ctx, runtime.Input)`; added `Abort`; dropped image staging, `core.MergeEnv`/`core.RedactArgs`/`core.SaveFilesToDisk`/`core.AppendFileRefs`, runtime-config probe (`codex app-server`), `patchSessionSource`. |
| `adapters/codex/protocol.go`      | `agent/codex/session.go` (parsing helpers) + `agent/codex/context_usage.go` | Pure helpers: `readJSONLines`, `extractItemText`, `codexExtractToolInput`, `codexToolSuccess`, prompt building, `mergeEnv`/`redactArgs`, and rollout token-count parsing mapped to `runtime.Usage` (was `core.ContextUsage`). `core.*` deps cut. |
| `adapters/codex/proc_unix.go`     | `agent/codex/proc_unix.go`                        | Verbatim port.                                                                |
| `adapters/codex/proc_windows.go`  | `agent/codex/proc_windows.go`                     | Verbatim port.                                                                |
| `adapters/codex/adapter_test.go`  | `agent/codex/session_test.go`, `usage_test.go`, `context_usage.go` tests | Adapted to `runtime` contract (fake-codex scripts, `runtime.Input` sends, `EventFinish` instead of `core.EventResult`). Dropped IM-specific and net/http-based tests (quota usage HTTP, image attachments, runtime-config probe). |

## Old `core`/IM dependencies cut

Every removed legacy nucleus import is gone; the package
imports stdlib + `github.com/tianlinzz/agent-cli-gateway/runtime` only. No
`net/http`, no `server/`, no Platform/IM types. Specific `core.*` symbols that
no longer appear: `core.Agent`, `core.AgentSession`, `core.Event*`,
`core.ProviderConfig`, `core.ModelOption`, `core.ParseCmdOpts`,
`core.MergeEnv`, `core.RedactArgs`, `core.SaveFilesToDisk`,
`core.AppendFileRefs`, `core.HistoryEntry`, `core.WorkspaceLister`,
`core.ContextUsage`, `core.ContinueSession`, `core.PermissionModeInfo`,
`core.UsageReport`/`core.UsageWindow`/`core.UsageBucket`.

## Deliberately NOT migrated (YAGNI, deleted in task 7)

- `app_server` backend (deferred; `Start` rejects it with a clear error).
- Provider switching / `provider_config.go` (auth.json writing), `live_config.go`.
- `list.go` (session listing), `GetSessionHistory`, `DeleteSession`, `ListWorkspaces`.
- Skill dirs / memory files / `CompressCommand`.
- `usage.go` quota HTTP endpoint (net/http), `fetchModelsFromAPI` (net/http).
- Image/file attachment staging (the runtime contract carries text content only).
- IM-facing assistant-message phase metadata.
