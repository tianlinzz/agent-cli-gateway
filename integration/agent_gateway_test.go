// Package integration contains container-level end-to-end tests for the agent
// gateway. They wire the real OpenAI-compatible HTTP API (api/openai) to the
// real worker Supervisor (worker/) which forks ONE isolated worker process per
// session, so the full path — HTTP -> API handler -> LocalExecutionBackend ->
// Supervisor -> worker child -> gRPC -> canonical events -> SSE — is exercised
// on any host, including this darwin dev host.
//
// The worker child is the hermetic stub worker (worker/testworker), NOT the
// real codex/claude/kimi CLIs, so the suite is fast and does not require any
// agent CLI to be installed. It is a Go binary built once by TestMain.
//
// Real nsjail cannot run on darwin: the production nsjail-wrapped path and the
// nsjail smoke checks run on Linux CI (docker/nsjail-smoke.sh). Here the
// nsjail preflight fail-closed behavior is exercised via a config stub (a dev
// profile pointed at a missing nsjail binary), which must make readiness 503
// and refuse to start sessions.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/api/openai"
	"github.com/tianlinzz/agent-cli-gateway/config"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/worker"
)

const (
	intToken = "integration-secret-token"
	intOwner = "owner-a"
)

// ---------------------------------------------------------------------------
// TestMain: build the hermetic stub worker once for the whole package.
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	var err error
	stubWorkerBin, stubWorkerDir, err = buildStubWorker()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(stubWorkerDir)
	os.Exit(code)
}

var (
	stubWorkerBin string
	stubWorkerDir string
)

func buildStubWorker() (bin, dir string, err error) {
	// Locate the module root: this file lives in <root>/integration/.
	_, file, _, _ := goruntime.Caller(0)
	root := filepath.Dir(filepath.Dir(file))
	dir, err = os.MkdirTemp("", "gateway-int-worker-*")
	if err != nil {
		return "", "", err
	}
	bin = filepath.Join(dir, "testworker")
	cmd := exec.Command("go", "build", "-o", bin, "./worker/testworker")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("build stub worker: %w\n%s", err, out)
	}
	return bin, dir, nil
}

// ---------------------------------------------------------------------------
// Harness: real Handler + real LocalExecutionBackend + real stub worker child.
// ---------------------------------------------------------------------------

// fakeAdapter satisfies runtime.AgentAdapter so the three first-generation
// agents can be registered for model discovery. Start must never be called:
// the worker child (testworker) serves the session, not this adapter.
type fakeAdapter struct {
	desc runtime.Descriptor
}

func (a *fakeAdapter) Describe(context.Context) (runtime.Descriptor, error) {
	return a.desc, nil
}

func (a *fakeAdapter) Start(context.Context, runtime.StartRequest) (runtime.Session, error) {
	return nil, fmt.Errorf("integration: fake adapter Start must never be called")
}

func registerFake(t *testing.T, reg *runtime.Registry, name, display string) {
	t.Helper()
	if err := reg.Register(name, func(ctx context.Context, n string) (runtime.AgentAdapter, error) {
		return &fakeAdapter{desc: runtime.Descriptor{
			ModelID:       name,
			DisplayName:   display,
			Description:   "hermetic fake adapter for integration tests",
			LifecycleMode: runtime.LifecyclePersistentProcess,
			Capabilities: runtime.Capabilities{
				Streaming: true, ToolCalls: true, Reasoning: true,
				Permission: true, Resume: true, MultiTurn: true,
			},
		}}, nil
	}); err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
}

// harness is one fully-wired gateway instance: HTTP API in front of the real
// Supervisor, which forks the stub worker (direct spawn in test mode). Every
// test gets its own harness so worker env/behavior selection never leaks.
type harness struct {
	t          *testing.T
	sup        *worker.Supervisor
	be         *worker.LocalExecutionBackend
	ts         *httptest.Server
	wsRoot     string
	runtimeDir string
}

