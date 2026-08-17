# Changelog

## Unreleased

### Added
- **Unified turn-deadline contract (O-F09b)**: `server.turn_timeout` (default
  10m, `0` = disabled) plus `agents.<id>.timeout` now bound every agent's
  turns. The deadline is enforced at the worker boundary (the adapter bridge
  aborts the native turn, then reports a settled `finish(timeout)` or an
  unsettled sentinel) and presented uniformly by the API: HTTP 504 with error
  code `turn_timeout` for non-streaming, an SSE error frame with the same code
  for streaming. The old inconsistency (HTTP 500 vs a fake
  `finish_reason:"length"` completion) is gone — a deadline is never presented
  as a token-length limit. Kimi's independent 30-minute native prompt timeout
  was removed; its abort/close settle budget is now `CloseTimeout` (8s
  default), like Codex.
- **Typed adapter configuration (O-F08)**: deployment config reaches adapter
  factories directly as `runtime.AdapterConfig` (execution config + worker
  placement). The `CC_GATEWAY_*` environment-variable roundtrip is deleted and
  permanently forbidden by an arch test. `agents.<id>.env` now flows typed into
  the CLI child environment without mutating the worker process environment.
- **Argv command form (O-F14)**: `agents.<id>.command` is an argv array
  (`command = ["/opt/agent tools/codex", "--flag"]`) executed verbatim — no
  shell, no whitespace re-tokenization; paths with spaces are expressible. The
  legacy single-string form is still accepted for one deprecation window and is
  interpreted as ONE executable path (the whole string becomes `argv[0]`, never
  shell syntax — arguments must migrate to the array form); it is logged with a
  deprecation warning and will be rejected in a future release.
- `limits.worker_log_check_interval` is now a real, wired setting (it was
  previously documented but could not be parsed).

### Fixed (code review round)
- Deadline watch is now armed only for a turn the native session ACCEPTED: a
  failed Send rolls the watch back atomically, so the timer can neither abort
  an idle session nor inject a stale timeout terminal into the next turn's
  stream.
- A deadline abort that FAILS (e.g. it escalated to a process kill) now always
  reports the unknown-outcome sentinel, even when a native terminal still
  arrives — an unconfirmed interrupt is never recorded as a settled
  `RunTimedOut` (the Phase 2 terminal-state rule). The synthesized
  finish(timeout) requires abort success AND a drained terminal.
- The legacy single-string `command` is interpreted as ONE executable path
  (roadmap Phase 3.3), not shell syntax: the shell-like splitter is gone, and
  `"codex --flag"` names one (deliberately unresolvable) executable —
  arguments must migrate to the argv array form.
- `CONTRIBUTING.md` rewritten for the Agent Gateway (it still described
  contributing to cc-connect, with upstream issue/PR/community links); the
  pre-rewrite changelog moved to `docs/CHANGELOG-cc-connect-archive.md` with a
  provenance header, and the architecture test now rejects IM-era keywords in
  contributor-facing documents.

### Changed
- **Shared native event contract (D1)**: the three identical per-agent event
  type copies collapsed into `agent/events`; each agent re-exports it via type
  aliases. Enforced by the architecture test (agents may import only
  agent/events, agent/process, agent/protocol, agent/rpcsession, stdlib).
- **Shared adapter bridge (D3)**: the contractually identical adapter session
  wrappers (event mapping, prompt assembly, resume-ID lookup) live in
  `adapters/internal/bridge`. This also fixes Claude Code's wrapper missing the
  Send mutex. Resume targets resolve exclusively from the server-owned
  `native_session_id`.
- **Shared JSON-RPC session lifecycle (D2)**: codex and kimi embed
  `agent/rpcsession.Core` (process, monitor/teardown, event channel, abort
  escalation, Close). Converged drifted behaviors: an aborted turn always ends
  with `finish("cancelled")` and a post-abort native failure is logged, never
  emitted as a run error; kimi's `usage_update` notifications are classified
  critical (never dropped under backpressure), matching codex.
- **Dead code/config cleanup (U2)**: removed `isolation.profile_override`,
  `isolation.seccomp.profile_file` (now rejected as unknown keys), codex/kimi
  `Mode` options (never consumed), unused codex preamble helpers, and the
  never-populated `WorkerArgs` plumbing. Token comparison now hashes before the
  constant-time compare so token length cannot leak. `Supervisor.Close`
  aggregates session-close errors instead of returning only the first.

### Added (previous)
- **Worker lifecycle governance**: split HTTP drain and Worker stop timeouts,
  retain persistent Agent CLI processes across normal turns, reclaim only idle
  processes with active-turn exemption, detect unhealthy Workers with bounded
  heartbeats, and run Tini as the container PID 1. The removed
  `server.shutdown_timeout` key is rejected; use `server.drain_timeout`,
  `worker.stop_grace_period`, and `[sessions]` lifecycle settings.
