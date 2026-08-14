package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// ---------------------------------------------------------------------------
// Fake adapters, backend and session handles (the API layer only ever speaks
// the canonical runtime contract, so a fake backend is enough; no real adapter
// is imported here).
// ---------------------------------------------------------------------------

type fakeAdapter struct {
	desc    runtime.Descriptor
	descErr error
}

func (a *fakeAdapter) Describe(ctx context.Context) (runtime.Descriptor, error) {
	return a.desc, a.descErr
}

func (a *fakeAdapter) Start(ctx context.Context, req runtime.StartRequest) (runtime.Session, error) {
	return nil, errors.New("fake adapter Start must never be called by the API layer")
}

type fakeHandle struct {
	mu      sync.Mutex
	sends   int
	dead    bool                // true = the worker/CLI exited; Send fails terminally
	script  func(h *fakeHandle) // runs once per Send, in a goroutine
	onAbort func(h *fakeHandle)
	events  chan runtime.Event

	abortOnce sync.Once
	aborted   chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

func newFakeHandle(script func(h *fakeHandle)) *fakeHandle {
	return &fakeHandle{
		script:  script,
		events:  make(chan runtime.Event, 256),
		aborted: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (h *fakeHandle) Send(ctx context.Context, input runtime.Input) error {
	h.mu.Lock()
	h.sends++
	dead := h.dead
	h.mu.Unlock()
	if dead {
		return errors.New("fake handle: session is closed (worker exited)")
	}
	if h.script != nil {
		go h.script(h)
	}
	return nil
}

// markDead simulates the worker/CLI exiting while the session is idle: the
// handle stops accepting turns (Send fails terminally) and reports Closed.
func (h *fakeHandle) markDead() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dead = true
}

// Closed implements the optional closedHandle capability the handler uses to
// distinguish a dead handle (recover with a fresh execution) from a transient
// send error.
func (h *fakeHandle) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dead
}

func (h *fakeHandle) SendCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sends
}

func (h *fakeHandle) emit(evs ...runtime.Event) {
	for _, ev := range evs {
		h.events <- ev
	}
}

// emitThenClose emits events and then closes the stream (session ended).
func (h *fakeHandle) emitThenClose(evs ...runtime.Event) {
	h.emit(evs...)
	close(h.events)
}

func (h *fakeHandle) Events() <-chan runtime.Event { return h.events }

func (h *fakeHandle) Abort(ctx context.Context) error {
	h.abortOnce.Do(func() {
		h.mu.Lock()
		onAbort := h.onAbort
		h.mu.Unlock()
		if onAbort != nil {
			onAbort(h)
		} else {
			h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "cancelled"})
		}
		close(h.aborted)
	})
	return nil
}

func (h *fakeHandle) Aborted() <-chan struct{} { return h.aborted }

// Done exposes the terminal signal (closed by Close) so the handle satisfies the
// terminatingHandle capability the watcher in registerHandle listens for.
func (h *fakeHandle) Done() <-chan struct{} { return h.closed }

func (h *fakeHandle) setOnAbort(onAbort func(h *fakeHandle)) {
	h.mu.Lock()
	h.onAbort = onAbort
	h.mu.Unlock()
}

func (h *fakeHandle) Close(ctx context.Context) error {
	h.closeOnce.Do(func() { close(h.closed) })
	return nil
}

type fakeBackend struct {
	mu           sync.Mutex
	handles      map[string]*fakeHandle
	started      []runtime.StartRequest
	startErr     error
	preflightErr error
	script       func(h *fakeHandle) // script handed to every created handle
}

func (b *fakeBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.ExecutionHandle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return nil, b.startErr
	}
	h := newFakeHandle(b.script)
	b.handles[req.SessionID] = h
	b.started = append(b.started, req)
	return h, nil
}

func (b *fakeBackend) Preflight(ctx context.Context) error {
	return b.preflightErr
}

func (b *fakeBackend) StartRequests() []runtime.StartRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]runtime.StartRequest, len(b.started))
	copy(out, b.started)
	return out
}

func (b *fakeBackend) Handle(sessionID string) *fakeHandle {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handles[sessionID]
}

// withScript sets the script for handles created by subsequent Start calls and
// resets any handles/start-requests recorded so far.
func (b *fakeBackend) withScript(script func(h *fakeHandle)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.script = script
	b.handles = make(map[string]*fakeHandle)
	b.started = nil
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{handles: make(map[string]*fakeHandle)}
}

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

const (
	testToken     = "test-secret-token"
	testTokenB    = "test-secret-token-b"
	testOwner     = "owner-a"
	testOwnerB    = "owner-b"
	testWorkspace = "ws-default"
)

// codexDesc is the fake adapter descriptor for the "codex" model.
var codexDesc = runtime.Descriptor{
	ModelID:       "codex",
	DisplayName:   "Codex",
	Description:   "fake codex for api tests",
	LifecycleMode: runtime.LifecyclePersistentProcess,
	Capabilities: runtime.Capabilities{
		Streaming: true, ToolCalls: true, Reasoning: true,
		Permission: true, Resume: true, MultiTurn: true,
	},
}

// newTestHandler builds a Handler backed by a fresh registry (registered fake
// adapters), a real memory SessionStore, and a fake backend. Tests may mutate
// opts before construction.
func newTestHandler(t *testing.T, opts *Options) (*Handler, *fakeBackend) {
	t.Helper()
	if opts == nil {
		opts = &Options{}
	}
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	mustRegister(t, reg, "zeta", runtime.Descriptor{ModelID: "zeta", DisplayName: "Zeta"}, nil)
	mustRegister(t, reg, "broken", runtime.Descriptor{ModelID: "broken"}, errors.New("discovery fails"))

	backend := newFakeBackend()
	store := runtime.NewMemorySessionStore()

	o := *opts
	o.Registry = reg
	o.Store = store
	o.Backend = backend
	h := NewHandler(o)
	t.Cleanup(h.Close)
	return h, backend
}

func mustRegister(t *testing.T, reg *runtime.Registry, name string, desc runtime.Descriptor, descErr error) {
	t.Helper()
	reg.Register(name, func(ctx context.Context, n string) (runtime.AgentAdapter, error) {
		return &fakeAdapter{desc: desc, descErr: descErr}, nil
	})
}

// newTestServer wraps a Handler in an httptest.Server with auth enabled.
func newTestServer(t *testing.T, mut ...func(*Options)) (*httptest.Server, *Handler, *fakeBackend) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	opts := &Options{
		CallerTokens: map[string]string{testToken: testOwner, testTokenB: testOwnerB},
		TurnTimeout:  5 * time.Second,
		UsageGrace:   15 * time.Millisecond,
		Now:          func() time.Time { return now },
		Enabled:      func(name string) bool { return name != "disabled" },
	}
	for _, m := range mut {
		m(opts)
	}
	h, backend := newTestHandler(t, opts)
	ts := httptest.NewServer(h.Routes())
	t.Cleanup(ts.Close)
	return ts, h, backend
}

// TestHandlerPruneLoopEvictsExpiredRecordAndClosesHandle is a regression test
// for the session-record/handle leak (U1): an expired session record must be
// evicted by the background prune loop, and its cached execution handle must be
// dropped and closed so neither grows unbounded over the process lifetime.
func TestHandlerPruneLoopEvictsExpiredRecordAndClosesHandle(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	store := runtime.NewMemorySessionStore()
	h := NewHandler(Options{
		Registry:         reg,
		Store:            store,
		Backend:          newFakeBackend(),
		SessionRecordTTL: time.Hour,
		PruneInterval:    5 * time.Millisecond,
	})
	t.Cleanup(h.Close)

	const sid = "sess-expiring"
	ctx := context.Background()
	// Seed a session record born already expired plus its cached handle.
	rec := runtime.SessionRecord{
		ID:        sid,
		CallerID:  testOwner,
		ModelID:   "codex",
		Status:    runtime.SessionActive,
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fh := newFakeHandle(nil)
	h.registerHandle(sid, fh)
	if h.getHandle(sid) == nil {
		t.Fatal("handle not registered")
	}

	// The prune loop evicts the record and dropHandleByID drops+closes the
	// handle. Poll the handle map (not store.Get, which would lazy-purge the
	// record and race the prune loop). dropHandleByID only runs for ids Prune
	// actually removed, so a nil handle proves the full chain ran.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.getHandle(sid) == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.getHandle(sid); got != nil {
		t.Fatalf("handle still registered after prune: %v", got)
	}
	// The record is gone too (List omits expired records without purging).
	recs, err := store.List(ctx, testOwner)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range recs {
		if r.ID == sid {
			t.Fatalf("expired record survived prune: %+v", r)
		}
	}
	select {
	case <-fh.closed:
	case <-time.After(time.Second):
		t.Fatal("expired session handle was not closed by prune loop")
	}
}

