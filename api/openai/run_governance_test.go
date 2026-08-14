package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/metrics"
	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// TestAdmissionSlotReleasedOnBeginTurnFailure is a regression test for the
// Phase 2 code review (P1): a request that passes admission but then fails
// BeginTurn (session busy) used to return WITHOUT releasing the active-run
// slot, permanently inflating the counters until every later request was
// wrongly 429'd. The release defer must be installed before BeginTurn.
func TestAdmissionSlotReleasedOnBeginTurnFailure(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTurn := func() { releaseOnce.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	defer func() {
		releaseTurn()
		wg.Wait()
	}()

	admit := newTestAdmission(AdmissionLimits{MaxActiveRuns: 2, MaxActiveRunsPerCaller: 2})
	ts, _, backend := newTestServer(t, func(o *Options) {
		o.Admission = admit
		o.TurnTimeout = 60 * time.Second // block on the release channel, not the deadline
	})
	// Request 1 blocks mid-turn until release is closed; later handles run
	// straight to the finish marker.
	var sends atomic.Int64
	backend.withScript(func(h *fakeHandle) {
		if sends.Add(1) == 1 {
			<-release
		}
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	// First request holds the turn on session-a (and one admission slot).
	reqDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(reqDone)
		resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq("codex", false, defaultMessages()),
			map[string]string{"X-Gateway-Session-Id": "sess-admit-a", "X-Workspace-Id": "ws-a"})
		if resp.StatusCode != http.StatusOK {
			t.Errorf("first request status = %d (%s)", resp.StatusCode, readBody(t, resp))
		}
	}()
	// Wait until the turn is actually in flight (registered + turn active).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if admit.ActiveRuns() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := admit.ActiveRuns(); got != 1 {
		t.Fatalf("first request never acquired: active runs = %d", got)
	}

	// Concurrent second request to the SAME session: admission passes (1 < 2)
	// but BeginTurn fails with session-busy. This is the leaking path.
	resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()),
		map[string]string{"X-Gateway-Session-Id": "sess-admit-a", "X-Workspace-Id": "ws-a"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second request status = %d, want 409 session busy (body %s)", resp.StatusCode, readBody(t, resp))
	}

	// The rejected request must have released its slot: exactly one run stays
	// admitted. Pre-fix this was 2 (one leaked forever).
	if got := admit.ActiveRuns(); got != 1 {
		t.Fatalf("active runs after BeginTurn failure = %d, want 1 (slot leaked)", got)
	}

	// A request for a NEW session must still be admitted, not 429'd.
	resp = doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()),
		map[string]string{"X-Gateway-Session-Id": "sess-admit-b", "X-Workspace-Id": "ws-b"})
	if resp.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("new-session request wrongly 429'd after a BeginTurn failure (body %s)", readBody(t, resp))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new-session request status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}

	// Unblock the first turn and let everything settle: no slot may remain.
	releaseTurn()
	<-reqDone
	if got := admit.ActiveRuns(); got != 0 {
		t.Fatalf("active runs after all requests settled = %d, want 0", got)
	}
}

