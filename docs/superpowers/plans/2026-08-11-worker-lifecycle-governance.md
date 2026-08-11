# Worker Lifecycle Governance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Separate graceful shutdown from persistent-session retention and add bounded idle reclamation, Worker health monitoring, and container PID 1 process reaping.

**Architecture:** The API keeps the Gateway session and native session ID as durable conversation identity, while the local Worker Supervisor owns disposable Worker/Agent processes. Idle or unhealthy processes are closed and removed; a later turn follows the existing native-session recovery path. Tini handles container-level signal forwarding and orphan reaping, while nsjail and the Supervisor retain sandbox and session-aware lifecycle ownership.

**Tech Stack:** Go 1.25, gRPC over Unix sockets, nsjail, Docker, Tini, TOML.

---

### Task 1: Split Lifecycle Configuration

**Files:**
- Modify: `config/gateway.go`
- Modify: `config/gateway_test.go`
- Modify: `config.example.toml`
- Modify: `scripts/dev.sh`
- Modify: `docker/entrypoint.sh`
- Modify: `README.md`

- [x] Add failing configuration tests for `server.drain_timeout`, `worker.stop_grace_period`, Worker heartbeat settings, and session idle/reaper settings.
- [x] Run `go test ./config -run 'TestDefaultGatewayConfig_Lifecycle|TestLoadGateway_Lifecycle' -count=1` and verify the new fields are missing.
- [x] Add the new configuration structs, defaults, TOML tags, and validation. Remove `server.shutdown_timeout` without a compatibility alias.
- [x] Update checked-in example and generated development/container configurations.
- [x] Run `go test ./config -count=1` and verify it passes.

### Task 2: Reclaim Only Idle Worker Processes

**Files:**
- Modify: `worker/supervisor.go`
- Modify: `worker/supervisor_test.go`

- [x] Add regression tests proving an idle Worker is reclaimed after its TTL, an active turn is exempt, and a normal completed turn remains alive before the TTL.
- [x] Run the focused tests and verify they fail because the Supervisor has no idle reaper.
- [x] Track per-Worker turn activity and last-idle time from successful sends and terminal events.
- [x] Add a Supervisor-owned reaper loop that closes only idle Workers and stops cleanly during Supervisor shutdown.
- [x] Run `go test ./worker -count=1` and verify it passes.

### Task 3: Detect Unhealthy Workers

**Files:**
- Modify: `worker/supervisor.go`
- Modify: `worker/supervisor_test.go`
- Modify: `worker/testworker/main.go`

- [x] Add a regression test where post-handshake health RPCs fail consecutively and the Supervisor publishes terminal state only at the configured threshold.
- [x] Run the focused test and verify it fails because no runtime heartbeat exists.
- [x] Add a bounded heartbeat loop per Worker; reset failure count on success and terminate after the configured consecutive-failure threshold.
- [x] Ensure heartbeat shutdown is tied to the existing Worker stopping signal and cannot outlive session teardown.
- [x] Run `go test ./worker -count=1` and verify it passes.

### Task 4: Wire Gateway Shutdown and Recovery Semantics

**Files:**
- Modify: `cmd/gateway/main.go`
- Modify: `api/openai/handlers_test.go`
- Modify: `README.md`

- [x] Add or extend an OpenAI regression test proving a reaped handle is transparently recreated on the next request with the saved native session ID.
- [x] Run the focused API test and confirm the recovery contract remains observable.
- [x] Wire `server.drain_timeout` only to HTTP draining, and Worker/session lifecycle settings only to `worker.Config`.
- [x] Document that idle reclamation preserves Gateway conversation identity and never replays an unknown turn.
- [x] Run `go test ./api/openai ./cmd/gateway ./worker -count=1` and verify it passes.

### Task 5: Add Container PID 1 Reaping

**Files:**
- Modify: `Dockerfile`
- Modify: `docker/nsjail-smoke.sh` or create a focused shell check if needed

- [x] Add a static Dockerfile assertion that the runtime image installs and invokes Tini before the entrypoint.
- [x] Run the assertion and verify it fails against the current image definition.
- [x] Install Tini in the runtime image and set it as `ENTRYPOINT`, leaving `docker/entrypoint.sh` responsible only for configuration and `exec` of Gateway.
- [x] Re-run the assertion and Docker/nsjail smoke checks available on the host.

### Task 6: Full Verification

**Files:**
- Modify: `CHANGELOG.md`

- [x] Document the lifecycle configuration break and new behavior.
- [x] Run `gofmt` on changed Go files.
- [x] Run `go test ./...`.
- [x] Run `go test -race ./...`.
- [x] Run `go vet ./...` and `go build ./...`.
- [x] Run `git diff --check` and inspect the final scoped diff.