// TestHandlerDeadHandleDroppedWithoutRequest is a regression test for the
// U1 dead-handle closure (Done()): a worker/CLI that dies while the session is
// idle must be dropped from the handle map promptly, without waiting for TTL
// prune or the next request to discover it.
func TestHandlerDeadHandleDroppedWithoutRequest(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	h, _ := newTestHandler(t, nil)

	fh := newFakeHandle(nil)
	const sid = "sess-idle-death"
	h.registerHandle(sid, fh)
	if h.getHandle(sid) == nil {
		t.Fatal("handle not registered")
	}
	// The worker dies while idle (Done() fires). The watcher must drop the
	// handle without any request arriving.
	fh.Close(context.Background())

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && h.getHandle(sid) != nil {
		time.Sleep(2 * time.Millisecond)
	}
	if got := h.getHandle(sid); got != nil {
		t.Fatalf("idle-dead handle not dropped by Done() watcher: %v", got)
	}
}

// TestHandlerPruneReclaimsSessionsAtScale is the 1000-session churn regression:
// after a burst of sessions expire, store records, cached handles, and tool
// ledgers all reclaim, and every per-handle termination watcher exits.
func TestHandlerPruneReclaimsSessionsAtScale(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	store := runtime.NewMemorySessionStore()
	// Controllable clock so records are alive during creation and expired only
	// after we advance time, avoiding a race between creation and the prune loop.
	// Guarded because the prune loop reads it from another goroutine.
	var clockMu sync.Mutex
	clock := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	h := NewHandler(Options{
		Registry:         reg,
		Store:            store,
		Backend:          newFakeBackend(),
		SessionRecordTTL: time.Hour,
		PruneInterval:    5 * time.Millisecond,
		Now:              nowFn,
	})
	t.Cleanup(h.Close)

	ctx := context.Background()
	const n = 1000
	handles := make([]*fakeHandle, 0, n)
	for i := 0; i < n; i++ {
		id := "sess-" + strconv.Itoa(i)
		rec := runtime.SessionRecord{
			ID: id, CallerID: testOwner, ModelID: "codex",
			Status: runtime.SessionActive, ExpiresAt: clock.Add(time.Hour), // alive vs clock
		}
		if err := store.Create(ctx, rec); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
		fh := newFakeHandle(nil)
		h.registerHandle(id, fh)
		h.recordServerToolID(id, "tool-"+strconv.Itoa(i))
		handles = append(handles, fh)
	}
	h.mu.Lock()
	startHandles := len(h.handles)
	h.mu.Unlock()
	if startHandles != n {
		t.Fatalf("registered %d handles, want %d", startHandles, n)
	}

	// Advance the clock past every record's TTL; the prune loop then evicts all.
	clockMu.Lock()
	clock = clock.Add(2 * time.Hour)
	clockMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		remaining := len(h.handles)
		h.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.mu.Lock()
	remainingHandles := len(h.handles)
	remainingLedgers := len(h.serverToolIDs)
	h.mu.Unlock()
	if remainingHandles != 0 {
		t.Fatalf("%d handles leaked after prune (want 0)", remainingHandles)
	}
	if remainingLedgers != 0 {
		t.Fatalf("%d tool ledgers leaked after prune (want 0)", remainingLedgers)
	}
	if recs, _ := store.List(ctx, testOwner); len(recs) != 0 {
		t.Fatalf("%d records leaked after prune (want 0)", len(recs))
	}
	// Every termination watcher exited: dropHandleByID closed each handle, so
	// its Done() channel is closed.
	for i, fh := range handles {
		select {
		case <-fh.closed:
		default:
			t.Fatalf("watcher for sess-%d did not close its handle", i)
		}
	}
}

func doAuthJSON(t *testing.T, method, url, token, owner string, body any) *http.Response {
	t.Helper()
	return doAuthJSONH(t, method, url, token, owner, body, nil)
}

