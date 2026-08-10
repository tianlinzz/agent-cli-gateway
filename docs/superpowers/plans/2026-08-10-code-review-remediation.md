# Agent Gateway Code Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix every release-blocking review finding and produce a caller-scoped, workspace-isolated, nsjail-enforced Agent Gateway that can be built and verified in Linux CI.

**Architecture:** Bearer keys resolve to trusted caller IDs; callers choose opaque workspace IDs, while the gateway hashes both into controlled paths. The API persists canonical session state, the supervisor owns trusted Agent configuration and process limits, Workers emit native-session updates, and nsjail runs each Worker inside an explicit private root with only its workspace, Agent home, socket, and tmp mounted writable.

**Tech Stack:** Go 1.25, net/http, gRPC/protobuf, nsjail, Docker BuildKit, GitHub Actions, TOML, Go race detector.

---

### Task 1: Replace User Headers With Caller Authentication

**Files:**
- Modify: `config/gateway.go`
- Modify: `config/gateway_test.go`
- Modify: `api/openai/types.go`
- Modify: `api/openai/handlers_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `runtime/session.go`
- Modify: `runtime/session_store.go`
- Modify: `runtime/session_store_test.go`

- [ ] **Step 1: Write failing config tests for caller keys**

Add tests that require production authentication and reject duplicate caller IDs or tokens:

```go
func TestGatewayConfigRejectsProdWithoutCallers(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Auth.Callers = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "auth.callers") {
		t.Fatalf("Validate() = %v, want auth.callers error", err)
	}
}

func TestGatewayConfigRejectsDuplicateCallerToken(t *testing.T) {
	c := DefaultGatewayConfig()
	c.Auth.Callers = []CallerConfig{
		{ID: "a", Tokens: []string{"same"}},
		{ID: "b", Tokens: []string{"same"}},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate token") {
		t.Fatalf("Validate() = %v, want duplicate token error", err)
	}
}
```

- [ ] **Step 2: Run the config tests and verify RED**

Run: `go test ./config -run 'TestGatewayConfigRejectsProdWithoutCallers|TestGatewayConfigRejectsDuplicateCallerToken' -v`

Expected: FAIL because `CallerConfig` and `Auth.Callers` do not exist.

- [ ] **Step 3: Implement caller configuration and validation**

Replace the shared token with:

```go
type AuthConfig struct {
	Required bool           `toml:"required"`
	Callers  []CallerConfig `toml:"callers"`
}