func newHarness(t *testing.T, mutCfg func(*worker.Config)) *harness {
	t.Helper()

	wsRoot := t.TempDir()
	// The per-session Unix socket path is kernel-limited (~104 bytes), so the
	// runtime dir must be a short host path, not Go's deep t.TempDir().
	runtimeDir, err := os.MkdirTemp(os.TempDir(), "gwint-")
	if err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })

	cfg := worker.Config{
		Mode:            config.ModeTest,
		Isolation:       config.IsolationConfig{Required: false},
		WorkspaceRoot:   wsRoot,
		RuntimeDir:      runtimeDir,
		WorkerExec:      stubWorkerBin,
		StartTimeout:    10 * time.Second,
		StopGracePeriod: 5 * time.Second,
	}
	if mutCfg != nil {
		mutCfg(&cfg)
	}

	backend, err := worker.NewLocalExecutionBackend(cfg)
	if err != nil {
		t.Fatalf("NewLocalExecutionBackend: %v", err)
	}
	sup := backend.Supervisor()

	reg := runtime.NewRegistry()
	registerFake(t, reg, "codex", "Codex")
	registerFake(t, reg, "claude-code", "Claude Code")
	registerFake(t, reg, "kimi", "Kimi")

	h := openai.NewHandler(openai.Options{
		Registry:     reg,
		Store:        runtime.NewMemorySessionStore(),
		Backend:      backend,
		CallerTokens: map[string]string{intToken: "integration-caller"},
		TurnTimeout:  15 * time.Second,
		UsageGrace:   40 * time.Millisecond,
		Enabled:      func(name string) bool { return true },
	})
	ts := httptest.NewServer(h.Routes())

	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := sup.Close(ctx); err != nil {
			t.Errorf("supervisor close: %v", err)
		}
	})
	return &harness{t: t, sup: sup, be: backend, ts: ts, wsRoot: wsRoot, runtimeDir: runtimeDir}
}

// setEnv sets an env var for the test process and restores it on cleanup. The
// supervisor captures os.Environ() at StartSession, so a value set here reaches
// workers spawned afterwards. os.Setenv (not t.Setenv) so one test can change
// a value multiple times (the stub worker selects its behavior from
// GW_TESTWORKER_BEHAVIOR at process start).
func (h *harness) setEnv(key, value string) {
	h.t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Setenv(key, value); err != nil {
		h.t.Fatalf("setenv %s: %v", key, err)
	}
	h.t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func (h *harness) setWorkerBehavior(v string) {
	h.t.Helper()
	h.setEnv("GW_TESTWORKER_BEHAVIOR", v)
}

// do performs an authenticated JSON request and returns the response (body is
// closed at cleanup; streaming callers should read it in-test).
func (h *harness) do(method, path string, body any, headers map[string]string) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+intToken)
	req.Header.Set("X-User-Id", intOwner)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// chatBody builds an OpenAI chat completion request. The gateway auto-creates
// a session when no session id is supplied and returns it in the
// X-Gateway-Session-Id response header; clients resume with that header.
func chatBody(model string, stream bool, workspaceID string) map[string]any {
	b := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"metadata": map[string]any{"workspace_id": workspaceID},
	}
	if stream {
		b["stream"] = true
	}
	return b
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestIntegration_HealthLiveAndReady verifies the health probes: liveness is
// always ok; readiness reports ok in test mode (isolation disabled so the
// trivially-passing preflight is the only gate).
func TestIntegration_HealthLiveAndReady(t *testing.T) {
	h := newHarness(t, nil)

	live := h.do("GET", "/health/live", nil, nil)
	if live.StatusCode != http.StatusOK {
		t.Errorf("live status = %d, want 200", live.StatusCode)
	}
	ready := h.do("GET", "/health/ready", nil, nil)
	if ready.StatusCode != http.StatusOK {
		t.Errorf("ready status = %d, want 200 (test profile: preflight passes)", ready.StatusCode)
	}
}

// TestIntegration_ModelDiscovery verifies /v1/models lists exactly the three
// registered first-generation adapters, sorted by id.
func TestIntegration_ModelDiscovery(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.do("GET", "/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	var ml struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(readBody(t, resp), &ml); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if ml.Object != "list" {
		t.Errorf("object = %q, want list", ml.Object)
	}
	if len(ml.Data) != 3 {
		t.Fatalf("data length = %d, want 3: %+v", len(ml.Data), ml.Data)
	}
	for i, want := range []string{"claude-code", "codex", "kimi"} {
		if ml.Data[i].ID != want {
			t.Errorf("data[%d].id = %q, want %q (sorted)", i, ml.Data[i].ID, want)
		}
		if ml.Data[i].OwnedBy != "agent-cli-gateway" {
			t.Errorf("data[%d].owned_by = %q, want agent-cli-gateway", i, ml.Data[i].OwnedBy)
		}
	}
}

