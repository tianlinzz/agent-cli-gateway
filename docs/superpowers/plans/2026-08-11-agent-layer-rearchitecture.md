# Agent Layer Rearchitecture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the build-tagged cc-connect reference tree and process-heavy adapters with reusable native Claude Code, Codex, and Kimi packages behind thin runtime adapters, while preserving the existing OpenAI API and Worker isolation behavior.

**Architecture:** Native CLI launch, protocol, resume, usage, permission, and abort behavior moves into `agent/<name>` and depends only on `agent/process`, `agent/protocol`, and the standard library. `adapters/<name>` converts native options, inputs, events, and sessions to the canonical `runtime` contract; Worker composition and the OpenAI-compatible API remain unchanged.

**Tech Stack:** Go 1.25, standard-library process APIs, bounded JSONL decoding, gRPC, nsjail, OpenAI HTTP/SSE

---

## File Map

The implementation creates or replaces these focused units:

- `internal/archtest/dependencies_test.go`: parses Go imports and enforces final dependency and source-tree rules.
- `agent/process/process.go`: shared command construction, environment merge, pipes, start, wait, and close primitives.
- `agent/process/process_unix.go`: Unix process-group preparation, graceful signal, and force kill.
- `agent/process/process_windows.go`: Windows process-group preparation and `taskkill` fallbacks.
- `agent/protocol/jsonl.go`: bounded newline-delimited JSON decoder with typed framing errors.
- `agent/claudecode/{options,events,launch,protocol,session}.go`: Claude-native configuration, event types, argv, stream-json parser, persistent lifecycle, permission, resume, usage, and abort.
- `agent/codex/{options,events,launch,protocol,session}.go`: Codex-native configuration, event types, exec JSON protocol, per-turn lifecycle, thread resume, usage, and abort.
- `agent/kimi/{options,events,launch,protocol,probe,session}.go`: Kimi-native configuration, event types, flag probing, stream protocol, per-turn lifecycle, resume, usage, and abort.
- `adapters/<name>/adapter.go`: runtime registration, descriptor, trusted config conversion, and start conversion only.
- `adapters/<name>/session.go`: native input/event/session conversion only.
- `adapters/<name>/adapter_test.go`: adapter mapping and registration tests only.
- `adapters/<name>/SOURCE.md`: upstream commit/file attribution and local modifications.
- `integration/agent_gateway_test.go`: preserved public OpenAI/Worker behavior using fake CLIs.
- `.github/workflows/ci.yml`: architecture, test, race, vet, build, cross-build, and Linux/nsjail gates.
- `README.md`, `AGENTS.md`, `CLAUDE.md`, `.dockerignore`: final architecture and development rules with no migration-reference instructions.

## Stable Native Contract

Each concrete Agent package defines its own `Options`, `Input`, `Event`, `Usage`, `PermissionRequest`, and `Session`. The packages intentionally do not share a speculative Agent interface. They do share these event semantics, with Agent-specific fields allowed where required:

```go
type EventKind string

const (
	EventText          EventKind = "text"
	EventToolUse       EventKind = "tool_use"
	EventToolResult    EventKind = "tool_result"
	EventPermission    EventKind = "permission"
	EventUsage         EventKind = "usage"
	EventError         EventKind = "error"
	EventFinish        EventKind = "finish"
	EventNativeSession EventKind = "native_session"
)

type Input struct {
	Prompt string
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}
```

Adapters are responsible for translating canonical message history into `Input.Prompt`; native packages never import `runtime`.

### Task 1: Establish Characterization Baseline And Architecture Test Harness

**Files:**
- Create: `internal/archtest/dependencies_test.go`
- Create: `internal/archtest/testdata/allowed_legacy.txt`
- Modify: `integration/agent_gateway_test.go`
- Test: `internal/archtest/dependencies_test.go`
- Test: `integration/agent_gateway_test.go`

- [x] **Step 1: Write the dependency scanner and a failing invariant test**

Create a scanner based on `go/parser` and `go/token`. During extraction, `testdata/allowed_legacy.txt` contains the exact current `agent_ref` and removed-`core` files; the test fails on any new violation and the allowlist is deleted in Task 10. Add this invariant immediately so migration cannot add more debt:

```go
func TestDependencyRules(t *testing.T) {
	root := moduleRoot(t)
	imports := scanImports(t, root)
	for file, paths := range imports {
		pkg := filepath.ToSlash(file)
		for _, path := range paths {
			switch {
			case strings.Contains(pkg, "/runtime/"):
				rejectImport(t, file, path, "/agent/", "/adapters/", "/worker/", "/api/")
			case strings.Contains(pkg, "/agent/") && !legacyAllowed(t, root, file):
				rejectImport(t, file, path, "/runtime", "/adapters/", "/worker/", "/api/", "/config")
			case strings.Contains(pkg, "/adapters/"):
				rejectImport(t, file, path, "/worker/", "/api/")
			case strings.Contains(pkg, "/worker/"):
				rejectImport(t, file, path, "/agent/claudecode", "/agent/codex", "/agent/kimi", "/adapters/")
			}
		}
	}
}

func TestNoNewLegacySources(t *testing.T) {
	root := moduleRoot(t)
	allowed := readAllowedLegacy(t, root)
	for _, match := range scanText(t, root, []string{"//go:build agent_ref", "agent-cli-gateway/core"}) {
		if !allowed[match.File] {
			t.Errorf("new legacy source %s contains %q", match.File, match.Needle)
		}
	}
}
```

