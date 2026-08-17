package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// Idempotency-Key tests (ROADMAP §4.2 exit criteria). Every test wires a real
// in-memory IdempotencyStore via Options; existing tests keep Idempotency nil,
// which pins the "no store -> feature off, behavior unchanged" contract.
//
// Note: newTestServer's default Now is a fixed PAST time, which would expire
// every record instantly (ExpiresAt = now+TTL is in the past). Idempotency
// tests therefore reset Now to nil (real time).

type idemConflictBody struct {
	Error  apiError `json:"error"`
	RunID  string   `json:"run_id"`
	Status string   `json:"status"`
}

func newIdemTestServer(t *testing.T, mut ...func(*Options)) (*httptest.Server, *Handler, *fakeBackend) {
	t.Helper()
	base := []func(*Options){
		func(o *Options) {
			o.Idempotency = runtime.NewMemoryIdempotencyStore()
			o.Now = nil // real time: idempotency TTLs are wall-clock
		},
	}
	return newTestServer(t, append(base, mut...)...)
}

func decodeIdemConflict(t *testing.T, resp *http.Response) idemConflictBody {
	t.Helper()
	var body idemConflictBody
	if err := json.Unmarshal(readBody(t, resp), &body); err != nil {
		t.Fatalf("decode idempotency conflict body: %v", err)
	}
	return body
}

// TestIdempotency_DuplicateActiveReturns409: a duplicate arriving while the
// original turn is in flight is rejected with 409 in_progress carrying the
// owning run id; no second turn is ever started.
func TestIdempotency_DuplicateActiveReturns409(t *testing.T) {
	release := make(chan struct{})
	ts, _, backend := newIdemTestServer(t, func(o *Options) {
		o.TurnTimeout = 5 * time.Second
	})
	backend.withScript(func(h *fakeHandle) {
		<-release
		h.emitThenClose(
			runtime.Event{Type: runtime.EventText, Text: "done"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})
	t.Cleanup(func() { close(release) })

	body := chatReq("codex", true, defaultMessages())
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", jsonReader(t, body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "key-active")
	firstCh := make(chan *http.Response, 1)
	go func() {
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		firstCh <- r
	}()

	var firstResp *http.Response
	select {
	case firstResp = <-firstCh:
	case <-time.After(5 * time.Second):
		t.Fatal("first (streaming) turn never responded")
	}
	t.Cleanup(func() {
		io.Copy(io.Discard, firstResp.Body)
		firstResp.Body.Close()
	})
	runID := firstResp.Header.Get("X-Gateway-Run-Id")
	if runID == "" {
		t.Fatal("first response has no X-Gateway-Run-Id")
	}
	sid := firstResp.Header.Get("X-Gateway-Session-Id")
	h := backend.Handle(sid)
	if h == nil {
		t.Fatal("handle missing")
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.SendCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// The identical request with the same key must be rejected, not admitted.
	dup := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"Idempotency-Key": "key-active"})
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409 (body %s)", dup.StatusCode, readBody(t, dup))
	}
	cb := decodeIdemConflict(t, dup)
	if cb.Error.Code != "idempotency_in_progress" {
		t.Errorf("code = %q, want idempotency_in_progress", cb.Error.Code)
	}
	if cb.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress", cb.Status)
	}
	if cb.RunID != runID {
		t.Errorf("run_id = %q, want the in-flight run %q", cb.RunID, runID)
	}
	if h.SendCount() != 1 {
		t.Errorf("sends = %d, want 1 (no second turn started)", h.SendCount())
	}
}

// TestIdempotency_ReplayCompletedNonStream: a duplicate of a completed
// non-streaming request gets the stored response byte-for-byte (marked via
// Idempotency-Replayed) and never starts a second turn. The replay also echoes
// the original session id, so a retried FIRST request (no session id supplied)
// still learns it.
func TestIdempotency_ReplayCompletedNonStream(t *testing.T) {
	ts, _, backend := newIdemTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emitThenClose(
			runtime.Event{Type: runtime.EventText, Text: "Hello world"},
			runtime.Event{Type: runtime.EventUsage, Usage: &runtime.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	body := chatReq("codex", false, defaultMessages())
	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"Idempotency-Key": "key-replay"})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d (body %s)", first.StatusCode, readBody(t, first))
	}
	firstBody := readBody(t, first)
	sid := first.Header.Get("X-Gateway-Session-Id")
	if sid == "" {
		t.Fatal("first response has no session header")
	}

	second := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"Idempotency-Key": "key-replay"})
	if second.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d (body %s)", second.StatusCode, readBody(t, second))
	}
	if second.Header.Get("Idempotency-Replayed") != "true" {
		t.Error("replay missing Idempotency-Replayed: true header")
	}
	if got := string(readBody(t, second)); got != string(firstBody) {
		t.Errorf("replay body = %s, want byte-identical %s", got, firstBody)
	}
	if got := second.Header.Get("X-Gateway-Session-Id"); got != sid {
		t.Errorf("replay session header = %q, want %q", got, sid)
	}
	if h := backend.Handle(sid); h == nil || h.SendCount() != 1 {
		t.Errorf("sends = %v, want exactly 1 turn", h)
	}
}