// TestIntegration_StreamCompletionEndToEnd runs a streaming completion through
// the real worker process: the stub worker echoes the turn, the gateway turns
// canonical events into SSE chunks, and the stream terminates with [DONE].
func TestIntegration_StreamCompletionEndToEnd(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.do("POST", "/v1/chat/completions", chatBody("codex", true, "ws-1"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if sid := resp.Header.Get("X-Gateway-Session-Id"); sid == "" {
		t.Error("no X-Gateway-Session-Id in streaming response")
	}

	events := splitSSE(t, readBody(t, resp))
	if len(events) < 3 {
		t.Fatalf("sse events = %d, want >= 3", len(events))
	}
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("last sse event = %q, want [DONE]", events[len(events)-1])
	}
	var content strings.Builder
	for i := 0; i < len(events)-1; i++ {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(events[i]), &chunk) == nil {
			for _, c := range chunk.Choices {
				content.WriteString(c.Delta.Content)
			}
		}
	}
	if got := content.String(); got != "echo:hello" {
		t.Errorf("streamed content = %q, want echo:hello", got)
	}
}

// TestIntegration_MultiTurnResumeReusesExecution verifies a second turn on the
// same gateway session reuses the live worker execution instead of forking a
// new one (the stub worker is a persistent process). The gateway returns the
// generated session id in the X-Gateway-Session-Id header; resuming with it
// must NOT start a second execution.
func TestIntegration_MultiTurnResumeReusesExecution(t *testing.T) {
	h := newHarness(t, nil)

	resp1 := h.do("POST", "/v1/chat/completions", chatBody("kimi", false, "ws-1"), nil)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("turn 1 status = %d (body %s)", resp1.StatusCode, readBody(t, resp1))
	}
	sid := resp1.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("no X-Gateway-Session-Id in turn 1 response")
	}

	resp2 := h.do("POST", "/v1/chat/completions", chatBody("kimi", false, "ws-1"),
		map[string]string{"X-Gateway-Session-Id": sid})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("turn 2 status = %d (body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	if n := h.sup.SessionCount(); n != 1 {
		t.Errorf("supervisor sessions = %d, want 1 (execution reused across turns)", n)
	}

	// Per-session isolation: exactly one per-session runtime dir (sockets,
	// agent-home, profiles) exists on disk for the single live session.
	entries, err := os.ReadDir(filepath.Join(h.runtimeDir, "sessions"))
	if err != nil {
		t.Fatalf("read runtime sessions dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("per-session runtime dirs = %d, want 1", len(entries))
	}
}

// TestIntegration_CallerProvidedSessionCreatedOnFirstUse freezes the public
// contract used by upstream systems such as f1-web: a business conversation
// id may be supplied on the first request and becomes the stable Gateway
// session id instead of requiring a separate create-session call.
func TestIntegration_CallerProvidedSessionCreatedOnFirstUse(t *testing.T) {
	h := newHarness(t, nil)
	const sessionID = "business-conversation-517f05c4"

	resp1 := h.do("POST", "/v1/chat/completions", chatBody("claude-code", false, "ws-1"),
		map[string]string{"X-Gateway-Session-Id": sessionID})
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first turn status = %d (body %s)", resp1.StatusCode, readBody(t, resp1))
	}
	if got := resp1.Header.Get("X-Gateway-Session-Id"); got != sessionID {
		t.Fatalf("gateway session id = %q, want caller id %q", got, sessionID)
	}

	resp2 := h.do("POST", "/v1/chat/completions", chatBody("claude-code", false, "ws-1"),
		map[string]string{"X-Gateway-Session-Id": sessionID})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second turn status = %d (body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	if n := h.sup.SessionCount(); n != 1 {
		t.Fatalf("supervisor sessions = %d, want 1", n)
	}
}