- [x] **Step 2: Run the architecture test and verify the unlisted violation is RED**

Run:

```bash
go test ./internal/archtest/ -run 'TestDependencyRules|TestNoNewLegacySources' -v
```

Expected: FAIL naming at least one existing legacy file before `allowed_legacy.txt` is populated.

- [x] **Step 3: Freeze the exact legacy allowlist and public behavior**

Populate `allowed_legacy.txt` from the current tracked files only:

```bash
rg -l 'go:build agent_ref|agent-cli-gateway/core' agent | sort > internal/archtest/testdata/allowed_legacy.txt
```

Add integration subtests using the existing fake Worker/CLI harness for `GET /v1/models`, streaming and non-streaming completion, caller-provided session reuse, same-workspace distinct-session concurrency, abort, disconnect, timeout, and Worker crash. Assert HTTP/SSE output and process settlement, never adapter internals.

- [x] **Step 4: Run the baseline gates and verify GREEN**

Run:

```bash
go test ./internal/archtest/ ./integration/ ./api/openai/ ./worker/ -v
go test ./...
```

Expected: PASS; the allowlist permits only the pre-existing legacy files and the public Gateway behavior is frozen.

- [x] **Step 5: Commit the baseline**

```bash
git add internal/archtest integration/agent_gateway_test.go
git commit -m "test: freeze agent gateway architecture baseline"
```

### Task 2: Extract Shared Process Mechanics

**Files:**
- Create: `agent/process/process.go`
- Create: `agent/process/process_unix.go`
- Create: `agent/process/process_windows.go`
- Create: `agent/process/process_test.go`
- Create: `agent/process/process_unix_test.go`
- Test: `agent/process/process_test.go`

- [x] **Step 1: Write failing process lifecycle tests**

Define tests for environment replacement, cwd, stdout/stderr capture, idempotent wait, graceful stop, forced process-group kill, and reaping descendants. The public API exercised by the test is:

```go
func TestProcessStartWaitAndMergedEnvironment(t *testing.T) {
	p, err := Start(context.Background(), Spec{
		Command: testHelperCommand(t, "print-env"),
		Dir:     t.TempDir(),
		Env:     []string{"PROCESS_TEST_VALUE=replaced"},
	})
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	require.Contains(t, p.StdoutString(), "replaced")
}

func TestProcessForceKillReapsProcessGroup(t *testing.T) {
	p, err := Start(context.Background(), Spec{Command: testHelperCommand(t, "spawn-child")})
	require.NoError(t, err)
	require.NoError(t, p.ForceKill())
	require.NoError(t, p.Wait())
	require.Eventually(t, func() bool { return !processGroupExists(p.PID()) }, time.Second, 10*time.Millisecond)
}
```

- [x] **Step 2: Run tests to verify RED**

Run:

```bash
go test ./agent/process/ -v
```

Expected: FAIL because `Spec`, `Start`, and `Process` do not exist.

- [x] **Step 3: Implement the minimum shared process API**

Implement this API without Agent-specific flags or lifecycle policy:

```go
type Spec struct {
	Command []string
	Dir     string
	Env     []string
	Stdin   bool
}

type Process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    bytes.Buffer
	waitOnce  sync.Once
	waitErr   error
}

func Start(ctx context.Context, spec Spec) (*Process, error)
func MergeEnv(base, overrides []string) []string
func (p *Process) Stdin() io.WriteCloser
func (p *Process) Stdout() io.ReadCloser
func (p *Process) StderrString() string
func (p *Process) PID() int
func (p *Process) Wait() error
func (p *Process) SignalGraceful() error
func (p *Process) ForceKill() error
```

`Start` must reject an empty command, set `Dir`, merge `os.Environ()` with deployment overrides, create requested pipes, prepare a process group before `cmd.Start`, and wrap errors with a safe executable/cwd summary. Unix and Windows files implement process-group operations using the already-proven adapter helpers.

- [x] **Step 4: Run focused, race, and cross-build tests**

Run:

```bash
go test ./agent/process/ -v
go test -race ./agent/process/
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/agent-process-linux.test ./agent/process/
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/agent-process-windows.test ./agent/process/
```

Expected: PASS on all commands.

- [x] **Step 5: Commit the process foundation**

```bash
git add agent/process
git commit -m "feat: add shared native agent process lifecycle"
```

