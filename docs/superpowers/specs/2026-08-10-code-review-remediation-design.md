# Agent Gateway Code Review Remediation Design

**Date:** 2026-08-10

**Status:** Approved architecture, pending implementation

## 1. Goal

Resolve every release-blocking issue found in the post-rewrite code review and
make the single-node gateway suitable for cloud deployment. The result keeps
the OpenAI-compatible API process separate from per-session Agent workers,
uses nsjail as a real filesystem security boundary, and supports Claude Code,
Codex, and Kimi through one canonical runtime contract.

This remediation intentionally does not add a user system, management UI,
distributed scheduling, a durable job queue, JWT/OIDC, remote token
introspection, or automatic Git worktree management.

## 2. Final Tenancy Contract

The gateway has callers and workspaces, not users.

- A Bearer API key authenticates one configured caller.
- `X-Workspace-Id` selects an opaque workspace owned by that caller.
- The security and storage key is `(caller_id, workspace_id)`.
- `X-User-Id` is removed from the authorization contract.
- A caller decides whether one workspace represents a user, project, team,
  task, Git worktree, or another business concept.
- Gateway API keys are server-to-server credentials and must not be exposed to
  untrusted end users.

Configuration supports key rotation without duplicating caller records:

```toml
[auth]
required = true

[[auth.callers]]
id = "f1-web"
tokens = ["gw-f1-web-current", "gw-f1-web-next"]

[[auth.callers]]
id = "internal-cli"
tokens = ["gw-internal-cli"]
```

Raw tokens are never stored in session records, returned by APIs, or written
to logs. Startup validation rejects empty caller IDs, empty token lists,
duplicate caller IDs, duplicate tokens, and an authentication-disabled
production configuration.

After authentication the API places this value in request context:

```go
type Caller struct {
	ID string
}
```

Every session operation, including creation, continuation, abort, deletion,
and recovery, is authorized against `SessionRecord.CallerID`.

## 3. Workspace and Concurrency Semantics

Workspace paths are derived only on the server. The resolver maps
`(caller_id, workspace_id)` to non-reversible, fixed-format directory names
under the configured root. External identifiers never become literal path
segments.

Conceptual layout:

```text
/srv/workspaces/
  callers/<caller-hash>/
    workspaces/<workspace-hash>/
```

Concurrency rules are deliberately narrow:

- One session accepts at most one active turn. Concurrent turns on the same
  session return `409 session_busy`.
- Different sessions may run concurrently against the same workspace. This is
  equivalent to running multiple Agent CLIs in separate terminals in one
  working tree.
- Each session has a distinct Worker process, Unix socket, Agent home, `/tmp`,
  and native Agent session.
- The shared workspace is intentionally the only writable resource shared by
  those sessions.
- The gateway does not serialize workspace writes or resolve Git/file
  conflicts. Callers that need stronger isolation allocate different
  workspace IDs or prepare separate Git worktrees.

Agent-level `max_concurrency` limits resource consumption but does not create a
workspace lock.

## 4. Sandbox Filesystem Boundary

The current mount namespace is insufficient because the container root remains
visible. The remediated profile gives every session a private root filesystem.

The runtime image contains an immutable Agent rootfs at a fixed path. It
includes only the runtime files required by `gateway-worker`, the enabled Agent
CLIs, shells, Git, certificates, and their shared libraries. The supervisor
creates a per-session root directory and constructs an nsjail profile that:

- enables a new mount/root namespace;
- uses the immutable Agent rootfs as the jail root;
- mounts system/runtime content read-only;
- bind-mounts only the current workspace at `/workspace`;
- bind-mounts only the current session Agent home at `/agent-home`;
- bind-mounts only the current session socket directory;
- creates a private tmpfs at `/tmp`;
- does not expose `/gateway-run`, `/etc/gateway`, the host workspace root, or
  sibling session directories;
- runs the Worker and Agent CLI as the configured unprivileged UID/GID;
- keeps provider network access while applying the generated seccomp policy.

The supervisor creates and owns runtime directories with explicit ownership
matching the jail UID/GID. It fails closed if ownership cannot be established.
The configuration file and all secret-bearing files are mode `0600`.

The sandbox test must prove negative properties, not only execute
`/bin/true`: a jailed process must be unable to read a sentinel placed in the
container root, a sibling session home, or the gateway configuration path.

## 5. nsjail Supply Chain and Deployment Gate

nsjail is pinned to a real upstream commit SHA. The Docker build records that
SHA in an image label and a root-owned read-only version file. Runtime
preflight validates the expected SHA using that file; it does not call the
unsupported `nsjail --version` option.

The Docker and CI path must validate:

1. the default image builds without build arguments;
2. the nsjail binary has no missing dynamic libraries;
3. the configured version metadata matches the built commit;
4. a minimal jail starts under the documented container security context;
5. the generated production profile hides container-root and sibling-session
   sentinels;
6. the jailed UID can access its own workspace, Agent home, and socket but not
   another session's private paths.

The Docker/nsjail job is a required CI job. Network fetches may use bounded
retry inside the build, but failures must fail the workflow rather than being
marked advisory.