type CallerConfig struct {
	ID     string   `toml:"id"`
	Tokens []string `toml:"tokens"`
}
```

Normalize caller IDs, require callers in prod, and reject duplicate IDs,
duplicate tokens, empty tokens, and empty caller token lists.

- [ ] **Step 4: Write failing API tests for token-derived caller identity**

Add tests proving a token selects the caller and `X-User-Id` cannot change it:

```go
func TestAuthDerivesCallerFromBearerToken(t *testing.T) {
	h, _ := newTestHandler(t, &Options{Callers: map[string]string{"token-a": "caller-a"}})
	req := newCompletionRequest(t, testWorkspace)
	req.Header.Set("Authorization", "Bearer token-a")
	req.Header.Set("X-User-Id", "caller-b")
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	if rr.Code == http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
```

Also test unknown/missing keys return `401 invalid_api_key`, and a caller
cannot continue or abort another caller's session.

- [ ] **Step 5: Run the API tests and verify RED**

Run: `go test ./api/openai -run 'TestAuthDerivesCaller|TestUnknownAPIKey|TestCallerCannot' -v`

Expected: FAIL because auth still uses one shared token and `X-User-Id`.

- [ ] **Step 6: Implement caller auth context and session ownership**

Introduce an internal caller context value:

```go
type Caller struct{ ID string }

func callerFromContext(ctx context.Context) (Caller, bool)
```

Build a constant-time token index in `NewHandler`, authenticate Bearer keys in
middleware, and remove `ownerID(r)`. Rename canonical ownership fields from
`OwnerID` to `CallerID` in runtime, API, worker requests, tests, and logs.

- [ ] **Step 7: Run focused and full tests**

Run: `go test ./config ./runtime ./api/openai ./worker -v`

Expected: PASS.

### Task 2: Hash Caller-Scoped Workspace Paths

**Files:**
- Modify: `workspace/resolver.go`
- Modify: `workspace/resolver_test.go`
- Modify: `worker/supervisor_test.go`

- [ ] **Step 1: Write failing resolver tests**

Assert external IDs are not literal path components, the same pair is stable,
same workspace IDs under different callers differ, and same-workspace sessions
resolve to the same path:

```go
func TestResolveHashesCallerAndWorkspace(t *testing.T) {
	r := newTestResolver(t)
	p, err := r.Resolve("caller-a", "project-secret-name")
	if err != nil { t.Fatal(err) }
	if strings.Contains(p, "caller-a") || strings.Contains(p, "project-secret-name") {
		t.Fatalf("resolved path leaks external identifiers: %s", p)
	}
}
```

- [ ] **Step 2: Run resolver tests and verify RED**

Run: `go test ./workspace -run 'TestResolveHashes|TestResolveScopes|TestResolveStable' -v`

Expected: FAIL because the current resolver uses literal IDs.

- [ ] **Step 3: Implement stable hashed path components**

Use SHA-256 with domain-separated inputs and fixed prefixes:

```go
func pathKey(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "-" + hex.EncodeToString(sum[:16])
}
```

Resolve to `callers/<caller-key>/workspaces/<workspace-key>`, retain symlink
containment checks, and never log raw workspace IDs.

- [ ] **Step 4: Verify same-workspace parallel sessions remain supported**

Add a supervisor/API test that starts two different session IDs with the same
caller/workspace and asserts both Workers start without a workspace lock.

Run: `go test ./workspace ./worker ./api/openai -run 'Workspace|Parallel' -v`

Expected: PASS.

### Task 3: Carry Trusted Agent Configuration Through Worker RPC

**Files:**
- Modify: `runtime/contract.go`
- Modify: `worker/proto/worker.proto`
- Regenerate: `worker/proto/worker.pb.go`
- Regenerate: `worker/proto/worker_grpc.pb.go`
- Modify: `worker/rpc.go`
- Modify: `cmd/gateway-worker/main.go`
- Modify: `worker/supervisor.go`
- Modify: `worker/supervisor_test.go`
- Modify: `cmd/gateway/main.go`
- Modify: `adapters/claudecode/adapter.go`
- Modify: `adapters/codex/adapter.go`
- Modify: `adapters/kimi/adapter.go`

- [ ] **Step 1: Write failing runtime/RPC round-trip tests**

Add `AgentExecutionConfig` to `StartRequest` and test command, model,
permission, timeout, and env survive conversion to/from protobuf while client
metadata cannot override them.

```go
want := runtime.AgentExecutionConfig{
	Command: "/opt/bin/codex", DefaultModel: "gpt-x", Permission: "deny",
	TurnTimeout: 45 * time.Second, MaxConcurrency: 2,
	Env: map[string]string{"OPENAI_API_KEY": "secret"},
}
```

- [ ] **Step 2: Run worker RPC tests and verify RED**

Run: `go test ./worker -run 'AgentExecutionConfig|StartRequestRoundTrip' -v`

Expected: FAIL because the contract has no execution config.

- [ ] **Step 3: Extend protobuf and regenerate stubs**

Add:

```proto
message AgentExecutionConfig {
  string command = 1;
  string default_model = 2;
  string permission = 3;
  int64 turn_timeout_nanos = 4;
  int32 max_concurrency = 5;
  map<string, string> env = 6;
}
```

Reference it from `StartSessionRequest`, then run `make generate`.

- [ ] **Step 4: Build adapters from trusted options**

Change adapter factories to accept runtime execution options instead of reading
`CC_GATEWAY_*` process environment. CLI child commands receive only the
configured environment plus a minimal platform baseline.

- [ ] **Step 5: Add failing supervisor concurrency tests**

For `max_concurrency=1`, assert the second session for the same Agent returns a
typed busy error and a slot is released after close, crash, and failed start.

- [ ] **Step 6: Implement per-Agent supervisor limiters**

Use a mutex-guarded counter keyed by model ID and a per-session `sync.Once`
release function. Do not lock by workspace.

- [ ] **Step 7: Verify configuration flow**

Run: `go test ./config ./runtime ./worker ./adapters/... -v`

Expected: PASS.

### Task 4: Propagate and Persist Native Session IDs

**Files:**
- Modify: `runtime/events.go`
- Modify: `worker/proto/worker.proto`
- Regenerate: `worker/proto/worker.pb.go`
- Regenerate: `worker/proto/worker_grpc.pb.go`
- Modify: `worker/rpc.go`
- Modify: `cmd/gateway-worker/main.go`
- Modify: `api/openai/chat_completions.go`
- Modify: `api/openai/handlers_test.go`
- Modify: all three adapter sessions and tests

- [ ] **Step 1: Write failing event/RPC tests**

Add a canonical event:

```go
const EventNativeSession EventType = "native_session"

type Event struct {
	// existing fields
	NativeSessionID string
}
```

Test protobuf round-trip and adapter emission when native IDs are discovered.

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./runtime ./worker ./adapters/... -run 'NativeSession' -v`

Expected: FAIL because no event exists.

- [ ] **Step 3: Implement adapter emission and RPC forwarding**

Emit only when a non-empty ID changes. Never include native IDs in public SSE
or completion payloads.

- [ ] **Step 4: Write failing API recovery test**

Simulate a native-session event, mark the handle dead, send the next turn, and
assert the restarted `StartRequest.Metadata` contains the adapter-specific
resume key derived from the stored native ID rather than client metadata.

- [ ] **Step 5: Implement store update and trusted resume metadata**

Consume native-session events alongside normal turn events, call
`SessionStore.Update`, and rebuild `claude_session_id`, `codex_thread_id`, or
`kimi_session_id` based on the stored model ID.

- [ ] **Step 6: Verify recovery tests**

Run: `go test ./api/openai ./worker ./adapters/... -run 'NativeSession|DeadHandle|Resume' -v`

Expected: PASS.

### Task 5: Make Abort Stop Claude Execution

**Files:**
- Modify: `adapters/claudecode/session.go`
- Modify: `adapters/claudecode/adapter_test.go`
- Modify: `cmd/gateway-worker/main.go`
- Modify: `worker/rpc.go`
- Modify: `api/openai/chat_completions.go`
- Modify: `api/openai/handlers_test.go`

- [ ] **Step 1: Write a failing Claude process regression test**

Start a fake Claude CLI that launches a long-running child and records a
sentinel if allowed to complete. Call `Abort`, wait for it to return, and assert
the process group is gone and the sentinel is absent.

- [ ] **Step 2: Run the test and verify RED**

Run: `go test ./adapters/claudecode -run TestAbortTerminatesInFlightProcessGroup -v`

Expected: FAIL because `Abort` only sets `turnAborted`.

- [ ] **Step 3: Implement destructive turn abort with resumable session state**

Terminate the Claude process group, wait with bounded TERM/KILL escalation,
close the old event stream, and mark the execution handle dead. Preserve the
native session ID through Task 4 so the API restarts on the next turn.

- [ ] **Step 4: Write failing API abort truthfulness tests**

Test that `POST /v1/sessions/{id}/abort` does not return success until the
handle confirms termination, and returns `500 abort_failed` when abort fails.

- [ ] **Step 5: Implement truthful API/Worker abort propagation**

Return abort errors through gRPC, drop failed/dead handles, and call `EndTurn`
only after execution has settled.

- [ ] **Step 6: Verify abort paths**

Run: `go test ./adapters/claudecode ./api/openai ./worker -run 'Abort|Disconnect|Timeout' -v`

Expected: PASS.

### Task 6: Build a Private nsjail Root and Correct Ownership

**Files:**
- Modify: `config/gateway.go`
- Modify: `config/gateway_test.go`
- Modify: `worker/nsjail/profile.go`
- Modify: `worker/nsjail/profile_test.go`
- Modify: `worker/nsjail/preflight_linux.go`
- Modify: `worker/supervisor.go`
- Modify: `worker/supervisor_test.go`
- Modify: `Dockerfile`
- Modify: `docker/entrypoint.sh`
- Modify/Create: `docker/nsjail-smoke.sh`

- [ ] **Step 1: Write failing profile tests for root isolation**

Require a non-empty rootfs path, a private root/chroot declaration, read-only
runtime mounts, and exactly the current workspace/home/socket writable mounts.
Assert the generated config does not bind `/`, `/gateway-run`, or the workspace
root.

- [ ] **Step 2: Run profile tests and verify RED**

Run: `go test ./worker/nsjail -run 'PrivateRoot|HostRoot|WritableMounts' -v`

Expected: FAIL because the host root remains visible.

- [ ] **Step 3: Implement private-root profile assembly**

Add `RootFSDir` to isolation/session layout, configure nsjail chroot/root
namespacing using a prebuilt immutable rootfs, and mount only session-specific
writable paths. Retain shared provider networking and fail-closed seccomp.

- [ ] **Step 4: Write failing ownership tests**

Run the supervisor as a different UID from the jail UID and assert workspace,
Agent home, and socket ownership permits the jailed UID while sibling paths do
not.

- [ ] **Step 5: Implement explicit ownership and secret modes**

Create directories with `0700`, chown the current workspace/session resources
to the configured jail UID/GID, create profile/config/version files with
`0600` or `0444` as appropriate, and fail on ownership errors.

- [ ] **Step 6: Add Linux negative-isolation smoke**

The smoke script creates current and sibling sentinels, launches a generated
profile, verifies current workspace read/write succeeds, and verifies container
root, gateway config, and sibling sentinels are absent or unreadable.

- [ ] **Step 7: Verify Go sandbox tests**

Run: `go test ./worker/nsjail ./worker -v`

Expected: PASS.

### Task 7: Repair nsjail Pinning, Docker Build, and CI Gate

**Files:**
- Modify: `Dockerfile`
- Modify: `config/gateway.go`
- Modify: `config.example.toml`
- Modify: `docker/entrypoint.sh`
- Modify: `docker/nsjail-smoke.sh`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 1: Pin a real upstream commit**

Set one real `NSJAIL_REF` commit SHA in Docker and config examples. Write it to
`/usr/local/share/agent-gateway/nsjail-revision` during build and label the
image with the same revision.

- [ ] **Step 2: Replace unsupported version probing**

Change preflight to read and compare the revision file before running the
minimal jail. Remove every `nsjail --version` call.

- [ ] **Step 3: Make CI mandatory**

Remove `continue-on-error`, run Docker build and negative-isolation smoke, and
retain bounded Git/network retry only inside the build step.

- [ ] **Step 4: Verify the default image**

Run:

```bash
docker build -t agent-gateway:verify .
docker run --rm --entrypoint /usr/local/bin/nsjail-smoke agent-gateway:verify
```

Expected: both commands exit 0 on a supported Linux Docker host. If the local
Darwin VM forbids nested user namespaces, preserve the exact error and require
the GitHub Linux job as the release gate.

### Task 8: Align Model Discovery With Invocation

**Files:**
- Modify: `api/openai/models.go`
- Modify: `api/openai/handlers_test.go`

- [ ] **Step 1: Write a failing unavailable-model test**

Register an adapter whose `Describe` fails, verify it is absent from
`/v1/models`, then POST a completion for it and require `404 model_not_found`
without calling the backend.

- [ ] **Step 2: Run the test and verify RED**

Run: `go test ./api/openai -run TestUnavailableModelCannotBeInvoked -v`

Expected: FAIL because `has()` only checks registration/enabled status.

- [ ] **Step 3: Implement one cached availability source**

Cache successful descriptors and discovery failures with a bounded TTL. Both
list and invocation use the same result. Do not cache request cancellation as a
permanent model failure.

- [ ] **Step 4: Verify model behavior**

Run: `go test ./api/openai -run 'Model|Models' -v`

Expected: PASS.

### Task 9: Documentation and Full Release Verification

**Files:**
- Modify: `README.md`
- Modify: `config.example.toml`
- Modify: `docs/superpowers/specs/2026-08-10-code-review-remediation-design.md` only if implementation exposes a confirmed contradiction

- [ ] **Step 1: Update public documentation**

Document caller API keys, workspace ownership, same-workspace concurrency,
server-to-server credential expectations, Agent configuration, native resume,
abort behavior, private-root isolation, required container permissions, and
the real Docker verification command. Remove `X-User-Id` examples and all
claims that the container root remains visible.

- [ ] **Step 2: Run formatting and generated-code checks**

Run:

```bash
gofmt -w $(rg --files api runtime worker adapters config cmd integration workspace -g '*.go')
make generate
git diff --check
```

Expected: clean exit; regenerated protobuf files match the proto source.

- [ ] **Step 3: Run full Go verification**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Expected: all commands exit 0 with no failed packages.

- [ ] **Step 4: Run final container verification**

Run:

```bash
docker build --no-cache -t agent-gateway:release-check .
docker run --rm --entrypoint /usr/local/bin/nsjail-smoke agent-gateway:release-check
```

Expected: image builds and every positive/negative sandbox assertion passes on
the supported Linux runner.

- [ ] **Step 5: Review final diff against all CR findings**

Confirm no production path still uses `OwnerID`, `X-User-Id`, shared auth
tokens, unsupported `nsjail --version`, host-visible jail roots, ignored Agent
configuration, non-terminating Claude abort, or unpersisted native session IDs.