### Task 3: Add Bounded JSONL Protocol Framing

**Files:**
- Create: `agent/protocol/jsonl.go`
- Create: `agent/protocol/jsonl_test.go`
- Test: `agent/protocol/jsonl_test.go`

- [x] **Step 1: Write failing framing tests**

```go
func TestDecoderReadsJSONLines(t *testing.T) {
	d := NewJSONLDecoder(strings.NewReader("{\"type\":\"a\"}\n{\"type\":\"b\"}\n"), 1024)
	var got map[string]any
	require.NoError(t, d.Decode(&got))
	require.Equal(t, "a", got["type"])
	require.NoError(t, d.Decode(&got))
	require.Equal(t, "b", got["type"])
}

func TestDecoderRejectsOversizedFrame(t *testing.T) {
	d := NewJSONLDecoder(strings.NewReader("{\"x\":\""+strings.Repeat("a", 128)+"\"}\n"), 64)
	var got map[string]any
	var frameErr *FrameError
	require.ErrorAs(t, d.Decode(&got), &frameErr)
	require.Equal(t, ErrFrameTooLarge, frameErr.Kind)
}
```

- [x] **Step 2: Run tests to verify RED**

Run: `go test ./agent/protocol/ -v`

Expected: FAIL because `NewJSONLDecoder`, `FrameError`, and `ErrFrameTooLarge` do not exist.

- [x] **Step 3: Implement bounded framing only**

```go
type ErrorKind string

const (
	ErrFrameTooLarge ErrorKind = "frame_too_large"
	ErrMalformedJSON ErrorKind = "malformed_json"
)

type FrameError struct {
	Kind ErrorKind
	Err  error
}

type JSONLDecoder struct {
	r       *bufio.Reader
	maxSize int
}

func NewJSONLDecoder(r io.Reader, maxSize int) *JSONLDecoder
func (d *JSONLDecoder) Decode(dst any) error
```

The decoder skips blank lines, returns `io.EOF` cleanly, detects a frame exceeding `maxSize` before unmarshalling, wraps malformed JSON as `FrameError`, and contains no Claude/Codex/Kimi event switch.

- [x] **Step 4: Run focused and race tests**

Run:

```bash
go test ./agent/protocol/ -v
go test -race ./agent/protocol/
```

Expected: PASS.

- [x] **Step 5: Commit framing**

```bash
git add agent/protocol
git commit -m "feat: add bounded native agent protocol framing"
```

### Task 4: Extract Claude Code Native Execution

**Files:**
- Create: `agent/claudecode/options.go`
- Create: `agent/claudecode/events.go`
- Create: `agent/claudecode/launch.go`
- Create: `agent/claudecode/protocol.go`
- Replace: `agent/claudecode/session.go`
- Replace: `agent/claudecode/proc_unix_test.go`
- Create: `agent/claudecode/launch_test.go`
- Create: `agent/claudecode/protocol_test.go`
- Create: `agent/claudecode/session_test.go`
- Test: `agent/claudecode/*_test.go`

- [x] **Step 1: Replace legacy tests with native characterization tests**

Remove the `agent_ref` test files for Claude-only features that are outside the approved scope. Port the proven tests from `adapters/claudecode/adapter_test.go` for argv, stream event ordering, compaction, permission, usage, native session ID, multi-turn persistent stdin, child-held stdout, close, and abort. Tests target this native API:

```go
type Options struct {
	Command            []string
	Env                []string
	WorkDir            string
	Model              string
	ReasoningEffort    string
	ResumeID           string
	Mode               string
	Permission         string
	SystemPrompt       string
	AppendSystemPrompt string
	AllowedTools       []string
	DisallowedTools    []string
	MaxContextTokens   int
}

func Start(ctx context.Context, opts Options) (*Session, error)
func (s *Session) Send(ctx context.Context, input Input) error
func (s *Session) Events() <-chan Event
func (s *Session) Abort(ctx context.Context) error
func (s *Session) Close(ctx context.Context) error
func (s *Session) NativeSessionID() string
```

Include the regression assertion that `BuildArgs` contains exactly one `--disallowedTools AskUserQuestion` entry and that an unexpected native `AskUserQuestion` control request emits a denied permission response without hanging.

- [x] **Step 2: Run Claude native tests to verify RED**

Run:

```bash
go test ./agent/claudecode/ -v
```

Expected: FAIL because the old build-tagged package does not expose the native API.

- [x] **Step 3: Implement Claude-native types and launch/protocol logic**

Define native events without importing runtime:

```go
type Event struct {
	Kind            EventKind
	Text            string
	Tool            *ToolCall
	Permission      *PermissionRequest
	Usage           *Usage
	Err             error
	FinishReason    string
	NativeSessionID string
}
```