// TestIdempotency_FingerprintMismatch409: reusing the key with a different
// payload is a conflict.
func TestIdempotency_FingerprintMismatch409(t *testing.T) {
	ts, _, backend := newIdemTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emitThenClose(
			runtime.Event{Type: runtime.EventText, Text: "ok"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()), map[string]string{"Idempotency-Key": "key-mismatch"})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d (body %s)", first.StatusCode, readBody(t, first))
	}

	other := chatReq("codex", false, []map[string]any{{"role": "user", "content": "different"}})
	dup := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, other,
		map[string]string{"Idempotency-Key": "key-mismatch"})
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409 (body %s)", dup.StatusCode, readBody(t, dup))
	}
	if cb := decodeIdemConflict(t, dup); cb.Error.Code != "idempotency_conflict" {
		t.Errorf("code = %q, want idempotency_conflict", cb.Error.Code)
	}
}

// TestIdempotency_ExpiredKeyReoccupiable: once the record's TTL passes, the
// key may be claimed again and the identical request executes a fresh turn.
func TestIdempotency_ExpiredKeyReoccupiable(t *testing.T) {
	ts, _, backend := newIdemTestServer(t, func(o *Options) {
		o.IdempotencyTTL = 100 * time.Millisecond
	})
	// No channel close: the persistent handle is reused across both turns.
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "ok"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	body := chatReq("codex", false, defaultMessages())
	headers := map[string]string{"Idempotency-Key": "key-ttl", "X-Gateway-Session-Id": "sess-ttl"}
	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d (body %s)", first.StatusCode, readBody(t, first))
	}
	_ = readBody(t, first)

	time.Sleep(200 * time.Millisecond)
	second := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("post-expiry status = %d, want 200 (key re-occupied; body %s)", second.StatusCode, readBody(t, second))
	}
	_ = readBody(t, second)
	if second.Header.Get("Idempotency-Replayed") != "" {
		t.Error("post-expiry response must be a fresh execution, not a replay")
	}
	if h := backend.Handle("sess-ttl"); h == nil || h.SendCount() != 2 {
		t.Errorf("sends = %v, want 2 (fresh turn after expiry)", h)
	}
}

// TestIdempotency_OutcomeUnknownNeverRerun: a turn whose completion outcome is
// unknown (event stream closed without a finish marker) completes the key
// WITHOUT a body; a duplicate gets a 409 status reference and the turn is
// never implicitly re-run.
func TestIdempotency_OutcomeUnknownNeverRerun(t *testing.T) {
	ts, h, backend := newIdemTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		// The execution dies mid-turn: text but no finish marker.
		h.emitThenClose(runtime.Event{Type: runtime.EventText, Text: "partial"})
	})

	body := chatReq("codex", false, defaultMessages())
	headers := map[string]string{"Idempotency-Key": "key-unknown"}
	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d (body %s)", first.StatusCode, readBody(t, first))
	}
	runID := first.Header.Get("X-Gateway-Run-Id")
	sid := first.Header.Get("X-Gateway-Session-Id")

	// The run record must reflect the unconfirmed outcome.
	run, err := h.runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != runtime.RunOutcomeUnknown {
		t.Fatalf("run status = %q, want %q", run.Status, runtime.RunOutcomeUnknown)
	}

	second := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409 (body %s)", second.StatusCode, readBody(t, second))
	}
	cb := decodeIdemConflict(t, second)
	if cb.Error.Code != "idempotency_not_replayable" {
		t.Errorf("code = %q, want idempotency_not_replayable", cb.Error.Code)
	}
	if cb.Status != string(runtime.RunOutcomeUnknown) {
		t.Errorf("status = %q, want %q", cb.Status, runtime.RunOutcomeUnknown)
	}
	if cb.RunID != runID {
		t.Errorf("run_id = %q, want the original run %q", cb.RunID, runID)
	}
	if h := backend.Handle(sid); h == nil || h.SendCount() != 1 {
		t.Errorf("sends = %v, want 1 (never implicitly re-run)", h)
	}
}