// TestChatCompletions_WorkerCapacityRejected429 is a regression test for the
// Phase 2 code review (P1): a backend at max_workers used to surface as a 500
// start_failed AFTER the session record and turn state were built. The
// overload contract (O-F10) requires 429 + Retry-After both for the admission
// precheck and for the authoritative backend rejection.
func TestChatCompletions_WorkerCapacityRejected429(t *testing.T) {
	t.Run("backend capacity error maps to 429", func(t *testing.T) {
		ts, h, backend := newTestServer(t)
		backend.mu.Lock()
		backend.startErr = fmt.Errorf("backend: %w", runtime.ErrCapacityExceeded)
		backend.mu.Unlock()

		resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq("codex", false, defaultMessages()))
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (body %s)", resp.StatusCode, readBody(t, resp))
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Error("429 response missing Retry-After header")
		}
		ae := decodeError(t, resp)
		if ae.Type != errorTypeRateLimit || ae.Code != "workers_max_workers" {
			t.Errorf("error = %+v, want rate_limit workers_max_workers", ae)
		}
		// The run must be terminal with the capacity error code.
		runID := resp.Header.Get("X-Gateway-Run-Id")
		rec, err := h.runs.Get(context.Background(), runID)
		if err != nil {
			t.Fatalf("run record missing: %v", err)
		}
		if rec.Status != runtime.RunFailed || rec.ErrorCode != "worker_capacity" {
			t.Errorf("run = %s/%s, want failed/worker_capacity", rec.Status, rec.ErrorCode)
		}
	})
	t.Run("admission precheck rejects before session churn", func(t *testing.T) {
		live := func() int { return 1 }
		admit := NewAdmissionController(AdmissionLimits{MaxWorkers: 1}, live, nil)
		ts, _, backend := newTestServer(t, func(o *Options) { o.Admission = admit })
		resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq("codex", false, defaultMessages()))
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429 (body %s)", resp.StatusCode, readBody(t, resp))
		}
		if got := len(backend.StartRequests()); got != 0 {
			t.Errorf("backend started %d executions, want 0 (reject before start)", got)
		}
	})
}

// TestChatCompletions_SessionCapacityRejected429 verifies the global/per-caller
// session record cap surfaces as 429 + Retry-After (O-F10) instead of a 500 or
// silent unbounded growth.
func TestChatCompletions_SessionCapacityRejected429(t *testing.T) {
	// Built by hand (not newTestServer): the shared harness replaces Store,
	// and this test needs a capacity-capped store.
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	backend := newFakeBackend()
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})
	h := NewHandler(Options{
		Registry:     reg,
		Store:        runtime.NewCappedMemorySessionStore(runtime.SessionLimits{MaxPerCaller: 1}),
		Backend:      backend,
		CallerTokens: map[string]string{testToken: testOwner},
		TurnTimeout:  5 * time.Second,
		UsageGrace:   15 * time.Millisecond,
	})
	t.Cleanup(h.Close)
	ts := httptestServer(t, h)

	first := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()),
		map[string]string{"X-Gateway-Session-Id": "sess-cap-1", "X-Workspace-Id": "ws-1"})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first session status = %d (body %s)", first.StatusCode, readBody(t, first))
	}

	second := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()),
		map[string]string{"X-Gateway-Session-Id": "sess-cap-2", "X-Workspace-Id": "ws-2"})
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second session status = %d, want 429 (body %s)", second.StatusCode, readBody(t, second))
	}
	if second.Header.Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	ae := decodeError(t, second)
	if ae.Type != errorTypeRateLimit || ae.Code != "sessions_cap_reached" {
		t.Errorf("error = %+v, want rate_limit sessions_cap_reached", ae)
	}
	// Resuming the EXISTING session must still work — the cap limits new
	// records, not turns on owned sessions.
	third := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()),
		map[string]string{"X-Gateway-Session-Id": "sess-cap-1", "X-Workspace-Id": "ws-1"})
	if third.StatusCode != http.StatusOK {
		t.Fatalf("existing session turn status = %d (body %s)", third.StatusCode, readBody(t, third))
	}
}