Move `buildClaudeArgs`, normalization, redaction, result/usage parsing, assistant/tool parsing, control-response JSON, and append-prompt temp-file ownership from the adapter. Use `agent/protocol.JSONLDecoder` with a 10 MiB bound and `agent/process.Process` for launch/reap. Preserve `--input-format stream-json`, `--output-format stream-json`, `--permission-prompt-tool stdio`, resume, persistent stdin, one terminal native error, event-channel single ownership, and lifecycle-aware abort.

- [x] **Step 4: Run Claude native validation**

Run:

```bash
go test ./agent/claudecode/ -v
go test -race ./agent/claudecode/
go vet ./agent/claudecode/
```

Expected: PASS; `rg 'agent-cli-gateway/(runtime|core)' agent/claudecode` returns no matches.

- [x] **Step 5: Commit the Claude native layer**

```bash
git add agent/claudecode agent/process agent/protocol
git commit -m "feat: extract native claude code execution"
```

### Task 5: Make The Claude Adapter A Thin Runtime Bridge

**Files:**
- Modify: `adapters/claudecode/adapter.go`
- Modify: `adapters/claudecode/session.go`
- Modify: `adapters/claudecode/adapter_test.go`
- Delete: `adapters/claudecode/protocol.go`
- Delete: `adapters/claudecode/proc_unix.go`
- Delete: `adapters/claudecode/proc_windows.go`
- Test: `adapters/claudecode/adapter_test.go`

- [x] **Step 1: Write failing adapter-only mapping tests**

Replace native protocol assertions with mapping tests driven by a fake native session:

```go
func TestSessionMapsNativeEventsToRuntime(t *testing.T) {
	native := newFakeSession(claudecode.Event{Kind: claudecode.EventText, Text: "hello"})
	s := wrapSession(native)
	got := <-s.Events()
	require.Equal(t, runtime.EventText, got.Type)
	require.Equal(t, "hello", got.Text)
}

func TestStartMapsRuntimeRequestToNativeOptions(t *testing.T) {
	starter := &captureStarter{}
	a := newAdapter(testOptions(), starter.Start)
	_, err := a.Start(context.Background(), runtime.StartRequest{
		Metadata: map[string]string{"native_session_id": "native-1"},
	})
	require.NoError(t, err)
	require.Equal(t, "native-1", starter.Options.ResumeID)
}
```

- [x] **Step 2: Run adapter tests to verify RED**

Run: `go test ./adapters/claudecode/ -v`

Expected: FAIL because `wrapSession` and the injectable native starter do not exist.

- [x] **Step 3: Implement runtime conversion and remove native mechanics**

Keep registry setup, descriptor, environment-derived trusted options, prompt conversion, event mapping, and delegation:

```go
type nativeSession interface {
	Send(context.Context, claudecode.Input) error
	Events() <-chan claudecode.Event
	Abort(context.Context) error
	Close(context.Context) error
	NativeSessionID() string
}

type starter func(context.Context, claudecode.Options) (nativeSession, error)

type session struct {
	native nativeSession
	events chan runtime.Event
}

func (s *session) Send(ctx context.Context, in runtime.Input) error {
	return s.native.Send(ctx, claudecode.Input{Prompt: promptFromRuntime(in)})
}
```

Map every native event exactly once, preserve native session IDs on usage/finish/native-session events, and delegate `Abort`/`Close`. Delete process helpers and raw JSON parsing from the adapter package.

- [x] **Step 4: Verify the Claude vertical slice**

Run:

```bash
go test ./agent/claudecode/ ./adapters/claudecode/ ./worker/ ./api/openai/ ./integration/ -v
go test -race ./agent/claudecode/ ./adapters/claudecode/
test -z "$(rg -l 'os/exec|bufio\.Scanner|json\.Unmarshal' adapters/claudecode || true)"
```

Expected: PASS and the final command prints nothing.

- [x] **Step 5: Commit the Claude adapter**

```bash
git add agent/claudecode adapters/claudecode
git commit -m "refactor: reduce claude adapter to runtime mapping"
```

### Task 6: Extract Codex Native Execution

**Files:**
- Create: `agent/codex/options.go`
- Create: `agent/codex/events.go`
- Create: `agent/codex/launch.go`
- Create: `agent/codex/protocol.go`
- Replace: `agent/codex/session.go`
- Create: `agent/codex/launch_test.go`
- Create: `agent/codex/protocol_test.go`
- Replace: `agent/codex/session_test.go`
- Test: `agent/codex/*_test.go`

- [x] **Step 1: Replace legacy tests with Codex native characterization tests**

Port the approved behavior from `adapters/codex/adapter_test.go`: multiline prompt over stdin, fresh and resumed argv, `thread.started` native ID, item event ordering, reasoning/text/tool mappings, last token usage, large JSONL frames, permission events, buffered commentary on abort, per-turn process termination, descendant reaping, and idempotent close. Target:

```go
type Options struct {
	Command            []string
	Env                []string
	WorkDir            string
	Model              string
	ReasoningEffort    string
	Mode               string
	ResumeID           string
	BaseURL            string
	ModelProvider      string
	SystemPrompt       string
	AppendSystemPrompt string
}

func New(opts Options) *Session
func (s *Session) Send(ctx context.Context, input Input) error
func (s *Session) Events() <-chan Event
func (s *Session) Abort(ctx context.Context) error
func (s *Session) Close(ctx context.Context) error
func (s *Session) NativeSessionID() string
```

- [x] **Step 2: Run Codex native tests to verify RED**

Run: `go test ./agent/codex/ -v`

Expected: FAIL because the legacy package does not expose the native API.

- [x] **Step 3: Implement Codex-native exec JSON lifecycle**

Move command splitting, argv construction, prompt writing, rollout usage lookup, event parsing, tracked-process bookkeeping, kill loop, redaction, and resume state from `adapters/codex`. Use the shared process and JSONL packages. Exclude old provider switching, app-server integration, MCP/config editing, history listing, and skill management. Every `Send` starts a fresh process and uses the learned native thread ID on the next turn.

- [x] **Step 4: Run Codex native validation**

Run:

```bash
go test ./agent/codex/ -v
go test -race ./agent/codex/
go vet ./agent/codex/
```

Expected: PASS; `rg 'agent-cli-gateway/(runtime|core)' agent/codex` returns no matches.

- [x] **Step 5: Commit the Codex native layer**

```bash
git add agent/codex agent/process agent/protocol
git commit -m "feat: extract native codex execution"
```

### Task 7: Make The Codex Adapter A Thin Runtime Bridge

**Files:**
- Modify: `adapters/codex/adapter.go`
- Modify: `adapters/codex/session.go`
- Modify: `adapters/codex/adapter_test.go`
- Delete: `adapters/codex/protocol.go`
- Delete: `adapters/codex/proc_unix.go`
- Delete: `adapters/codex/proc_windows.go`
- Test: `adapters/codex/adapter_test.go`

- [x] **Step 1: Write failing mapping and lifecycle delegation tests**

Use a fake Codex native session implementing an adapter-private `nativeSession` interface (the same method set defined explicitly in Task 5, with Codex-native `Input` and `Event` types) to assert runtime input-to-prompt conversion; text, tool, permission, usage, error, finish, and native-session event conversion; resume metadata; and exact delegation of `Abort` and `Close`.

```go
func TestSessionDelegatesAbortAndClose(t *testing.T) {
	native := &fakeNativeSession{}
	s := wrapSession(native)
	require.NoError(t, s.Abort(context.Background()))
	require.NoError(t, s.Close(context.Background()))
	require.Equal(t, 1, native.abortCalls)
	require.Equal(t, 1, native.closeCalls)
}
```

- [x] **Step 2: Run adapter tests to verify RED**

Run: `go test ./adapters/codex/ -v`

Expected: FAIL because the adapter still owns the concrete process session.

- [x] **Step 3: Implement the thin Codex bridge and delete native code**

Construct `codex.Options` from deployment-owned adapter options, translate `runtime.StartRequest.Metadata["native_session_id"]`, convert runtime messages to a native prompt, map the native event stream, and delegate lifecycle methods. Reject unsupported configured backend values during trusted adapter construction, not in the native protocol parser.

- [x] **Step 4: Verify the Codex vertical slice**

Run:

```bash
go test ./agent/codex/ ./adapters/codex/ ./worker/ ./api/openai/ ./integration/ -v
go test -race ./agent/codex/ ./adapters/codex/
test -z "$(rg -l 'os/exec|bufio\.Scanner|json\.Unmarshal' adapters/codex || true)"
```

Expected: PASS and no native process/protocol match below the adapter.

- [x] **Step 5: Commit the Codex adapter**

```bash
git add agent/codex adapters/codex
git commit -m "refactor: reduce codex adapter to runtime mapping"
```

### Task 8: Extract Kimi Native Execution

**Files:**
- Create: `agent/kimi/options.go`
- Create: `agent/kimi/events.go`
- Create: `agent/kimi/launch.go`
- Create: `agent/kimi/protocol.go`
- Replace: `agent/kimi/probe.go`
- Replace: `agent/kimi/session.go`
- Create: `agent/kimi/launch_test.go`
- Create: `agent/kimi/protocol_test.go`
- Replace: `agent/kimi/probe_test.go`
- Replace: `agent/kimi/session_test.go`
- Test: `agent/kimi/*_test.go`

- [x] **Step 1: Replace legacy tests with Kimi native characterization tests**

Port the approved tests from `adapters/kimi/adapter_test.go`: modern/legacy help probing, conditional `--print`, plan/quiet modes, fresh and resumed argv, native session extraction from stdout and stderr, assistant thinking/text/tool ordering, tool results, usage, process failure, per-turn abort, and close. Target:

```go
type Options struct {
	Command  []string
	Env      []string
	WorkDir  string
	Model    string
	Mode     string
	ResumeID string
	Timeout  time.Duration
	Flags    FlagSupport
}

func ProbeFlags(ctx context.Context, command []string, timeout time.Duration) FlagSupport
func New(opts Options) *Session
func (s *Session) Send(ctx context.Context, input Input) error
func (s *Session) Events() <-chan Event
func (s *Session) Abort(ctx context.Context) error
func (s *Session) Close(ctx context.Context) error
func (s *Session) NativeSessionID() string
```

- [x] **Step 2: Run Kimi native tests to verify RED**

Run: `go test ./agent/kimi/ -v`

Expected: FAIL because the old build-tagged Kimi package does not expose the native API.

- [x] **Step 3: Implement Kimi-native probing and per-turn execution**

Move flag probing, argv, resume extraction, native parsing, pending text ordering, process lifecycle, timeout, usage, and redaction from `adapters/kimi`. Use shared process/JSONL code and retain the conservative no-`--print` fallback when probing fails.

- [x] **Step 4: Run Kimi native validation**

Run:

```bash
go test ./agent/kimi/ -v
go test -race ./agent/kimi/
go vet ./agent/kimi/
```

Expected: PASS; `rg 'agent-cli-gateway/(runtime|core)' agent/kimi` returns no matches.

- [x] **Step 5: Commit the Kimi native layer**

```bash
git add agent/kimi agent/process agent/protocol
git commit -m "feat: extract native kimi execution"
```

### Task 9: Make The Kimi Adapter A Thin Runtime Bridge

**Files:**
- Modify: `adapters/kimi/adapter.go`
- Modify: `adapters/kimi/session.go`
- Modify: `adapters/kimi/adapter_test.go`
- Delete: `adapters/kimi/protocol.go`
- Delete: `adapters/kimi/proc_unix.go`
- Delete: `adapters/kimi/proc_windows.go`
- Test: `adapters/kimi/adapter_test.go`

- [x] **Step 1: Write failing Kimi adapter mapping tests**

Use a fake Kimi native session implementing an adapter-private `nativeSession` interface (the same method set defined explicitly in Task 5, with Kimi-native `Input` and `Event` types) and an injectable probe function to assert descriptor output, trusted option conversion, native resume ID mapping, prompt conversion, all native event mappings, and lifecycle delegation. Assert that probe results are passed to native options and no probe command is run per turn.

- [x] **Step 2: Run adapter tests to verify RED**

Run: `go test ./adapters/kimi/ -v`

Expected: FAIL because probing and process management still live in the adapter.

- [x] **Step 3: Implement the thin Kimi bridge**

Keep environment parsing, registry setup, descriptor, runtime prompt conversion, native event conversion, and method delegation. Move all command execution and raw event parsing out, then delete adapter process/protocol files.

- [x] **Step 4: Verify the Kimi vertical slice**

Run:

```bash
go test ./agent/kimi/ ./adapters/kimi/ ./worker/ ./api/openai/ ./integration/ -v
go test -race ./agent/kimi/ ./adapters/kimi/
test -z "$(rg -l 'os/exec|bufio\.Scanner|json\.Unmarshal' adapters/kimi || true)"
```

Expected: PASS and no native process/protocol implementation remains in the adapter.

- [x] **Step 5: Commit the Kimi adapter**

```bash
git add agent/kimi adapters/kimi
git commit -m "refactor: reduce kimi adapter to runtime mapping"
```

### Task 10: Remove Every Legacy Agent Source And Compatibility Trace

**Files:**
- Delete: all superseded build-tagged files below `agent/claudecode/`, `agent/codex/`, and `agent/kimi/`
- Delete: `internal/archtest/testdata/allowed_legacy.txt`
- Modify: `internal/archtest/dependencies_test.go`
- Modify: `.dockerignore`
- Test: `internal/archtest/dependencies_test.go`

- [x] **Step 1: Make the final no-legacy assertions RED**

Remove allowlist support from the tests and require zero matches:

```go
func TestNoLegacyAgentSources(t *testing.T) {
	root := moduleRoot(t)
	for _, match := range scanText(t, root, []string{
		"//go:build agent_ref",
		"github.com/tianlinzz/agent-cli-gateway/core",
	}) {
		t.Errorf("legacy source remains: %s contains %q", match.File, match.Needle)
	}
}

func TestAdaptersContainNoNativeExecution(t *testing.T) {
	root := moduleRoot(t)
	for _, match := range scanTextUnder(t, root, "adapters", []string{
		"os/exec", "exec.Command", "bufio.Scanner", "json.Unmarshal",
	}) {
		t.Errorf("native execution remains in adapter: %s contains %q", match.File, match.Needle)
	}
}
```

- [x] **Step 2: Run final architecture tests to verify RED**

Run: `go test ./internal/archtest/ -v`

Expected: FAIL naming any remaining build tag, removed-core import, or native adapter implementation.

- [x] **Step 3: Delete superseded files and narrow the scan scope correctly**