// TestIntegration_SameWorkspaceDifferentSessionsRunConcurrently guards the
// intended concurrency boundary: only turns within one session serialize.
// Distinct sessions sharing a workspace are allowed to run at the same time.
func TestIntegration_SameWorkspaceDifferentSessionsRunConcurrently(t *testing.T) {
	h := newHarness(t, nil)
	h.setWorkerBehavior("slow-echo")

	resp1 := h.do("POST", "/v1/chat/completions", chatBody("codex", true, "shared-workspace"),
		map[string]string{"X-Gateway-Session-Id": "shared-session-a"})
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("session A status = %d (body %s)", resp1.StatusCode, readBody(t, resp1))
	}

	resp2 := h.do("POST", "/v1/chat/completions", chatBody("codex", true, "shared-workspace"),
		map[string]string{"X-Gateway-Session-Id": "shared-session-b"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("session B status = %d (body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	if n := h.sup.SessionCount(); n != 2 {
		t.Fatalf("concurrent supervisor sessions = %d, want 2", n)
	}

	for name, resp := range map[string]*http.Response{"A": resp1, "B": resp2} {
		events := splitSSE(t, readBody(t, resp))
		if len(events) == 0 || events[len(events)-1] != "[DONE]" {
			t.Errorf("session %s did not finish cleanly: %v", name, events)
		}
	}
}

// TestIntegration_AbortCancelsInFlightTurn verifies POST /v1/sessions/{id}/abort
// reaches the worker (the Abort RPC is logged by the stub) and that the
// in-flight SSE stream terminates.
func TestIntegration_AbortCancelsInFlightTurn(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	h := newHarness(t, nil)
	h.setWorkerBehavior("slow-echo")
	h.setEnv("GW_TESTWORKER_LOG", logPath)

	// Start a streaming turn that stays in flight: the stub delays its reply
	// ~1.5s, so the abort lands while the turn is active. The gateway
	// auto-creates the session and returns its id in the response header.
	sid := ""
	body, err := json.Marshal(chatBody("codex", true, "ws-1"))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", h.ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+intToken)
	req.Header.Set("X-User-Id", intOwner)
	req.Header.Set("Content-Type", "application/json")

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := http.DefaultClient.Do(req)
		respCh <- r
		errCh <- err
	}()

	var r *http.Response
	select {
	case r = <-respCh:
		if err := <-errCh; err != nil {
			t.Fatalf("stream request error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("streaming response never arrived")
	}
	defer r.Body.Close()
	sid = r.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("no X-Gateway-Session-Id in streaming response")
	}

	// The handler flushed headers, so the turn is in flight. Abort it.
	abort := h.do("POST", "/v1/sessions/"+sid+"/abort", nil, nil)
	if abort.StatusCode != http.StatusOK {
		t.Fatalf("abort status = %d (body %s)", abort.StatusCode, readBody(t, abort))
	}
	var abortBody map[string]string
	if err := json.Unmarshal(readBody(t, abort), &abortBody); err != nil {
		t.Fatalf("decode abort body: %v", err)
	}
	if abortBody["status"] != "aborted" {
		t.Errorf("abort body = %+v, want status aborted", abortBody)
	}

	// The cancelled turn must terminate the stream.
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read stream body: %v", err)
	}
	if !bytes.Contains(b, []byte("[DONE]")) {
		t.Errorf("stream body after abort missing [DONE]: %q", b)
	}

	// The Abort RPC must have reached the worker process.
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, rerr := os.ReadFile(logPath)
		if rerr == nil && strings.Contains(string(data), "abort") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker log never recorded the abort RPC:\n%s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A successful turn-scoped abort keeps the same Worker/session available.
	nextBody := chatBody("codex", false, "ws-1")
	nextBody["messages"] = []map[string]any{{"role": "user", "content": "after"}}
	next := h.do("POST", "/v1/chat/completions", nextBody,
		map[string]string{"X-Gateway-Session-Id": sid})
	if next.StatusCode != http.StatusOK {
		t.Fatalf("post-abort turn status = %d (body %s)", next.StatusCode, readBody(t, next))
	}
	var completion completionBody
	if err := json.Unmarshal(readBody(t, next), &completion); err != nil {
		t.Fatalf("decode post-abort completion: %v", err)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content == nil || *completion.Choices[0].Message.Content != "echo:after" {
		t.Fatalf("post-abort content = %+v, want echo:after", completion.Choices)
	}
	if count := h.sup.SessionCount(); count != 1 {
		t.Fatalf("post-abort supervisor sessions = %d, want same live execution", count)
	}
}

// TestIntegration_WorkerCrash_ApiRemainsUsable verifies worker failures are
// isolated to the session: a worker that dies mid-turn (the stub exits without
// output ~1.5s after accepting the turn) closes its event stream, is reaped by
// the supervisor, and the API immediately serves a fresh session.
func TestIntegration_WorkerCrash_ApiRemainsUsable(t *testing.T) {
	h := newHarness(t, nil)

	// Session A: the worker accepts the turn, then dies mid-turn with no output.
	h.setWorkerBehavior("slow-crash")
	respA := h.do("POST", "/v1/chat/completions", chatBody("codex", true, "ws-1"), nil)
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("crashing session status = %d (body %s)", respA.StatusCode, readBody(t, respA))
	}
	eventsA := splitSSE(t, readBody(t, respA))
	if len(eventsA) == 0 || eventsA[len(eventsA)-1] != "[DONE]" {
		t.Errorf("crashing session stream must terminate with [DONE], got %v", eventsA)
	}

	// The crashed session must be reaped, not linger in the supervisor.
	deadline := time.Now().Add(5 * time.Second)
	for h.sup.SessionCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("crashed session not reaped (sessions = %d)", h.sup.SessionCount())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Session B: the API must still serve a normal turn end to end.
	h.setWorkerBehavior("")
	respB := h.do("POST", "/v1/chat/completions", chatBody("kimi", false, "ws-1"), nil)
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("recovery session status = %d (body %s)", respB.StatusCode, readBody(t, respB))
	}
	var cb completionBody
	if err := json.Unmarshal(readBody(t, respB), &cb); err != nil {
		t.Fatalf("decode recovery response: %v", err)
	}
	if len(cb.Choices) != 1 || cb.Choices[0].Message.Content == nil || *cb.Choices[0].Message.Content != "echo:hello" {
		t.Errorf("recovery session content = %+v, want echo:hello", cb.Choices)
	}
}