## 6. Agent Configuration Boundary

Agent configuration is selected by the API/supervisor from trusted TOML. It is
never accepted from OpenAI request metadata.

The canonical internal execution configuration contains:

```go
type AgentExecutionConfig struct {
	Command        string
	DefaultModel   string
	Permission     string
	TurnTimeout    time.Duration
	MaxConcurrency int
	Env            map[string]string
}
```

The gateway resolves the configuration by public model ID and includes the
validated values in the internal `StartSession` RPC. The Worker constructs the
adapter with those values and passes `Env` only to the Agent CLI child process.
Arbitrary process environment variables are not inherited through nsjail.

Secrets may be supplied in TOML through environment references, but resolved
secret values are never logged, returned, or persisted in session metadata.
Unknown fields, invalid permission modes, invalid commands, negative timeouts,
and negative concurrency values fail startup validation.

`max_concurrency` is enforced per Agent in the supervisor. A failed start does
not consume a slot; Worker exit and session close release the slot exactly
once.

## 7. Abort Semantics

The public abort contract means the current turn stops executing tools and
commands before the session becomes idle.

- Codex and Kimi retain process-group cancellation for their per-turn child
  processes.
- Claude Code abort terminates the persistent Claude CLI process group because
  its stream-json input protocol has no reliable in-process turn cancellation.
- Abort does not delete the gateway session or workspace.
- A later turn starts a new Claude process with the previously persisted native
  Claude session ID.
- The API does not call `EndTurn` until the Worker confirms abort completion or
  the Worker/process has been forcefully reaped.
- Client disconnect, explicit abort, API turn timeout, and shutdown use the
  same bounded cancellation path.

The endpoint returns success only after execution has stopped. Timeout or
reaping failure returns an error and marks the handle unusable; it must not
pretend the session is idle while old commands continue running.

## 8. Native Session Recovery

Native Agent session IDs are first-class internal state.

The canonical event/RPC contract gains a native-session update event containing
the opaque native ID. Adapters emit it whenever Claude, Codex, or Kimi first
discovers or changes its native session ID. The Worker forwards it to the
supervisor/API, which persists it in `SessionRecord.NativeSessionID`.

When a Worker is restarted after an idle crash, abort, or dead handle, the
gateway rebuilds the adapter-specific resume metadata from the persisted native
ID and model ID. Client-provided metadata cannot set or override native resume
IDs.

The first implementation remains single-node with an in-memory session store,
but all persistence mutations go through the `SessionStore` interface so a
future durable store and distributed scheduler can replace it without changing
the API contract.

## 9. Model Discovery and Error Semantics

The model catalog and chat validation use the same availability result. A model
is callable only if it is registered, enabled, configured, and its adapter
`Describe` check succeeds.

- `/v1/models` lists exactly the callable Agents.
- Requests for unavailable Agents return OpenAI-compatible
  `404 model_not_found`, not a later internal Worker start error.
- Availability checks use a bounded cache to avoid spawning CLI discovery on
  every request, with explicit invalidation at process start/config reload.

The runtime image is a base gateway image. Deployments must install the Agent
CLIs they enable; readiness reports unavailable Agents without making healthy
installed Agents unusable.

## 10. Error Handling and Logging

All new errors wrap their subsystem and operation. Security failures are
fail-closed. Logs may include caller ID, workspace hash, gateway session ID,
model ID, Worker ID, and lifecycle state, but never raw API keys, provider
credentials, native transcript contents, or full external workspace IDs.

Public errors distinguish:

- `401 invalid_api_key`;
- `400 workspace_id_required` or invalid workspace ID;
- `404 session_not_found` and `model_not_found`;
- `409 session_busy` and Agent concurrency exhaustion;
- `500 worker_start_failed` or `abort_failed` without secret-bearing internal
  details.

## 11. Testing and Release Criteria

Every behavior change is developed with a failing regression test first.

Required automated coverage:

- API key to caller authentication, rotation, duplicate validation, and prod
  fail-closed behavior;
- caller-scoped session authorization and same-named workspace separation;
- same-session turn serialization;
- same-workspace, different-session parallel execution;
- Agent configuration RPC propagation and rejection of client overrides;
- per-Agent concurrency slot acquisition and release;
- Claude abort process termination and native resume on the next turn;
- native ID propagation for all three adapters and dead-Worker recovery;
- consistent `/v1/models` and chat availability behavior;
- generated nsjail profile mount/root assertions;
- Linux container smoke tests for rootfs hiding, sibling isolation, directory
  ownership, socket access, seccomp, and signal propagation.

Release verification commands include:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
docker build -t agent-gateway:verify .
docker run --rm --entrypoint /usr/local/bin/nsjail-smoke agent-gateway:verify
```

The release is blocked if any required Docker/nsjail validation cannot run in
the supported Linux CI environment.

## 12. Migration Policy

This rewrite has no backward-compatibility requirement. Existing
`X-User-Id`-based authorization and single shared-token configuration are
removed instead of deprecated. Configuration errors fail at startup with an
actionable message. Documentation and examples describe only the caller-scoped
workspace contract.