Delete only old files not replaced by Tasks 4, 6, and 8. Remove old hooks, live config, MCP config, provider config/switching, list/history, skill dirs, app-server, and their tests. Do not retain aliases, wrappers, deprecated packages, or hidden build tags. Restrict source scans to active Go/Markdown/config files and exclude `.git` and historical approved design/plan artifacts.

- [x] **Step 4: Run final architecture and full tests**

Run:

```bash
go test ./internal/archtest/ -v
go test ./...
test -z "$(rg -l 'go:build agent_ref|agent-cli-gateway/core' agent adapters runtime worker api cmd || true)"
```

Expected: PASS and the final command prints nothing.

- [x] **Step 5: Commit legacy removal**

```bash
git add -A agent adapters internal/archtest .dockerignore
git commit -m "refactor: remove legacy cc-connect agent tree"
```

### Task 11: Add Provenance, Documentation, And CI Release Gates

**Files:**
- Modify: `adapters/claudecode/SOURCE.md`
- Modify: `adapters/codex/SOURCE.md`
- Modify: `adapters/kimi/SOURCE.md`
- Create: `LICENSES/cc-connect-MIT.txt`
- Modify: `README.md`
- Modify: `AGENTS.md`
- Modify: `CLAUDE.md`
- Modify: `.github/workflows/ci.yml`
- Test: `internal/archtest/dependencies_test.go`

- [x] **Step 1: Add failing documentation/provenance assertions**

Extend the architecture test to require each `SOURCE.md` to contain the upstream repository, baseline commit, source file list, migrated behaviors, material local modifications, and local regression test paths. Require `LICENSES/cc-connect-MIT.txt` and reject the phrases `migration-reference`, `never build with -tags agent_ref`, and the old cc-connect package diagram from active root documentation.

- [x] **Step 2: Run documentation gates to verify RED**

Run: `go test ./internal/archtest/ -run 'TestProvenance|TestActiveDocumentation' -v`

Expected: FAIL naming missing attribution or stale documentation.

- [x] **Step 3: Rewrite active documentation and strengthen CI**

Document the final data path:

```text
OpenAI HTTP/SSE -> runtime.ExecutionBackend -> Worker Supervisor/RPC
-> thin runtime adapter -> native agent package -> CLI process
```

Document the three supported Agents, macOS direct-Worker development, Linux nsjail fail-closed operation, one-active-turn-per-session rule, same-workspace distinct-session concurrency, and dependency boundaries. Update CI with an explicit architecture step and fixed cross-build commands:

```yaml
- name: Enforce architecture boundaries
  run: go test ./internal/archtest/ -v

- name: Build for linux arm64
  run: GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...

- name: Build for windows amd64
  run: GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

Retain the real Linux Docker/nsjail smoke as a required release signal; remove `continue-on-error` if present on the job or step.

- [x] **Step 4: Run documentation and CI-equivalent checks**

Run:

```bash
go test ./internal/archtest/ -v
go vet ./...
go build ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

Expected: PASS.

- [x] **Step 5: Commit documentation and CI**

```bash
git add LICENSES README.md AGENTS.md CLAUDE.md adapters/*/SOURCE.md .github/workflows/ci.yml internal/archtest
git commit -m "docs: publish native agent architecture and release gates"
```

### Task 12: Run Full Verification And Real Development Smokes

**Files:**
- Modify: `docs/superpowers/plans/2026-08-11-agent-layer-rearchitecture.md`
- Test: all packages and local development entrypoints

- [x] **Step 1: Run formatting and static source scans**

Run:

```bash
gofmt -w agent adapters internal/archtest integration
git diff --check
go test ./internal/archtest/ -v
test -z "$(rg -l 'go:build agent_ref|agent-cli-gateway/core' agent adapters runtime worker api cmd || true)"
test -z "$(rg -l 'os/exec|exec\.Command|bufio\.Scanner|json\.Unmarshal' adapters || true)"
```

Expected: all commands succeed and both `test -z` commands produce no output.

- [x] **Step 2: Run the complete Go release gate**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

Expected: PASS with no skipped package caused by build tags.

- [x] **Step 3: Run authenticated macOS Claude Code smoke**

Start the current source with `make dev`, discover models, then send streaming and non-streaming requests to `claude-code/haiku` using the same `X-Workspace-ID` and `X-Gateway-Session-ID`. Verify logs show one persistent Worker session, the second turn reuses it, a client disconnect/abort settles the turn, and no `AskUserQuestion` request can block execution.

```bash
curl -fsS http://127.0.0.1:4096/v1/models
curl -fsS http://127.0.0.1:4096/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Workspace-ID: smoke' \
  -H 'X-Gateway-Session-ID: smoke-claude' \
  -d '{"model":"claude-code/haiku","stream":false,"messages":[{"role":"user","content":"Reply with OK only."}]}'
```

Expected: HTTP 200, content `OK`, one Gateway session ID reused, and no surviving turn process after abort/close.