// TestIntegration_WorkspaceOutOfBoundsRejected verifies the workspace resolver
// boundary over the public API: traversal/absolute workspace ids fail closed
// (no directory is created, the request 500s) while a benign opaque id works.
// The nsjail mount namespace is the second boundary on Linux; this asserts the
// first (resolver) boundary from the API surface.
func TestIntegration_WorkspaceOutOfBoundsRejected(t *testing.T) {
	h := newHarness(t, nil)

	for _, bad := range []string{"..", "../escape", "/etc", "a/b"} {
		resp := h.do("POST", "/v1/chat/completions", chatBody("codex", false, bad), nil)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Errorf("workspace_id %q: status = %d, want 500 (body %s)", bad, resp.StatusCode, readBody(t, resp))
		}
	}

	// Nothing may have been created under the workspace root by the rejected
	// requests.
	entries, err := os.ReadDir(h.wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("workspace root has entries after rejected requests: %v", entries)
	}

	// A benign opaque workspace id still works.
	resp := h.do("POST", "/v1/chat/completions", chatBody("codex", false, "ws-ok"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("benign workspace status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
}

// TestIntegration_PreflightFailClosed verifies the nsjail preflight boundary
// via a config stub: a dev profile pointed at a missing nsjail binary makes
// readiness 503 and refuses to start sessions — the gateway never serves
// unsandboxed workers. (The real nsjail preflight + smoke jail run on Linux CI
// via docker/nsjail-smoke.sh; on darwin this is the fail-closed simulation.)
func TestIntegration_PreflightFailClosed(t *testing.T) {
	h := newHarness(t, func(c *worker.Config) {
		c.Mode = config.ModeDev
		c.Isolation = config.IsolationConfig{
			Required:      true,
			NsjailVersion: "0.12.0",
			NsjailSource:  "https://github.com/google/nsjail",
			BinaryPath:    filepath.Join(t.TempDir(), "nsjail-missing"),
			Mounts: config.MountsConfig{
				WorkspaceDir: "/workspace",
				AgentHomeDir: "/agent-home",
				TmpDir:       "/tmp",
			},
		}
	})

	if err := h.be.Preflight(context.Background()); err == nil {
		t.Fatal("Preflight must fail closed when the nsjail binary is missing")
	}

	ready := h.do("GET", "/health/ready", nil, nil)
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("ready status = %d, want 503 (body %s)", ready.StatusCode, readBody(t, ready))
	}
	live := h.do("GET", "/health/live", nil, nil)
	if live.StatusCode != http.StatusOK {
		t.Errorf("live status = %d, want 200", live.StatusCode)
	}

	// A session start must fail closed (500) without spawning any worker.
	resp := h.do("POST", "/v1/chat/completions", chatBody("codex", false, "ws-1"), nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("session start under broken isolation status = %d, want 500 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if n := h.sup.SessionCount(); n != 0 {
		t.Errorf("failed session must not be registered, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type completionBody struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Model   string          `json:"model"`
	Choices []choiceMessage `json:"choices"`
}

type choiceMessage struct {
	Index        int             `json:"index"`
	Message      chatCompMsg     `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

type chatCompMsg struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// splitSSE splits a raw SSE body into individual "data:" payloads.
func splitSSE(t *testing.T, body []byte) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	var cur []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, "\n"))
			}
			cur = nil
			continue
		}
		if strings.HasPrefix(line, "data:") {
			cur = append(cur, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan sse body: %v", err)
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}