func doAuthJSONH(t *testing.T, method, url, token, owner string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		if owner == testOwnerB && token == testToken {
			token = testTokenB
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAuthDerivesCallerFromBearerToken(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp := doAuthJSONH(t, "GET", ts.URL+"/v1/models", testToken, "spoofed-user", nil, map[string]string{
		"X-User-Id": "spoofed-user",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(readBody(t, resp)))
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// decodeError decodes the OpenAI error envelope.
func decodeError(t *testing.T, resp *http.Response) apiError {
	t.Helper()
	var eb errorBody
	if err := json.Unmarshal(readBody(t, resp), &eb); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return eb.Error
}

type completionBody struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Model   string          `json:"model"`
	Choices []choiceMessage `json:"choices"`
	Usage   *usageInfo      `json:"usage"`
}

type choiceMessage struct {
	Index        int             `json:"index"`
	Message      chatCompMessage `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

type chatCompMessage struct {
	Role      string            `json:"role"`
	Content   *string           `json:"content"`
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

// sseData is one parsed SSE event.
type sseData struct {
	raw string
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

func chatReq(model string, stream bool, messages []map[string]any) map[string]any {
	body := map[string]any{
		"model":    model,
		"messages": messages,
		"metadata": map[string]any{"workspace_id": testWorkspace},
	}
	if stream {
		body["stream"] = true
	}
	return body
}

func defaultMessages() []map[string]any {
	return []map[string]any{
		{"role": "system", "content": "You are a helpful assistant."},
		{"role": "user", "content": "hello"},
	}
}

// ---------------------------------------------------------------------------
// /v1/models
// ---------------------------------------------------------------------------

func TestModels_SortedAndFiltered(t *testing.T) {
	ts, h, _ := newTestServer(t)
	_ = h

	resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", testToken, testOwner, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
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
	if len(ml.Data) != 2 {
		t.Fatalf("data length = %d, want 2 (broken must be skipped, disabled excluded)", len(ml.Data))
	}
	// Stable sorted order by id.
	if ml.Data[0].ID != "codex" || ml.Data[1].ID != "zeta" {
		t.Errorf("data ids = [%s %s], want [codex zeta] (sorted)", ml.Data[0].ID, ml.Data[1].ID)
	}
	for _, m := range ml.Data {
		if m.Object != "model" {
			t.Errorf("model %s object = %q, want model", m.ID, m.Object)
		}
		if m.OwnedBy != "agent-cli-gateway" {
			t.Errorf("model %s owned_by = %q, want agent-cli-gateway", m.ID, m.OwnedBy)
		}
	}
}

func TestModels_DisabledExcluded(t *testing.T) {
	ts, _, _ := newTestServer(t, func(o *Options) {
		o.Enabled = func(name string) bool { return name == "codex" }
	})
	resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", testToken, testOwner, nil)
	var ml struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(readBody(t, resp), &ml); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(ml.Data) != 1 || ml.Data[0].ID != "codex" {
		t.Errorf("data = %+v, want only codex (zeta/disabled excluded)", ml.Data)
	}
}

// TestModels_CommandProbeHidesUnavailable is a regression test for the
// model-discovery/availability mismatch (O-F04): with command knowledge wired,
// /v1/models must advertise only enabled adapters whose CLI command resolves on
// PATH. A missing command, an empty command, and an adapter whose Describe
// fails are all hidden.
func TestModels_CommandProbeHidesUnavailable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ts, _, _ := newTestServer(t, func(o *Options) {
		o.Commands = map[string]string{
			"codex":  exe,                         // resolves -> advertised
			"zeta":   "/definitely/not/installed", // missing -> hidden
			"broken": exe,                         // resolves but Describe fails -> hidden
		}
	})
	resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", testToken, testOwner, nil)
	var ml struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(readBody(t, resp), &ml); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(ml.Data) != 1 || ml.Data[0].ID != "codex" {
		t.Fatalf("data = %+v, want only codex (missing/undescribable CLIs hidden)", ml.Data)
	}
}

// TestModels_EnabledWithoutCommandHidden verifies the probe-mode fail-safe:
// when command knowledge is wired (production), an enabled adapter with no
// command is ambiguous (the gateway cannot verify a CLI) and must be hidden
// rather than advertised as usable.
func TestModels_EnabledWithoutCommandHidden(t *testing.T) {
	ts, _, _ := newTestServer(t, func(o *Options) {
		o.Commands = map[string]string{"codex": ""}
	})
	resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", testToken, testOwner, nil)
	var ml struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(readBody(t, resp), &ml); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	if len(ml.Data) != 0 {
		t.Fatalf("data = %+v, want empty catalog (no command resolves)", ml.Data)
	}
}

// ---------------------------------------------------------------------------
// Chat completions (non-stream)
// ---------------------------------------------------------------------------

func TestChatCompletions_NonStream(t *testing.T) {
	ts, h, backend := newTestServer(t)
	_ = h
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "Hello "},
			runtime.Event{Type: runtime.EventText, Text: "world"},
			runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{InputTokens: 11, OutputTokens: 22, TotalTokens: 33}},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	var cb completionBody
	if err := json.Unmarshal(readBody(t, resp), &cb); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if cb.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", cb.Object)
	}
	if cb.Model != "codex" {
		t.Errorf("model = %q, want codex", cb.Model)
	}
	if len(cb.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(cb.Choices))
	}
	ch := cb.Choices[0]
	if ch.Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", ch.Message.Role)
	}
	if ch.Message.Content == nil || *ch.Message.Content != "Hello world" {
		t.Errorf("content = %v, want Hello world", ch.Message.Content)
	}
	if ch.FinishReason == nil || *ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", ch.FinishReason)
	}
	if cb.Usage == nil {
		t.Fatal("usage missing")
	}
	if cb.Usage.PromptTokens != 11 || cb.Usage.CompletionTokens != 22 || cb.Usage.TotalTokens != 33 {
		t.Errorf("usage = %+v, want 11/22/33", cb.Usage)
	}
	if cb.ID == "" || !strings.HasPrefix(cb.ID, "chatcmpl-") {
		t.Errorf("id = %q, want chatcmpl- prefix", cb.ID)
	}
}

func TestChatCompletions_UnknownModel(t *testing.T) {
	ts, _, _ := newTestServer(t)
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("no-such-model", false, defaultMessages()))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	ae := decodeError(t, resp)
	if ae.Type != "invalid_request_error" || ae.Code != "model_not_found" {
		t.Errorf("error = %+v, want type invalid_request_error code model_not_found", ae)
	}
}

func TestChatCompletions_AgentError(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventError, Error: "boom: codex failed"})
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	ae := decodeError(t, resp)
	if ae.Type != "server_error" {
		t.Errorf("error type = %q, want server_error", ae.Type)
	}
	if !strings.Contains(ae.Message, "boom") {
		t.Errorf("error message = %q, want to mention agent error", ae.Message)
	}
}

// TestChatCompletions_BodyTooLargeRejected is a regression test for HTTP input
// hardening (O-F07): a body over the limit must yield an explicit 413 via
// http.MaxBytesReader, not a 400 or a silently truncated read.
func TestChatCompletions_BodyTooLargeRejected(t *testing.T) {
	ts, _, _ := newTestServer(t)
	body := map[string]any{
		"model":    "codex",
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("x", 2*1024*1024)}},
		"metadata": map[string]any{"workspace_id": testWorkspace},
	}
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", resp.StatusCode, readBody(t, resp))
	}
}

// TestChatCompletions_TrailingJSONRejected is a regression test for O-F07: a
// valid object followed by a second top-level JSON value must be rejected, not
// silently accepted on the first value's prefix.
func TestChatCompletions_TrailingJSONRejected(t *testing.T) {
	ts, _, _ := newTestServer(t)
	payload := `{"model":"codex","messages":[{"role":"user","content":"hi"}],"metadata":{"workspace_id":"` + testWorkspace + `"}} {"ignored":true}`
	req, err := http.NewRequest("POST", ts.URL+"/v1/chat/completions", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", resp.StatusCode, readBody(t, resp))
	}
}

// TestChatCompletions_MetadataTooManyKeysRejected is a regression test for the
// metadata bound (O-F07): an oversized metadata map must be rejected before it
// reaches the native prompt.
func TestChatCompletions_MetadataTooManyKeysRejected(t *testing.T) {
	ts, _, _ := newTestServer(t)
	meta := map[string]any{"workspace_id": testWorkspace}
	for i := 0; i < maxMetadataKeys; i++ {
		meta[fmt.Sprintf("k%d", i)] = "v"
	}
	body := map[string]any{
		"model":    "codex",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"metadata": meta,
	}
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", resp.StatusCode, readBody(t, resp))
	}
}

func TestChatCompletions_AllAgentsKeepInternalToolsOutOfModelToolCalls(t *testing.T) {
	ts, h, backend := newTestServer(t)
	mustRegister(t, h.catalog.reg, "claude-code", runtime.Descriptor{ModelID: "claude-code"}, nil)
	mustRegister(t, h.catalog.reg, "kimi", runtime.Descriptor{ModelID: "kimi"}, nil)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "Let me run that."},
			runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
				Name: "Bash", Arguments: map[string]any{"command": "ls -la"},
			}},
			runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{
				Name: "Bash", Result: "file1", IsError: false,
			}},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	for _, model := range []string{"claude-code", "codex", "kimi"} {
		t.Run(model, func(t *testing.T) {
			resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
				chatReq(model, false, defaultMessages()))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
			}
			var cb completionBody
			if err := json.Unmarshal(readBody(t, resp), &cb); err != nil {
				t.Fatalf("decode completion: %v", err)
			}
			if len(cb.Choices) != 1 {
				t.Fatalf("choices len = %d", len(cb.Choices))
			}
			ch := cb.Choices[0]
			if len(ch.Message.ToolCalls) != 0 {
				t.Fatalf("tool_calls = %#v, want none; the native agent already executed them", ch.Message.ToolCalls)
			}
			if ch.Message.Content == nil || *ch.Message.Content != "Let me run that." {
				t.Errorf("content = %v, want native assistant text", ch.Message.Content)
			}
			if ch.FinishReason == nil || *ch.FinishReason != "stop" {
				t.Errorf("finish_reason = %v, want stop so the OpenAI caller does not start another tool loop", ch.FinishReason)
			}
			started := backend.StartRequests()
			if got := started[len(started)-1].ModelID; got != model {
				t.Errorf("backend model = %q, want %q", got, model)
			}
		})
	}
}

func TestChatCompletions_InternalAgentToolWithEmptyReasonStillStops(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
				Name: "Bash", Arguments: map[string]any{"command": "ls"},
			}},
			// A missing native reason must not turn an already-executed internal
			// tool into an instruction for the OpenAI caller to execute it again.
			runtime.Event{Type: runtime.EventFinish, FinishReason: ""},
		)
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	var cb completionBody
	if err := json.Unmarshal(readBody(t, resp), &cb); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if len(cb.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(cb.Choices))
	}
	ch := cb.Choices[0]
	if len(ch.Message.ToolCalls) != 0 {
		t.Fatalf("tool_calls = %#v, want none", ch.Message.ToolCalls)
	}
	if ch.FinishReason == nil || *ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", ch.FinishReason)
	}
}

func TestChatCompletions_UnknownFinishReason(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "ok"},
			// Unknown canonical reason: never pass it through; fall back to stop.
			runtime.Event{Type: runtime.EventFinish, FinishReason: "pause_turn"},
		)
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	var cb completionBody
	if err := json.Unmarshal(readBody(t, resp), &cb); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if len(cb.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(cb.Choices))
	}
	ch := cb.Choices[0]
	if ch.FinishReason == nil || *ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop (unknown reason falls back)", ch.FinishReason)
	}
}

// TestMapFinishReason pins the Agent-as-model boundary. Native tool-related
// reasons are internal lifecycle details, never requests for the OpenAI caller
// to execute a function and invoke this autonomous Agent again.
func TestMapFinishReason(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		toolCalls int
		want      string
	}{
		// A turn that used tools but ended with end_turn/stop is done; clients
		// must not continue the tool loop.
		{"text after tools", "end_turn", 1, "stop"},
		{"explicit stop after tools", "stop", 1, "stop"},
		{"plain end_turn", "end_turn", 0, "stop"},
		{"empty reason, no tools", "", 0, "stop"},
		{"empty reason after internal tool", "", 1, "stop"},
		{"native tool_use reason", "tool_use", 1, "stop"},
		{"native function_calls reason", "function_calls", 1, "stop"},
		{"native requires_action reason", "requires_action", 1, "stop"},
		// Unknown reasons fall back to the safe OpenAI default.
		{"unknown reason", "pause_turn", 0, "stop"},
		{"unknown reason after tools", "idk", 1, "stop"},
		// max_tokens maps to the OpenAI length value.
		{"max_tokens", "max_tokens", 0, "length"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapFinishReason(c.reason); got != c.want {
				t.Errorf("mapFinishReason(%q) = %q, want %q", c.reason, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Chat completions (streaming)
// ---------------------------------------------------------------------------

func TestChatCompletions_Stream(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "Hello"},
			runtime.Event{Type: runtime.EventText, Text: " world"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", true, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	events := splitSSE(t, readBody(t, resp))
	if len(events) < 3 {
		t.Fatalf("sse events = %d, want >= 3 (role + 2 content + final + DONE)", len(events))
	}
	// Last event must be [DONE].
	if events[len(events)-1] != "[DONE]" {
		t.Errorf("last sse event = %q, want [DONE]", events[len(events)-1])
	}
	// Parse the chunks and accumulate content.
	var content strings.Builder
	seenFinish := false
	var lastFinish *string
	for i := 0; i < len(events)-1; i++ {
		var chunk struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(events[i]), &chunk); err != nil {
			t.Fatalf("chunk %d not json: %v (raw %q)", i, err, events[i])
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d object = %q, want chat.completion.chunk", i, chunk.Object)
		}
		if chunk.Model != "codex" {
			t.Errorf("chunk %d model = %q, want codex", i, chunk.Model)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		content.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			seenFinish = true
			lastFinish = chunk.Choices[0].FinishReason
		}
	}
	if got := content.String(); got != "Hello world" {
		t.Errorf("streamed content = %q, want Hello world", got)
	}
	if !seenFinish {
		t.Error("no final chunk with finish_reason seen")
	} else if lastFinish == nil || *lastFinish != "stop" {
		t.Errorf("finish_reason = %v, want stop", lastFinish)
	}
}

func TestChatCompletions_StreamAllAgentsKeepInternalToolsOutOfModelToolCalls(t *testing.T) {
	ts, h, backend := newTestServer(t)
	mustRegister(t, h.catalog.reg, "claude-code", runtime.Descriptor{ModelID: "claude-code"}, nil)
	mustRegister(t, h.catalog.reg, "kimi", runtime.Descriptor{ModelID: "kimi"}, nil)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "before "},
			runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{
				Name: "Bash", Arguments: map[string]any{"command": "pwd"},
			}},
			runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{
				Name: "Bash", Result: "/workspace", IsError: false,
			}},
			runtime.Event{Type: runtime.EventText, Text: "after"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "tool_use"},
		)
	})

	for _, model := range []string{"claude-code", "codex", "kimi"} {
		t.Run(model, func(t *testing.T) {
			resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
				chatReq(model, true, defaultMessages()))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
			}
			events := splitSSE(t, readBody(t, resp))
			var content strings.Builder
			var finish string
			for _, event := range events {
				if event == "[DONE]" {
					continue
				}
				if strings.Contains(event, `"tool_calls"`) {
					t.Fatalf("native tool leaked as OpenAI tool_calls: %s", event)
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(event), &chunk); err != nil {
					t.Fatalf("decode chunk: %v (%q)", err, event)
				}
				if len(chunk.Choices) == 0 {
					continue
				}
				content.WriteString(chunk.Choices[0].Delta.Content)
				if chunk.Choices[0].FinishReason != nil {
					finish = *chunk.Choices[0].FinishReason
				}
			}
			if got := content.String(); got != "before after" {
				t.Errorf("content = %q, want native text around the internal tool", got)
			}
			if finish != "stop" {
				t.Errorf("finish_reason = %q, want stop", finish)
			}
			started := backend.StartRequests()
			if got := started[len(started)-1].ModelID; got != model {
				t.Errorf("backend model = %q, want %q", got, model)
			}
		})
	}
}

func TestChatCompletions_StreamProjectsReasoningAndTerminalToolSummary(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventReasoning, Reasoning: &runtime.Reasoning{ID: "reason-1", Text: "checking workspace"}},
			runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{ID: "tool-1", Name: "Bash", Arguments: map[string]any{"command": "pwd"}}},
			runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{ID: "tool-1", Result: "/workspace", IsError: false}},
			runtime.Event{Type: runtime.EventText, Text: "done"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", true, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	events := splitSSE(t, readBody(t, resp))
	var reasoning []string
	var content strings.Builder
	for _, event := range events {
		if event == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string          `json:"content"`
					ReasoningContent string          `json:"reasoning_content"`
					ToolCalls        json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			t.Fatalf("decode chunk: %v (%q)", err, event)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		if delta.ToolCalls != nil {
			t.Fatalf("native tool leaked as tool_calls: %s", event)
		}
		if delta.ReasoningContent != "" {
			reasoning = append(reasoning, delta.ReasoningContent)
		}
		content.WriteString(delta.Content)
	}
	if len(reasoning) != 2 || reasoning[0] != "checking workspace" {
		t.Fatalf("reasoning = %#v, want native reasoning and one tool summary", reasoning)
	}
	if reasoning[1] != "⏺ Bash\n  ⎿ /workspace\n" {
		t.Fatalf("tool summary = %q, want normalized display text", reasoning[1])
	}
	if content.String() != "done" {
		t.Errorf("content = %q, want done", content.String())
	}
}

func TestFormatToolExecutionSummaryUsesIconNameAndResult(t *testing.T) {
	tests := []struct {
		name string
		tool runtime.ToolCall
		want string
	}{
		{name: "success", tool: runtime.ToolCall{Name: "TaskUpdate", Result: "Updated task #2 status"}, want: "⏺ TaskUpdate\n  ⎿ Updated task #2 status\n"},
		{name: "failure", tool: runtime.ToolCall{Name: "Bash", Result: "command exited with status 1", IsError: true}, want: "⨯ Bash\n  ⎿ command exited with status 1\n"},
		{name: "empty result", tool: runtime.ToolCall{Name: "Read"}, want: "⏺ Read\n"},
		{name: "multiline result", tool: runtime.ToolCall{Name: "Bash", Result: "line one\nline two"}, want: "⏺ Bash\n  ⎿ line one\n    line two\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatToolExecutionSummary(tt.tool); got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatToolExecutionSummaryRedactsAndBoundsResult(t *testing.T) {
	result := "Authorization: Bearer sk-secret\n" + strings.Repeat("x", maxToolSummaryResultBytes+100)
	encoded := formatToolExecutionSummary(runtime.ToolCall{ID: "tool-1", Name: "Bash", Result: result})
	if strings.Contains(encoded, "sk-secret") {
		t.Fatalf("encoded summary leaked bearer token: %q", encoded)
	}
	if !strings.Contains(encoded, "Authorization: ***") {
		t.Fatalf("encoded summary did not preserve redacted marker: %q", encoded)
	}
	if len(encoded) > maxToolSummaryResultBytes+64 {
		t.Fatalf("summary length = %d, want bounded", len(encoded))
	}
}

func TestFormatToolExecutionSummaryBoundsMultilineExpansion(t *testing.T) {
	result := strings.Repeat("x\n", maxToolSummaryResultBytes)
	summary := formatToolExecutionSummary(runtime.ToolCall{Name: "Bash", Result: result})
	if len(summary) > maxToolSummaryResultBytes+64 {
		t.Fatalf("multiline summary length = %d, want bounded", len(summary))
	}
	if !utf8.ValidString(summary) {
		t.Fatalf("multiline summary is invalid UTF-8: %q", summary)
	}
}

func TestFormatToolExecutionSummaryRecursivelyRedactsJSONAndUsesDelimiter(t *testing.T) {
	result := `{"authorization":"Basic abc","nested":{"access_token":"value-access","items":[{"API-Key":"value-api"},{"refresh_token":"value-refresh","id_token":"value-id","client_secret":"value-client","password":"value-password","secret":"value-secret"}]},"cookie":"sid=123","set-cookie":"sid=456","ok":"visible"}`
	encoded := formatToolExecutionSummary(runtime.ToolCall{ID: "tool-1", Name: "Bash", Result: result})
	if !strings.HasSuffix(encoded, "\n") {
		t.Fatalf("summary is missing record delimiter: %q", encoded)
	}
	for _, leak := range []string{"Basic abc", "value-access", "value-api", "value-refresh", "value-id", "value-client", "value-password", "value-secret", "sid=123", "sid=456"} {
		if strings.Contains(encoded, leak) {
			t.Fatalf("encoded summary leaked %q: %q", leak, encoded)
		}
	}
	if !strings.Contains(encoded, `"ok":"visible"`) {
		t.Fatalf("non-secret JSON value was lost: %q", encoded)
	}
}

func TestSanitizeToolSummaryResultHandlesPlainTextAndInvalidUTF8(t *testing.T) {
	plain := "Authorization: Basic abc\nCookie=session=123\napi_key=value-api password=value-password token=value-token"
	sanitized := sanitizeToolSummaryResult(plain)
	for _, leak := range []string{"Basic abc", "session=123", "value-api", "value-password", "value-token"} {
		if strings.Contains(sanitized, leak) {
			t.Fatalf("plain result leaked %q: %q", leak, sanitized)
		}
	}
	invalid := strings.Repeat("x", maxToolSummaryResultBytes) + string([]byte{0xff, 0xfe}) + "tail"
	sanitized = sanitizeToolSummaryResult(invalid)
	if !utf8.ValidString(sanitized) || len(sanitized) > maxToolSummaryResultBytes+3 {
		t.Fatalf("invalid UTF-8 was not safely bounded: len=%d result=%q", len(sanitized), sanitized)
	}
}

func TestRejectServerExecutedToolReplay(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventToolUse, Tool: &runtime.ToolCall{ID: "tool-1", Name: "Bash"}},
			runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{ID: "tool-1", Name: "Bash", Result: "ok"}},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})
	first := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d (body %s)", first.StatusCode, readBody(t, first))
	}
	sessionID := first.Header.Get("X-Gateway-Session-Id")
	if sessionID == "" {
		t.Fatal("first response did not return session id")
	}
	replay := map[string]any{
		"model": "codex",
		"messages": []map[string]any{
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{{
				"id": "tool-1", "type": "function", "function": map[string]any{"name": "Bash", "arguments": "{}"},
			}}},
			{"role": "tool", "tool_call_id": "tool-1", "content": "ok"},
			{"role": "user", "content": "continue"},
		},
		"metadata": map[string]any{"workspace_id": testWorkspace},
	}
	second := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, replay,
		map[string]string{"X-Gateway-Session-Id": sessionID})
	if second.StatusCode != http.StatusBadRequest {
		t.Fatalf("replay status = %d (body %s), want 400", second.StatusCode, readBody(t, second))
	}
	ae := decodeError(t, second)
	if !strings.Contains(ae.Message, "server-executed") {
		t.Fatalf("replay error = %+v, want server-executed explanation", ae)
	}
	if got := backend.Handle(sessionID).SendCount(); got != 1 {
		t.Fatalf("native sends = %d, want 1", got)
	}
}

func TestChatCompletions_StreamIgnoresToolResultAfterFinish(t *testing.T) {
	ts, h, backend := newTestServer(t)
	backend.withScript(func(handle *fakeHandle) {
		handle.emit(
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
			runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{ID: "late-tool", Result: "late"}},
		)
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, chatReq("codex", true, defaultMessages()))
	body := readBody(t, resp)
	if strings.Contains(string(body), "⏺") || strings.Contains(string(body), "⨯") {
		t.Fatalf("late tool result leaked after finish: %s", body)
	}
	sessionID := resp.Header.Get("X-Gateway-Session-Id")
	if h.isServerToolID(sessionID, "late-tool") {
		t.Fatal("late tool result was recorded after finish")
	}
}

func TestServerToolLedgerIsDeterministicAndBounded(t *testing.T) {
	_, h, _ := newTestServer(t)
	const sessionID = "ledger-session"
	for i := 0; i < maxServerToolIDsPerSession; i++ {
		h.recordServerToolID(sessionID, fmt.Sprintf("tool-%03d", i))
	}
	h.recordServerToolID(sessionID, "tool-000")
	for i := 0; i < maxServerToolIDsPerSession; i++ {
		if !h.isServerToolID(sessionID, fmt.Sprintf("tool-%03d", i)) {
			t.Fatalf("duplicate insertion evicted tool-%03d", i)
		}
	}
	h.recordServerToolID(sessionID, "tool-new")
	if h.isServerToolID(sessionID, "tool-000") {
		t.Fatal("oldest tool ID was not evicted")
	}
	if !h.isServerToolID(sessionID, "tool-new") || !h.isServerToolID(sessionID, "tool-127") {
		t.Fatal("newest tool IDs were evicted")
	}
}

func TestServerToolLedgerBoundsSessionsAndConcurrentWrites(t *testing.T) {
	_, h, _ := newTestServer(t)
	var wg sync.WaitGroup
	for i := 0; i < maxServerToolSessions+1; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.recordServerToolID(fmt.Sprintf("session-%04d", i), "tool")
		}(i)
	}
	wg.Wait()
	h.mu.Lock()
	count := len(h.serverToolIDs)
	h.mu.Unlock()
	if count != maxServerToolSessions {
		t.Fatalf("session ledgers = %d, want %d", count, maxServerToolSessions)
	}
}

func TestServerToolLedgerClearsWhenSessionIsDeletedOrRecreated(t *testing.T) {
	ts, h, backend := newTestServer(t)
	backend.withScript(func(handle *fakeHandle) {
		handle.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})
	const sessionID = "reused-session"
	h.recordServerToolID(sessionID, "old-tool")
	if err := h.store.Create(context.Background(), runtime.SessionRecord{ID: sessionID, CallerID: testOwner, ModelID: "codex", WorkspaceID: testWorkspace}); err != nil {
		t.Fatal(err)
	}
	h.deleteSession(context.Background(), sessionID, testOwner)
	if h.isServerToolID(sessionID, "old-tool") {
		t.Fatal("deleting a session retained its server tool ledger")
	}

	h.recordServerToolID(sessionID, "old-tool")
	req := chatReq("codex", false, []map[string]any{{"role": "tool", "tool_call_id": "old-tool", "content": "stale"}, {"role": "user", "content": "new turn"}})
	resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, req, map[string]string{"X-Gateway-Session-Id": sessionID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recreated session inherited stale ledger: status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
}

type failAfterResponseWriter struct {
	header http.Header
	writes int
	failAt int
}

func (w *failAfterResponseWriter) Header() http.Header { return w.header }
func (w *failAfterResponseWriter) WriteHeader(int)     {}
func (w *failAfterResponseWriter) Flush()              {}
func (w *failAfterResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes >= w.failAt {
		return 0, errors.New("client disconnected")
	}
	return len(p), nil
}

func TestStreamRecordsServerToolBeforeClientWriteFailure(t *testing.T) {
	_, h, _ := newTestServer(t)
	handle := newFakeHandle(nil)
	handle.emit(runtime.Event{Type: runtime.EventToolResult, Tool: &runtime.ToolCall{ID: "tool-before-write", Name: "Bash", Result: "ok"}})
	w := &failAfterResponseWriter{header: make(http.Header), failAt: 2}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	h.streamTurn(w, req, context.Background(), "codex", "write-failure-session", testOwner, handle, false)
	if !h.isServerToolID("write-failure-session", "tool-before-write") {
		t.Fatal("server tool ID was lost when its display chunk could not be written")
	}
}

func TestChatCompletions_StreamUsage(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "hi"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
			runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}},
		)
	})
	body := chatReq("codex", true, defaultMessages())
	body["stream_options"] = map[string]any{"include_usage": true}
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	events := splitSSE(t, readBody(t, resp))
	var found bool
	for _, ev := range events {
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   *usageInfo        `json:"usage"`
		}
		if err := json.Unmarshal([]byte(ev), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 && chunk.Usage != nil && chunk.Usage.TotalTokens == 8 {
			found = true
		}
	}
	if !found {
		t.Error("streamed usage chunk (empty choices, total_tokens 8) not found")
	}
}

func TestChatCompletions_StreamAgentError(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "partial"},
			runtime.Event{Type: runtime.EventError, Error: "codex exploded"},
		)
	})
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", true, defaultMessages()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	events := splitSSE(t, readBody(t, resp))
	// [DONE] must still terminate the stream, and an error frame must be present.
	if len(events) == 0 {
		t.Fatal("stream produced no events")
	}
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("last sse event = %q, want [DONE]", events[len(events)-1])
	}
	foundErr := false
	for _, ev := range events[:len(events)-1] {
		var eb errorBody
		if err := json.Unmarshal([]byte(ev), &eb); err == nil && eb.Error.Message != "" {
			foundErr = true
			if eb.Error.Type != "server_error" {
				t.Errorf("error type = %q, want server_error", eb.Error.Type)
			}
		}
	}
	if !foundErr {
		t.Error("no error frame in stream")
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestAuth_Required(t *testing.T) {
	ts, _, _ := newTestServer(t)
	for name, token := range map[string]string{"missing": "", "wrong": "not-the-token"} {
		resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", token, testOwner, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
		ae := decodeError(t, resp)
		if ae.Type != "authentication_error" {
			t.Errorf("%s: error type = %q, want authentication_error", name, ae.Type)
		}
	}
	// Health endpoints must NOT require auth.
	for _, path := range []string{"/health/live", "/health/ready"} {
		resp := doAuthJSON(t, "GET", ts.URL+path, "", testOwner, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s without auth: status = %d, want 200", path, resp.StatusCode)
		}
	}
}

func TestAuth_DisabledWhenNoTokenConfigured(t *testing.T) {
	ts, _, _ := newTestServer(t, func(o *Options) { o.CallerTokens = nil })
	resp := doAuthJSON(t, "GET", ts.URL+"/v1/models", "", testOwner, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 when auth disabled", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Owner isolation + existence leak prevention
// ---------------------------------------------------------------------------

func TestOwnerIsolation_SessionNotLeaked(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "ok"}, runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	// Owner A creates a session implicitly (no gateway session id on the
	// request) and receives it in the X-Gateway-Session-Id response header.
	body := chatReq("codex", false, defaultMessages())
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner A create: status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	sid := resp.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("owner A create did not return X-Gateway-Session-Id")
	}

	// Owner B must not be able to observe A's session: the response must be the
	// same generic not-found/not-authorized (404), never a 403 that leaks that
	// the session exists.
	resp = doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwnerB, body,
		map[string]string{"X-Gateway-Session-Id": sid})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("owner B resume: status = %d, want 404 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	ae := decodeError(t, resp)
	if ae.Code != "session_not_found" {
		t.Errorf("owner B resume code = %q, want session_not_found", ae.Code)
	}

	// Same for the abort endpoint.
	resp = doAuthJSON(t, "POST", ts.URL+"/v1/sessions/"+sid+"/abort", testToken, testOwnerB, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("owner B abort: status = %d, want 404 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	ae = decodeError(t, resp)
	if ae.Code != "session_not_found" {
		t.Errorf("owner B abort code = %q, want session_not_found", ae.Code)
	}

	// A caller may supply its stable business conversation id on the first
	// request. The gateway creates that session and echoes the same id, so an
	// OpenAI-compatible caller can reuse one persistent agent worker without a
	// separate create-session round trip.
	before := len(backend.StartRequests())
	resp = doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"X-Gateway-Session-Id": "business-session-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("caller-provided session create: status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("X-Gateway-Session-Id"); got != "business-session-1" {
		t.Errorf("created session header = %q, want business-session-1", got)
	}
	if got := len(backend.StartRequests()); got != before+1 {
		t.Errorf("caller-provided session start requests = %d, want %d", got, before+1)
	}

	// A second turn with the same caller-provided id reuses the existing
	// execution instead of starting a second worker.
	resp = doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"X-Gateway-Session-Id": "business-session-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("caller-provided session resume: status = %d, want 200 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if got := len(backend.StartRequests()); got != before+1 {
		t.Errorf("resumed caller-provided session started another execution: got %d starts, want %d", got, before+1)
	}
}

// ---------------------------------------------------------------------------
// Client disconnect -> abort
// ---------------------------------------------------------------------------

func TestClientDisconnect_AbortsTurn(t *testing.T) {
	ts, _, backend := newTestServer(t)
	// A handle that never emits anything: the turn would block forever.
	backend.withScript(func(h *fakeHandle) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, err := json.Marshal(chatReq("codex", true, defaultMessages()))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-User-Id", testOwner)
	req.Header.Set("Content-Type", "application/json")

	done := make(chan error, 1)
	var resp *http.Response
	go func() {
		r, err := http.DefaultClient.Do(req)
		resp = r
		done <- err
	}()

	// Wait for the handler to have started the session and delivered the turn.
	sid := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		started := backend.StartRequests()
		if len(started) > 0 {
			sid = started[0].SessionID
			if h := backend.Handle(sid); h != nil && h.SendCount() >= 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sid == "" {
		t.Fatal("session never started")
	}
	h := backend.Handle(sid)
	if h == nil {
		t.Fatal("handle missing")
	}
	if h.SendCount() < 1 {
		t.Fatalf("send count = %d, want >= 1", h.SendCount())
	}

	// Simulate client disconnect.
	cancel()
	<-done
	if resp != nil {
		resp.Body.Close()
	}

	select {
	case <-h.Aborted():
	case <-time.After(3 * time.Second):
		t.Fatal("fake handle Abort was not called after client disconnect")
	}
	_ = resp
}

func TestClientDisconnect_DrainsCancelledTurnBeforeNextTurn(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		if h.SendCount() == 2 {
			h.emit(
				runtime.Event{Type: runtime.EventText, Text: "second-answer"},
				runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
			)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	body, err := json.Marshal(chatReq("codex", true, defaultMessages()))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			requestErr <- err
			return
		}
		response <- resp
	}()
	var firstResp *http.Response
	select {
	case firstResp = <-response:
	case err := <-requestErr:
		t.Fatalf("first stream request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first stream did not return response headers")
	}

	var sid string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		started := backend.StartRequests()
		if len(started) > 0 {
			sid = started[0].SessionID
			if h := backend.Handle(sid); h != nil && h.SendCount() == 1 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sid == "" {
		t.Fatal("first turn did not start")
	}
	handle := backend.Handle(sid)
	handle.setOnAbort(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "cancelled"})
	})
	cancel()
	firstResp.Body.Close()
	select {
	case <-handle.Aborted():
	case <-time.After(3 * time.Second):
		t.Fatal("first turn was not aborted")
	}

	resp2 := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()), map[string]string{"X-Gateway-Session-Id": sid})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second turn status = %d (body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	var completion completionBody
	if err := json.NewDecoder(resp2.Body).Decode(&completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content == nil || *completion.Choices[0].Message.Content != "second-answer" {
		t.Fatalf("second turn response = %#v, want second-answer", completion)
	}
}

type backpressuredAbortHandle struct {
	events chan runtime.Event
}

type slowSuccessfulAbortHandle struct {
	events chan runtime.Event
	closed atomic.Bool
}

type delayedTerminalAbortHandle struct {
	events chan runtime.Event
	closed atomic.Bool
}

func (h *delayedTerminalAbortHandle) Send(context.Context, runtime.Input) error { return nil }
func (h *delayedTerminalAbortHandle) Events() <-chan runtime.Event              { return h.events }
func (h *delayedTerminalAbortHandle) Abort(context.Context) error {
	go func() {
		time.Sleep(700 * time.Millisecond)
		h.events <- runtime.Event{Type: runtime.EventFinish, FinishReason: "cancelled"}
	}()
	return nil
}
func (h *delayedTerminalAbortHandle) Close(context.Context) error {
	h.closed.Store(true)
	return nil
}

func (h *slowSuccessfulAbortHandle) Send(context.Context, runtime.Input) error { return nil }
func (h *slowSuccessfulAbortHandle) Events() <-chan runtime.Event              { return h.events }
func (h *slowSuccessfulAbortHandle) Abort(ctx context.Context) error {
	timer := time.NewTimer(5200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		h.events <- runtime.Event{Type: runtime.EventFinish, FinishReason: "cancelled"}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (h *slowSuccessfulAbortHandle) Close(context.Context) error {
	h.closed.Store(true)
	return nil
}

func (h *backpressuredAbortHandle) Send(context.Context, runtime.Input) error { return nil }
func (h *backpressuredAbortHandle) Events() <-chan runtime.Event              { return h.events }
func (h *backpressuredAbortHandle) Close(context.Context) error               { return nil }
func (h *backpressuredAbortHandle) Abort(context.Context) error {
	h.events <- runtime.Event{Type: runtime.EventFinish, FinishReason: "cancelled"}
	return nil
}

func TestAbortAndDrainConsumesEventsWhileAbortIsRunning(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	handle := &backpressuredAbortHandle{events: make(chan runtime.Event)}
	done := make(chan struct{})
	go func() {
		h.abortAndDrain(context.Background(), "backpressure", handle)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("abortAndDrain deadlocked while Abort waited for event consumption")
	}
}

func TestAbortAndDrainAllowsClaudeInterruptLongerThanFiveSeconds(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	handle := &slowSuccessfulAbortHandle{events: make(chan runtime.Event)}
	h.abortAndDrain(context.Background(), "slow-interrupt", handle)
	if handle.closed.Load() {
		t.Fatal("execution was closed before the native interrupt could settle")
	}
}

func TestAbortAndDrainWaitsForDelayedTerminalEvent(t *testing.T) {
	h, _ := newTestHandler(t, nil)
	handle := &delayedTerminalAbortHandle{events: make(chan runtime.Event, 1)}
	started := time.Now()
	h.abortAndDrain(context.Background(), "delayed-terminal", handle)
	if elapsed := time.Since(started); elapsed < 700*time.Millisecond {
		t.Fatalf("abortAndDrain returned before terminal event arrived: %s", elapsed)
	}
	if handle.closed.Load() {
		t.Fatal("execution was closed despite receiving the delayed terminal event")
	}
	select {
	case event := <-handle.events:
		t.Fatalf("terminal event leaked into the next turn: %#v", event)
	default:
	}
}

// ---------------------------------------------------------------------------
// Abort endpoint
// ---------------------------------------------------------------------------

func TestAbortEndpoint(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {})

	// Start a blocking stream as owner A (session id is generated by the
	// gateway and returned in the response header).
	body := chatReq("codex", true, defaultMessages())
	req, err := http.NewRequest("POST", ts.URL+"/v1/chat/completions", jsonReader(t, body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-User-Id", testOwner)
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
	case <-time.After(5 * time.Second):
		t.Fatal("streaming response never arrived")
	}
	sid := r.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("no X-Gateway-Session-Id in response")
	}
	// Wait until the turn has actually been delivered before aborting.
	h := backend.Handle(sid)
	if h == nil {
		t.Fatal("handle missing")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.SendCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.SendCount() < 1 {
		t.Fatal("turn never delivered")
	}

	// Abort it via the endpoint.
	abortResp := doAuthJSON(t, "POST", ts.URL+"/v1/sessions/"+sid+"/abort", testToken, testOwner, nil)
	if abortResp.StatusCode != http.StatusOK {
		t.Fatalf("abort status = %d (body %s)", abortResp.StatusCode, readBody(t, abortResp))
	}
	select {
	case <-h.Aborted():
	case <-time.After(3 * time.Second):
		t.Fatal("abort endpoint did not reach the handle")
	}
	// The blocking stream must terminate (client sees the stream end).
	b := readBody(t, r)
	if !strings.Contains(string(b), "[DONE]") {
		t.Errorf("stream body missing [DONE], got %q", b)
	}
}

// ---------------------------------------------------------------------------
// Single active turn
// ---------------------------------------------------------------------------

func TestSessionBusy_ConcurrentTurn(t *testing.T) {
	// A short turn timeout so the first blocking turn terminates promptly when
	// the test ends.
	ts, _, backend := newTestServer(t, func(o *Options) {
		o.TurnTimeout = 300 * time.Millisecond
	})
	backend.withScript(func(h *fakeHandle) {})

	// Start a blocking turn (session id generated by the gateway).
	body := chatReq("codex", true, defaultMessages())
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", jsonReader(t, body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-User-Id", testOwner)
	req.Header.Set("Content-Type", "application/json")
	firstResp := make(chan *http.Response, 1)
	go func() {
		r, _ := http.DefaultClient.Do(req)
		firstResp <- r
	}()

	// Wait for the session id and confirm the turn is active.
	var sid string
	select {
	case r := <-firstResp:
		sid = r.Header.Get("X-Gateway-Session-Id")
		if sid == "" {
			t.Fatal("no X-Gateway-Session-Id in response")
		}
		go func() {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}()
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never responded")
	}
	h := backend.Handle(sid)
	if h == nil {
		t.Fatal("handle missing")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.SendCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// A second concurrent turn on the same session must be rejected.
	dup := chatReq("codex", true, defaultMessages())
	resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, dup,
		map[string]string{"X-Gateway-Session-Id": sid})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second turn status = %d, want 409 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	ae := decodeError(t, resp)
	if ae.Code != "session_busy" {
		t.Errorf("code = %q, want session_busy", ae.Code)
	}
}

// ---------------------------------------------------------------------------
// Multi-turn reuse
// ---------------------------------------------------------------------------

func TestMultiTurn_ReusesExecution(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "turn done"}, runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	body := chatReq("codex", false, defaultMessages())

	// Turn 1 creates the execution; the gateway returns the session id.
	resp1 := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("turn1 status = %d (body %s)", resp1.StatusCode, readBody(t, resp1))
	}
	sid := resp1.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("no X-Gateway-Session-Id in turn 1 response")
	}
	if got := len(backend.StartRequests()); got != 1 {
		t.Fatalf("start requests after turn 1 = %d, want 1", got)
	}

	// Turn 2 on the same session (via the gateway session header) must reuse
	// the same execution handle, not start a second one.
	resp2 := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"X-Gateway-Session-Id": sid})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("turn2 status = %d (body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	if got := len(backend.StartRequests()); got != 1 {
		t.Errorf("start requests after turn 2 = %d, want 1 (execution reused)", got)
	}
	h := backend.Handle(sid)
	if h == nil || h.SendCount() != 2 {
		t.Errorf("handle sends = %d, want 2", h.SendCount())
	}
}

// TestDeadHandleBetweenTurns_RecoversWithFreshExecution is the regression test
// for F1: a worker/handle that dies while the session is IDLE between turns
// must be dropped and replaced with a FRESH execution on the next request.
// Before the fix the dead handle stayed in the map, Send failed terminally,
// and every subsequent request to the session returned 500 "failed to send
// turn" until the gateway restarted.
func TestDeadHandleBetweenTurns_RecoversWithFreshExecution(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "turn done"}, runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	body := chatReq("codex", false, defaultMessages())

	// Turn 1 creates the session and its execution.
	resp1 := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("turn1 status = %d (body %s)", resp1.StatusCode, readBody(t, resp1))
	}
	sid := resp1.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("no X-Gateway-Session-Id in turn 1 response")
	}
	if got := len(backend.StartRequests()); got != 1 {
		t.Fatalf("start requests after turn 1 = %d, want 1", got)
	}

	// The worker/CLI exits while the session is idle between turns.
	backend.Handle(sid).markDead()

	// Turn 2 on the same session must recover: drop the dead handle, start a
	// fresh execution, and deliver the turn — NOT return 500 forever.
	resp2 := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"X-Gateway-Session-Id": sid})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("turn2 status = %d, want 200 (dead handle must recover with a fresh execution, body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	if got := len(backend.StartRequests()); got != 2 {
		t.Errorf("start requests after recovery = %d, want 2 (dead handle replaced by a fresh execution)", got)
	}
	fresh := backend.Handle(sid)
	if fresh == nil {
		t.Fatal("no fresh handle registered for the session after recovery")
	}
	if fresh.SendCount() != 1 {
		t.Errorf("fresh handle sends = %d, want 1 (the turn was delivered to the fresh execution)", fresh.SendCount())
	}
}

type racingDeadHandle struct {
	*fakeHandle
	done        chan struct{}
	terminating atomic.Bool
}

func newRacingDeadHandle() *racingDeadHandle {
	return &racingDeadHandle{fakeHandle: newFakeHandle(nil), done: make(chan struct{})}
}

func (h *racingDeadHandle) Send(context.Context, runtime.Input) error {
	go func() {
		time.Sleep(10 * time.Millisecond)
		h.terminating.Store(true)
		time.Sleep(500 * time.Millisecond)
		h.markDead()
		close(h.done)
	}()
	return errors.New("worker transport closed before supervisor published termination")
}

func (h *racingDeadHandle) Done() <-chan struct{} { return h.done }
func (h *racingDeadHandle) Terminating() bool     { return h.terminating.Load() }

// TestDeadHandleSendRace_WaitsForTerminationAndRecovers covers the real
// worker-exit race: Send observes the broken transport just before the
// supervisor publishes Closed. The turn must wait for that terminal signal,
// replace the stale handle, and deliver the input to a fresh execution.
func TestDeadHandleSendRace_WaitsForTerminationAndRecovers(t *testing.T) {
	h, backend := newTestHandler(t, nil)
	backend.withScript(func(fresh *fakeHandle) {
		fresh.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})
	stale := newRacingDeadHandle()
	h.registerHandle("session-race", stale)
	startReq := runtime.StartRequest{
		ModelID: "codex", SessionID: "session-race", CallerID: testOwner, WorkspaceID: testWorkspace,
		Metadata: map[string]string{"native_session_id": "native-existing"},
	}

	fresh, err := h.deliverTurn(context.Background(), "session-race", stale, startReq, runtime.Input{
		Messages: []runtime.Message{{Role: "user", Content: "second turn"}},
	})
	if err != nil {
		t.Fatalf("deliver second turn during worker termination race: %v", err)
	}
	if fresh == stale {
		t.Fatal("stale execution handle was not replaced")
	}
	if got := len(backend.StartRequests()); got != 1 {
		t.Fatalf("fresh execution starts = %d, want 1", got)
	}
	if got := backend.StartRequests()[0].Metadata["native_session_id"]; got != "native-existing" {
		t.Fatalf("native resume id = %q, want native-existing", got)
	}
	if got := backend.Handle("session-race").SendCount(); got != 1 {
		t.Fatalf("fresh execution sends = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Normalizer: no client-controlled workdir; messages/tools/metadata mapping
// ---------------------------------------------------------------------------

func TestNormalizer_NoWorkDirInjection(t *testing.T) {
	ts, _, backend := newTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventText, Text: "ok"}, runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	body := map[string]any{
		"model": "codex",
		"messages": []map[string]any{
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "do something"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "Bash", "arguments": `{"command":"ls"}`,
				}},
			}},
			{"role": "tool", "tool_call_id": "call_1", "content": "file list"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{
				"name": "Bash", "description": "run a shell command",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		},
		"metadata": map[string]any{
			"workdir":           "/etc",
			"cwd":               "/tmp",
			"working_directory": "/var",
			"workspace_id":      "ws-norm",
			"trace_id":          "abc",
			"native_session_id": "attacker-native",
			"codex_thread_id":   "attacker-thread",
			"claude_session_id": "attacker-claude",
			"kimi_session_id":   "attacker-kimi",
			"Native_Session_ID": "attacker-cased",
		},
	}
	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}
	sid := resp.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("no X-Gateway-Session-Id in response")
	}

	reqs := backend.StartRequests()
	if len(reqs) != 1 {
		t.Fatalf("start requests = %d, want 1", len(reqs))
	}
	sr := reqs[0]
	// The gateway must route workspace_id from metadata to the start request.
	if sr.WorkspaceID != "ws-norm" {
		t.Errorf("workspace id = %q, want ws-norm", sr.WorkspaceID)
	}
	if sr.SessionID != sid {
		t.Errorf("session id = %q, want gateway-generated %q", sr.SessionID, sid)
	}
	// Absolute-path metadata must never be forwarded to the execution.
	for _, key := range []string{"workdir", "cwd", "working_directory"} {
		if _, ok := sr.Metadata[key]; ok {
			t.Errorf("metadata %q leaked into the start request: %v", key, sr.Metadata)
		}
	}
	// Server-owned native resume ids must never be client-supplied: a caller
	// could otherwise select or override the native session/thread the gateway
	// resumes into. The gateway injects the sole canonical native_session_id
	// from its own SessionRecord.
	for _, key := range []string{"native_session_id", "codex_thread_id", "claude_session_id", "kimi_session_id"} {
		if v, ok := sr.Metadata[key]; ok {
			t.Errorf("resume id %q leaked into the start request: %q", key, v)
		}
	}
	if sr.Metadata["trace_id"] != "abc" {
		t.Errorf("trace_id metadata lost: %v", sr.Metadata)
	}
	if sr.ModelID != "codex" || sr.CallerID != testOwner {
		t.Errorf("start request = %+v, want model codex owner %s", sr, testOwner)
	}
	if h := backend.Handle(sid); h == nil {
		t.Fatal("handle missing")
	}

	// Resume routing via metadata["session_id"] (not the header) must resolve
	// to the same session and reuse the execution.
	resumeBody := map[string]any{
		"model":    "codex",
		"messages": defaultMessages(),
		"metadata": map[string]any{"session_id": sid, "workspace_id": "ws-norm"},
	}
	resp2 := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, resumeBody)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("resume status = %d (body %s)", resp2.StatusCode, readBody(t, resp2))
	}
	if got := len(backend.StartRequests()); got != 1 {
		t.Errorf("start requests after metadata-session resume = %d, want 1 (execution reused)", got)
	}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func TestHealth(t *testing.T) {
	ts, _, backend := newTestServer(t)
	resp := doAuthJSON(t, "GET", ts.URL+"/health/live", "", testOwner, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("live status = %d, want 200", resp.StatusCode)
	}
	resp = doAuthJSON(t, "GET", ts.URL+"/health/ready", "", testOwner, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("ready status = %d, want 200", resp.StatusCode)
	}
	// Preflight failure must make readiness 503.
	backend.preflightErr = errors.New("nsjail binary missing")
	resp = doAuthJSON(t, "GET", ts.URL+"/health/ready", "", testOwner, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("ready with preflight failure status = %d, want 503", resp.StatusCode)
	}
}

// jsonReader builds an io.Reader from a value.
func jsonReader(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}
