# Source / provenance

The Claude Code adapter in this directory is a **selective migration** of the
mature, proven Claude Code protocol code that already lived in this repository
at `agent/claudecode/` — which itself originated in **cc-connect** (MIT License
© chenhg5, see repo `LICENSE`).

## Upstream baseline

- Upstream project: cc-connect (https://github.com/chenhg5/cc-connect), MIT License.
- In-repo migration baseline: `agent/claudecode/` as of commit
  `cd7e24b033904ef2acc36a1ad4064daf3db386c4` (2026-08-07), the HEAD of the
  gateway-rewrite work when this task ran. The rewrite Tasks 1–5 landed on
  `main` before this adapter was extracted.
- Migration rule (per the plan's 决策对齐 #3): port the protocol, not the IM
  surface. The old `core`/IM types were replaced with the new `runtime`
  contract; nothing of cc-connect's core, Platform, IM, cron/timer/relay or
  management code was copied.

## Source → new file mapping

| New file                              | Migrated from                                   | What was cut / adapted                                                                                           |
|---------------------------------------|-------------------------------------------------|------------------------------------------------------------------------------------------------------------------|
| `adapters/claudecode/adapter.go`      | `agent/claudecode/claudecode.go`                | `core.Agent`/`RegisterAgent` → `runtime.AgentAdapter`/`runtime.Register`; dropped provider switching + thinking-rewrite proxy, workspace listing, session history, skill/command dirs, memory files, `run_as_user` OS isolation, model-catalog HTTP fetch (net/http), `PermissionModes`/live-mode UI, `WorkspaceAgentOptions` propagation. Added `Describe`/`Start`/`Options` per the runtime contract. |
| `adapters/claudecode/session.go`      | `agent/claudecode/session.go`                   | `core.AgentSession`/`core.Event*` → `runtime.Session`/`runtime.Event`; `Send(prompt, images, files)` → `Send(ctx, runtime.Input)` (text only, image base64 staging cut); `EventPermissionRequest` → canonical `runtime.EventPermission` (always emitted, then phase-1 auto-approve/deny response); `EventResult` → `EventFinish` + `EventUsage`; compaction subtype handling preserved (#481); added `Abort(ctx)`; shared cc-connect system prompt file machinery cut (per-spawn append file kept). |
| `adapters/claudecode/protocol.go`     | `agent/claudecode/claudecode.go` + `session.go` helpers | Pure helpers: `normalizePermissionMode`, `normalizeEffort`, `isCompactionResult`/`resultSubtype`, `parseClaudeUsage`, `summarizeInput`, `isClaudeEditTool`, `filterEnv`, `truncateStr`, `writeTempAppendPromptFile`, `buildClaudeArgs`, `mergeEnv`/`redactArgs`. `core.*` deps cut. |
| `adapters/claudecode/proc_unix.go`    | `agent/claudecode/proc_unix.go`                 | Verbatim port (prepareCmdForKill, signalProcessGroup, forceKillCmd).                                              |
| `adapters/claudecode/proc_windows.go` | `agent/claudecode/proc_windows.go`              | Verbatim port.                                                                                                    |
| `adapters/claudecode/adapter_test.go` | `agent/claudecode/session_test.go`, `claudecode_test.go` | Adapted to the `runtime` contract (fake-claude scripts, `runtime.Input` sends, `EventFinish`/`EventUsage` instead of `core.EventResult`, canonical `EventPermission` assertions). Dropped IM/provider/management tests: run_as_user spawn, workspace agent options, session listing/history/delete/validate, `scanSessionMeta`, `encodeClaudeProjectKey`/`findProjectDir`, `SetLiveMode`, `SetWorkDir/SetModel/SetPlatformPrompt`, `parseUserQuestions`, `PermissionModes`. |

## Old `core`/IM dependencies cut

Every `github.com/tianlinzz/agent-cli-gateway/core` import is gone; the package
imports stdlib + `github.com/tianlinzz/agent-cli-gateway/runtime` only. No
`net/http`, no `server/`, no Platform/IM types. Specific `core.*` symbols that
no longer appear: `core.Agent`, `core.AgentSession`, `core.Event*`,
`core.ContextUsage`, `core.ProviderConfig`, `core.ProviderProxy`,
`core.ModelOption`, `core.ParseCmdOpts`, `core.AgentSystemPrompt`,
`core.MergeEnv`, `core.RedactArgs`, `core.RedactEnv`, `core.SaveFilesToDisk`,
`core.SpawnOptions`, `core.BuildSpawnCommand`, `core.VerifyRunAsUserCheap`,
`core.RunAsChdirEnv`, `core.FilterEnvForSpawn`, `core.ContinueSession`,
`core.PermissionModeInfo`, `core.HistoryEntry`, `core.AgentSessionInfo`,
`core.AgentWorkspaceInfo`, `core.UserQuestion`, `core.WorkspaceLister`,
`core.SessionIDValidator`, `core.ResumeCommander`.

## Deliberately NOT migrated (YAGNI, deleted in task 7)

- Provider switching / thinking-rewrite reverse proxy (net/http).
- `live_config.go` / `mcp_config.go` config-file HTTP machinery.
- `cc_hooks.go` (Claude Code PermissionRequest settings.json hook runner — cc-connect
  platform/IM decision layer; the canonical permission event path replaces it).
- Workspace listing, session history, `DeleteSession`, `ListWorkspaces`,
  `ValidateSessionID` (#599), `GetSessionHistory`.
- Skill dirs, command dirs, memory files (`CLAUDE.md`), `/compact`.
- `run_as_user` OS-isolation spawning (`core/runas.go`).
- Model catalog HTTP fetch (`fetchModelsFromAPI`, net/http).
- The shared cc-connect system prompt file (`ensureSharedSystemPromptFile`),
  platform formatting prompts, AskUserQuestion structured UI.
- Image/file attachment staging (the runtime contract carries text content only).