- [x] **Step 4: Run authenticated macOS Codex smoke**

Use the same workspace with a distinct Codex Gateway session, send two turns, and verify the second process uses the native thread resume ID while the Gateway session remains stable:

```bash
curl -fsS http://127.0.0.1:4096/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-Workspace-ID: smoke' \
  -H 'X-Gateway-Session-ID: smoke-codex' \
  -d '{"model":"codex","stream":false,"messages":[{"role":"user","content":"Reply with OK only."}]}'
```

Expected: HTTP 200, content `OK`, same Gateway session across turns, distinct per-turn CLI PIDs, and a reused native Codex thread ID.

- [x] **Step 5: Run Linux/nsjail smoke or record the external gate explicitly**

On Linux with Docker available:

```bash
docker build -t agent-gateway:agent-layer-smoke .
docker run --rm --privileged --entrypoint /bin/sh \
  -v "$PWD/docker/nsjail-smoke.sh:/nsjail-smoke.sh:ro" \
  agent-gateway:agent-layer-smoke /nsjail-smoke.sh
```

Expected: image build succeeds, nsjail version/preflight succeeds, and the minimal jail executes. On macOS without a Linux Docker runtime, do not claim this gate passed; report it as pending CI/release verification.

- [x] **Step 6: Mark executed checkboxes and commit verification evidence**

Update only the checkboxes actually executed in this plan. Include command outcomes and explicitly mark the Linux/nsjail smoke as externally pending when it could not run locally.

```bash
git add docs/superpowers/plans/2026-08-11-agent-layer-rearchitecture.md
git commit -m "test: verify native agent gateway rearchitecture"
```

## Verification Record (2026-08-11)

- Formatting and architecture: `gofmt -d` and `git diff --check` were clean;
  `go test ./internal/archtest/ -v` passed all dependency, legacy-source,
  adapter-native-execution, provenance, and active-documentation gates. Direct
  scans found no `agent_ref`, removed `core` import, or native process/JSONL
  implementation below `adapters/`.
- Complete Go gate: `go test ./...`, `go test -race ./...`, `go vet ./...`,
  `go build ./...`, Linux arm64 cross-build, and Windows amd64 cross-build all
  exited successfully after the final regression fixes.
- Development discovery: on macOS, `make dev` detected installed Claude Code
  `2.1.220` and Codex CLI `0.145.0`, omitted unavailable Kimi, and `/v1/models`
  returned only the three configured Claude variants plus `codex`.
- Claude Code smoke: non-streaming and SSE turns returned exactly `OK` and
  `TWO` under Gateway session `smoke-claude`; the Worker created one persistent
  process. `AskUserQuestion` did not block, abort settled the active request,
  and the child process was reaped.
- Codex smoke: three turns reused Gateway session `smoke-codex`; each turn used
  a separate CLI process and later turns invoked `codex exec resume` with the
  same native thread ID `019feee3-3d51-7722-9477-9b44dd60f153`.
- Kimi coverage: the Kimi executable was not installed on this macOS host, so
  no authenticated real-CLI smoke was claimed. Native fake-CLI regression,
  race, vet, Linux arm64 compilation, and Windows amd64 compilation passed.
- Linux/nsjail smoke: Docker Desktop provided a Linux arm64 runtime. Image
  `agent-gateway:agent-layer-smoke` built successfully; the privileged smoke
  validated dynamic linkage and executed `/bin/true` in the minimal nsjail,
  which exited 0 with no PIDs left.
- Upstream provenance: the selected cc-connect baseline is
  `3fc360ee6acc9bab13ab1b48ddde3af44062903b`. Its README declares MIT, but that
  Git tree has no standalone `LICENSE` or `COPYING` file; the local provenance
  record preserves this caveat and does not invent missing grant text.

## Completion Audit

Before declaring the refactor complete, verify all of the following from the repository state rather than from prior command output:

- All three production paths are `agent/<name> -> adapters/<name> -> runtime -> worker -> api/openai`.
- Native Agent packages import neither `runtime` nor removed `core`.
- Adapters contain no command launch, process-group, raw JSONL, or Agent-native event switch.
- Worker and API packages contain no concrete Agent protocol knowledge.
- No `agent_ref`, compatibility wrapper, deprecated alias, duplicate implementation, or intentionally uncompilable source remains.
- The caller/workspace/session separation and one-active-turn-per-session behavior remain covered by integration tests.
- Claude persistent lifecycle, Codex/Kimi resume-per-turn behavior, cancellation settlement, and native session persistence are covered by fake-CLI regression tests.
- Upstream MIT attribution names commit `3fc360ee6acc9bab13ab1b48ddde3af44062903b` and the selectively migrated source files.
- Full tests, race tests, vet, native build, Linux arm64 build, Windows amd64 build, authenticated Claude/Codex smoke, and Linux/nsjail release smoke have an explicit current result.