// TestIdempotency_StreamingNotReplayable: streaming (SSE) responses are never
// stored; a duplicate of a completed streaming request gets a 409 status
// reference. Documented contract: SSE cannot be replayed.
func TestIdempotency_StreamingNotReplayable(t *testing.T) {
	ts, _, backend := newIdemTestServer(t)
	// No channel close: a closed channel ends the stream as outcome_unknown;
	// this test pins the replay rejection of a SUCCESSFUL streaming turn.
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "streamed"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	body := chatReq("codex", true, defaultMessages())
	headers := map[string]string{"Idempotency-Key": "key-stream"}
	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d (body %s)", first.StatusCode, readBody(t, first))
	}
	runID := first.Header.Get("X-Gateway-Run-Id")
	sid := first.Header.Get("X-Gateway-Session-Id")
	_ = readBody(t, first) // drain the SSE stream

	second := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409 (body %s)", second.StatusCode, readBody(t, second))
	}
	cb := decodeIdemConflict(t, second)
	if cb.Error.Code != "idempotency_not_replayable" {
		t.Errorf("code = %q, want idempotency_not_replayable", cb.Error.Code)
	}
	if cb.Status != string(runtime.RunSucceeded) {
		t.Errorf("status = %q, want %q", cb.Status, runtime.RunSucceeded)
	}
	if cb.RunID != runID {
		t.Errorf("run_id = %q, want %q", cb.RunID, runID)
	}
	if h := backend.Handle(sid); h == nil || h.SendCount() != 1 {
		t.Errorf("sends = %v, want 1", h)
	}
}

// TestIdempotency_NoKeyBehaviorUnchanged: with the store wired but no
// Idempotency-Key header, identical requests each execute their own turn.
func TestIdempotency_NoKeyBehaviorUnchanged(t *testing.T) {
	ts, _, backend := newIdemTestServer(t)
	// No channel close: the persistent handle serves both turns.
	backend.withScript(func(h *fakeHandle) {
		h.emit(
			runtime.Event{Type: runtime.EventText, Text: "ok"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	body := chatReq("codex", false, defaultMessages())
	headers := map[string]string{"X-Gateway-Session-Id": "sess-nokey"}
	for i := 0; i < 2; i++ {
		resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d (body %s)", i, resp.StatusCode, readBody(t, resp))
		}
		_ = readBody(t, resp)
	}
	if h := backend.Handle("sess-nokey"); h == nil || h.SendCount() != 2 {
		t.Errorf("sends = %v, want 2 (no dedup without a key)", h)
	}
}

// TestIdempotency_CrossCallerIsolation: the same key under a different caller
// is an independent claim; neither tenant observes the other's record.
func TestIdempotency_CrossCallerIsolation(t *testing.T) {
	ts, _, backend := newIdemTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emitThenClose(
			runtime.Event{Type: runtime.EventText, Text: "ok"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	body := chatReq("codex", false, defaultMessages())
	a := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"Idempotency-Key": "key-shared", "X-Gateway-Session-Id": "sess-a"})
	if a.StatusCode != http.StatusOK {
		t.Fatalf("caller A status = %d (body %s)", a.StatusCode, readBody(t, a))
	}
	_ = readBody(t, a)
	b := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testTokenB, testOwnerB, body,
		map[string]string{"Idempotency-Key": "key-shared", "X-Gateway-Session-Id": "sess-b"})
	if b.StatusCode != http.StatusOK {
		t.Fatalf("caller B status = %d, want 200 (independent claim; body %s)", b.StatusCode, readBody(t, b))
	}
	_ = readBody(t, b)
	if h := backend.Handle("sess-b"); h == nil || h.SendCount() != 1 {
		t.Errorf("caller B sends = %v, want 1 (own execution)", h)
	}
}

// TestIdempotency_PreExecutionRejectionReleasesKey: a request rejected BEFORE
// any execution (here: the session already has an active turn) releases the
// key, so the caller may retry the identical request once the session is free.
func TestIdempotency_PreExecutionRejectionReleasesKey(t *testing.T) {
	ts, h, backend := newIdemTestServer(t)
	backend.withScript(func(h *fakeHandle) {
		h.emitThenClose(
			runtime.Event{Type: runtime.EventText, Text: "ok"},
			runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"},
		)
	})

	body := chatReq("codex", false, defaultMessages())
	headers := map[string]string{"X-Gateway-Session-Id": "sess-busy", "Idempotency-Key": "key-busy"}

	// Create the session with a first, unkeyed request.
	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body,
		map[string]string{"X-Gateway-Session-Id": "sess-busy"})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("setup status = %d (body %s)", first.StatusCode, readBody(t, first))
	}
	_ = readBody(t, first)

	// Force the session into turn-active and confirm the keyed request is
	// rejected with session_busy.
	if err := h.store.BeginTurn(context.Background(), "sess-busy", testOwner); err != nil {
		t.Fatalf("force BeginTurn: %v", err)
	}
	busy := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if busy.StatusCode != http.StatusConflict {
		t.Fatalf("busy status = %d, want 409 (body %s)", busy.StatusCode, readBody(t, busy))
	}
	if ae := decodeError(t, busy); ae.Code != "session_busy" {
		t.Fatalf("busy code = %q, want session_busy", ae.Code)
	}
	if err := h.store.EndTurn(context.Background(), "sess-busy", testOwner); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}

	// The key was released: the identical retry executes normally.
	retry := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner, body, headers)
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (key released; body %s)", retry.StatusCode, readBody(t, retry))
	}
	_ = readBody(t, retry)
}