// TestHandlerPrunesTerminalRuns is a regression test for the Phase 2 code
// review (P1): the prune loop only evicted session records, so every turn
// leaked one RunRecord for the process lifetime. Terminal runs older than
// RunRecordTTL must be deleted by the same loop.
//
// The clock is FROZEN (Options.Now): the real-clock version of this test was
// flaky under -race because waiting for the old record to be pruned advanced
// wall time past the TTL, letting a later tick legitimately prune the fresh
// record too. With a frozen clock only the pre-aged record ever expires.
func TestHandlerPrunesTerminalRuns(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	frozen := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	h := NewHandler(Options{
		Registry:         reg,
		Store:            runtime.NewMemorySessionStore(),
		Backend:          newFakeBackend(),
		SessionRecordTTL: -1, // disable session-record expiry to isolate run pruning
		RunRecordTTL:     time.Hour,
		PruneInterval:    5 * time.Millisecond,
		Now:              func() time.Time { return frozen },
	})
	t.Cleanup(h.Close)

	ctx := context.Background()
	now := h.now()
	// One terminal (old) run, one non-terminal run, one terminal-but-fresh run.
	_ = h.runs.Create(ctx, runtime.RunRecord{ID: "run-old", SessionID: "s", CallerID: "c", StartedAt: now.Add(-2 * time.Hour)})
	_, _ = h.runs.Update(ctx, "run-old", func(r *runtime.RunRecord) {
		r.Status = runtime.RunSucceeded
		r.FinishedAt = now.Add(-2 * time.Hour)
	})
	_ = h.runs.Create(ctx, runtime.RunRecord{ID: "run-live", SessionID: "s", CallerID: "c"})
	_ = h.runs.Create(ctx, runtime.RunRecord{ID: "run-fresh", SessionID: "s", CallerID: "c", StartedAt: now})
	_, _ = h.runs.Update(ctx, "run-fresh", func(r *runtime.RunRecord) {
		r.Status = runtime.RunFailed
		r.FinishedAt = now
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.runs.Get(ctx, "run-old"); err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := h.runs.Get(ctx, "run-old"); err == nil {
		t.Fatal("expired terminal run was never pruned")
	}
	// Non-terminal and not-yet-expired runs survive (clock is frozen, so the
	// fresh terminal record can never cross the TTL).
	for _, id := range []string{"run-live", "run-fresh"} {
		if _, err := h.runs.Get(ctx, id); err != nil {
			t.Fatalf("%s should survive prune: %v", id, err)
		}
	}
}

// TestFinishRunMetricsMatrix verifies the O-F11 metric coverage per terminal
// state: runs_total per status, plus the dedicated abort/timeout/unknown
// counters.
func TestFinishRunMetricsMatrix(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	h := NewHandler(Options{
		Registry: reg,
		Store:    runtime.NewMemorySessionStore(),
		Backend:  newFakeBackend(),
		Metrics:  metrics.NewRegistry(),
	})
	t.Cleanup(h.Close)

	started := h.now().Add(-time.Second)
	statuses := []runtime.RunStatus{
		runtime.RunSucceeded,
		runtime.RunFailed,
		runtime.RunCancelled,
		runtime.RunTimedOut,
		runtime.RunOutcomeUnknown,
	}
	for _, st := range statuses {
		runID := "run-" + string(st)
		_ = h.runs.Create(context.Background(), runtime.RunRecord{ID: runID, SessionID: "s", CallerID: "c", StartedAt: started})
		h.finishRun(runID, "codex", "req", "trace", "s", "caller-1", "ws-1", "alive", started, st, nil, "")
	}

	// Exercise the active-runs gauge so its zero sample exists.
	h.incActiveRuns()
	h.decActiveRuns()

	var out strings.Builder
	if m, ok := h.metrics.(prometheusExporter); ok {
		if err := m.WritePrometheus(&out); err != nil {
			t.Fatalf("export metrics: %v", err)
		}
	}
	body := out.String()
	for _, want := range []string{
		`gateway_runs_total{adapter="codex",status="cancelled"} 1`,
		`gateway_runs_total{adapter="codex",status="failed"} 1`,
		`gateway_runs_total{adapter="codex",status="outcome_unknown"} 1`,
		`gateway_runs_total{adapter="codex",status="succeeded"} 1`,
		`gateway_runs_total{adapter="codex",status="timed_out"} 1`,
		`gateway_run_aborts_total{adapter="codex"} 1`,
		`gateway_run_timeouts_total{adapter="codex"} 1`,
		`gateway_unknown_outcomes_total{adapter="codex"} 1`,
		`gateway_active_runs 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics export missing %q", want)
		}
	}
}

// metricsExporter is intentionally not redefined here: the handler package
// already exposes prometheusExporter (used by the /metrics endpoint) and the
// in-package tests reuse it.

// TestRunFinishedLogCorrelationFields verifies the O-F11 structured run log
// carries the full correlation chain: caller, gateway session, workspace,
// worker state, and terminal status.
func TestRunFinishedLogCorrelationFields(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	ts, _, backend := newTestServer(t, func(o *Options) { o.Logger = logger })
	backend.withScript(func(h *fakeHandle) {
		h.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()),
		map[string]string{"X-Gateway-Session-Id": "sess-log-1", "X-Workspace-Id": "ws-log"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, readBody(t, resp))
	}

	var line map[string]any
	found := false
	for _, l := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(l, "run finished") {
			continue
		}
		if err := json.Unmarshal([]byte(l), &line); err != nil {
			t.Fatalf("decode run log line %q: %v", l, err)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("no 'run finished' log line captured")
	}
	for _, key := range []string{"run_id", "request_id", "caller", "session", "workspace", "adapter", "worker", "status", "duration_ms"} {
		if _, ok := line[key]; !ok {
			t.Errorf("run finished log missing %q (line: %v)", key, line)
		}
	}
	if line["session"] != "sess-log-1" || line["workspace"] != "ws-log" || line["caller"] != testOwner {
		t.Errorf("correlation fields wrong: session=%v workspace=%v caller=%v", line["session"], line["workspace"], line["caller"])
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// httptestServer mounts a pre-built handler in an httptest server.
func httptestServer(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// ---------------------------------------------------------------------------
// Final-review regressions: per-agent capacity 429, unsettled abort outcome,
// run identity / lifecycle transitions.
// ---------------------------------------------------------------------------

// sendErrHandle fails every Send with the configured error (a live handle:
// not closed, so the API treats the failure per its typed error).
type sendErrHandle struct {
	*fakeHandle
	sendErr error
}

func (h *sendErrHandle) Send(ctx context.Context, input runtime.Input) error {
	_ = h.fakeHandle.Send(ctx, input)
	return h.sendErr
}

type sendErrBackend struct{ *fakeBackend }

func (b *sendErrBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.ExecutionHandle, error) {
	h, err := b.fakeBackend.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return &sendErrHandle{fakeHandle: h.(*fakeHandle), sendErr: fmt.Errorf("agent full: %w", runtime.ErrCapacityExceeded)}, nil
}

// TestChatCompletions_AgentCapacityRejected429 verifies a full per-agent
// concurrency slot surfaces as a fast 429 (O-F10), not a 500 or an unbounded
// wait — the slot acquisition moved from session lifetime to the active turn.
func TestChatCompletions_AgentCapacityRejected429(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	inner := newFakeBackend()
	h := NewHandler(Options{
		Registry:     reg,
		Store:        runtime.NewMemorySessionStore(),
		Backend:      &sendErrBackend{inner},
		CallerTokens: map[string]string{testToken: testOwner},
		TurnTimeout:  5 * time.Second,
		UsageGrace:   15 * time.Millisecond,
	})
	t.Cleanup(h.Close)
	ts := httptestServer(t, h)

	resp := doAuthJSON(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
		chatReq("codex", false, defaultMessages()))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", resp.StatusCode, readBody(t, resp))
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	ae := decodeError(t, resp)
	if ae.Type != errorTypeRateLimit || ae.Code != "agent_max_concurrency" {
		t.Errorf("error = %+v, want rate_limit agent_max_concurrency", ae)
	}
	rec, err := h.runs.Get(context.Background(), resp.Header.Get("X-Gateway-Run-Id"))
	if err != nil {
		t.Fatalf("run record: %v", err)
	}
	if rec.Status != runtime.RunFailed || rec.ErrorCode != "agent_capacity" {
		t.Errorf("run = %s/%s, want failed/agent_capacity", rec.Status, rec.ErrorCode)
	}
}

// stubbornAbortHandle accepts the turn but its Abort RPC always fails. It
// still emits a terminal error event so the drain completes quickly — the
// interrupt itself remains unconfirmed because the abort RPC failed.
type stubbornAbortHandle struct{ *fakeHandle }

func (h *stubbornAbortHandle) Abort(ctx context.Context) error {
	h.emit(runtime.Event{Type: runtime.EventError, Error: "interrupt lost"})
	return errors.New("abort rpc failed")
}

type stubbornBackend struct{ *fakeBackend }

func (b *stubbornBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.ExecutionHandle, error) {
	h, err := b.fakeBackend.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return &stubbornAbortHandle{fakeHandle: h.(*fakeHandle)}, nil
}

// TestUnsettledAbortRecordedAsOutcomeUnknown is a regression test for the
// final review (P1): a cancel whose abort RPC fails (no confirmed terminal
// event) must be recorded as RunOutcomeUnknown, never as a confirmed
// RunCancelled that would inflate the abort metrics.
func TestUnsettledAbortRecordedAsOutcomeUnknown(t *testing.T) {
	reg := runtime.NewRegistry()
	mustRegister(t, reg, "codex", codexDesc, nil)
	inner := newFakeBackend()
	h := NewHandler(Options{
		Registry:     reg,
		Store:        runtime.NewMemorySessionStore(),
		Backend:      &stubbornBackend{inner},
		CallerTokens: map[string]string{testToken: testOwner},
		TurnTimeout:  60 * time.Second,
		UsageGrace:   15 * time.Millisecond,
	})
	t.Cleanup(h.Close)
	ts := httptestServer(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions", nil)
	// Send a streaming request; the turn never produces events.
	body := fmt.Sprintf(`{"model":"codex","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"workspace_id":%q}}`, testWorkspace)
	req.Body = io.NopCloser(strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")

	runID := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		runID <- resp.Header.Get("X-Gateway-Run-Id")
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		errCh <- nil
	}()

	// Wait for the turn to be in flight (run record exists), then disconnect.
	deadline := time.Now().Add(2 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if h.activeRuns.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.activeRuns.Load() == 0 {
		t.Fatal("turn never started")
	}
	select {
	case id = <-runID:
	case <-time.After(2 * time.Second):
		t.Fatal("no response headers")
	case err := <-errCh:
		t.Fatalf("request failed early: %v", err)
	}
	cancel() // client disconnect → cancel path with a failing abort
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("stream request error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not terminate after client disconnect")
	}

	pollDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(pollDeadline) {
		rec, err := h.runs.Get(context.Background(), id)
		if err == nil && rec.Status.Terminal() {
			if rec.Status != runtime.RunOutcomeUnknown {
				t.Fatalf("run status = %s, want outcome_unknown for an unsettled abort", rec.Status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run never reached a terminal status")
}

// TestRunRecordTransitionsToRunning verifies a delivered turn moves the run
// record out of RunStarting into RunRunning while it executes (O-F11), and
// the record keeps the PUBLIC model id from the request.
func TestRunRecordTransitionsToRunning(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTurn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseTurn()

	ts, h, backend := newTestServer(t)
	backend.withScript(func(fh *fakeHandle) {
		<-release
		fh.emit(runtime.Event{Type: runtime.EventFinish, FinishReason: "end_turn"})
	})

	reqDone := make(chan string, 1)
	go func() {
		resp := doAuthJSONH(t, "POST", ts.URL+"/v1/chat/completions", testToken, testOwner,
			chatReq("codex", false, defaultMessages()),
			map[string]string{"X-Gateway-Session-Id": "sess-run-live", "X-Workspace-Id": "ws-1"})
		reqDone <- resp.Header.Get("X-Gateway-Run-Id")
	}()

	// While the turn is blocked mid-execution, its record must be RunRunning
	// (not stuck at RunStarting).
	sawRunning := false
	poll := time.Now().Add(5 * time.Second)
	for time.Now().Before(poll) && !sawRunning {
		runs, err := h.runs.ListBySession(context.Background(), "sess-run-live")
		if err == nil {
			for _, rec := range runs {
				if rec.Status == runtime.RunRunning {
					sawRunning = true
					if rec.ModelID != "codex" {
						t.Errorf("run ModelID = %q, want the public model codex", rec.ModelID)
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	releaseTurn()
	runID := <-reqDone
	if !sawRunning {
		t.Fatal("run record never transitioned to RunRunning while the turn was executing")
	}
	rec, err := h.runs.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("final run record: %v", err)
	}
	if rec.Status != runtime.RunSucceeded {
		t.Fatalf("final status = %s, want succeeded", rec.Status)
	}
}
