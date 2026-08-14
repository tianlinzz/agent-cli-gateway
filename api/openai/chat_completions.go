package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// maxBodyBytes bounds the chat request body.
const maxBodyBytes = 1 << 20 // 1 MiB

// Metadata pass-through bounds (HTTP input hardening, O-F07). They cap the
// number of keys and the byte length of each key/value so an oversized
// metadata map cannot balloon memory or the native prompt.
const (
	maxMetadataKeys       = 64
	maxMetadataKeyBytes   = 64
	maxMetadataValueBytes = 1024
)

// ---------------------------------------------------------------------------
// Normalizer: OpenAI request -> canonical runtime Input/StartRequest.
// ---------------------------------------------------------------------------

// normalizeInput converts an OpenAI chat request into a canonical runtime
// Input. It is the only place the OpenAI message/tool shape is translated.
func normalizeInput(req ChatCompletionRequest) (runtime.Input, error) {
	var in runtime.Input
	for _, m := range req.Messages {
		rm, err := normalizeMessage(m)
		if err != nil {
			return runtime.Input{}, err
		}
		in.Messages = append(in.Messages, rm)
	}
	for _, t := range req.Tools {
		if t.Type != "" && !strings.EqualFold(t.Type, "function") {
			// Phase 1 supports function tools only; other tool kinds are
			// ignored rather than rejected so clients can send a superset.
			continue
		}
		in.Tools = append(in.Tools, runtime.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	meta, err := sanitizeMetadata(req.Metadata)
	if err != nil {
		return runtime.Input{}, err
	}
	in.Metadata = meta
	return in, nil
}

func normalizeMessage(m ChatMessage) (runtime.Message, error) {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	switch role {
	case "system", "user", "assistant", "tool":
	default:
		return runtime.Message{}, fmt.Errorf("unsupported message role %q", m.Role)
	}
	content, err := normalizeContent(m.Content)
	if err != nil {
		return runtime.Message{}, err
	}
	rm := runtime.Message{
		Role:       role,
		Content:    content,
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
	}
	for _, tc := range m.ToolCalls {
		rtc, err := normalizeToolCall(tc)
		if err != nil {
			return runtime.Message{}, err
		}
		rm.ToolCalls = append(rm.ToolCalls, rtc)
	}
	return rm, nil
}

// normalizeContent accepts a plain string or OpenAI's structured content
// parts; phase 1 concatenates text parts and ignores non-text parts (images).
func normalizeContent(content any) (string, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		var parts []string
		for _, p := range v {
			pp, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := pp["type"].(string); typ == "text" {
				if t, ok := pp["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("unsupported content type %T", content)
	}
}

func normalizeToolCall(tc ChatToolCall) (runtime.ToolCall, error) {
	args := map[string]any{}
	raw := strings.TrimSpace(tc.Function.Arguments)
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return runtime.ToolCall{}, fmt.Errorf("tool call %q arguments: invalid JSON: %v", tc.ID, err)
		}
	}
	return runtime.ToolCall{
		ID:        tc.ID,
		Name:      tc.Function.Name,
		Arguments: args,
	}, nil
}

// sanitizeMetadata converts arbitrary metadata into a map[string]string and
// strips keys the server must own outright:
//   - path-bearing keys (workdir/cwd/...) so a client can never inject a
//     workspace path; path control belongs solely to the workspace resolver;
//   - native resume ids (native_session_id and the per-agent variants) so a
//     client can never select or override the native session/thread the
//     gateway resumes into. The gateway injects the sole canonical
//     native_session_id from its own SessionRecord after this step.
func sanitizeMetadata(m map[string]any) (map[string]string, error) {
	if len(m) > maxMetadataKeys {
		return nil, fmt.Errorf("metadata has %d keys; max %d", len(m), maxMetadataKeys)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "workdir", "cwd", "working_directory", "working_dir":
			continue
		case "native_session_id", "codex_thread_id", "claude_session_id", "kimi_session_id":
			continue
		case "run_id", "request_id", "trace_id":
			// Server-owned correlation identity: the gateway injects the sole
			// canonical values after sanitization, so a client can never
			// forge them (O-F11).
			continue
		}
		if len(k) > maxMetadataKeyBytes {
			return nil, fmt.Errorf("metadata key %q exceeds %d bytes", k, maxMetadataKeyBytes)
		}
		var s string
		switch t := v.(type) {
		case string:
			s = t
		default:
			s = fmt.Sprint(t)
		}
		if len(s) > maxMetadataValueBytes {
			return nil, fmt.Errorf("metadata value for key %q exceeds %d bytes", k, maxMetadataValueBytes)
		}
		out[k] = s
	}
	return out, nil
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Session resolution and the chat completions handler.
// ---------------------------------------------------------------------------

// handleChatCompletions serves POST /v1/chat/completions for both streaming
// and non-streaming requests.
func (h *Handler) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, authError("missing authenticated caller"))
		return
	}
	callerID := caller.ID

	// Correlation identifiers for structured logging and trace propagation.
	// request_id is always server-generated; trace_id comes from the W3C
	// traceparent header when the caller provides one.
	reqID := "req-" + h.newID()
	traceID := parseTraceparent(r.Header.Get("traceparent"))

	var req ChatCompletionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, invalidRequest("request body exceeds 1 MiB"))
			return
		}
		writeError(w, http.StatusBadRequest, invalidRequest("invalid JSON body: "+err.Error()))
		return
	}
	// Reject a body holding more than one top-level JSON value, or trailing
	// bytes after a valid object. Without this, a second JSON value (or garbage)
	// after a valid object is silently ignored by the single Decode above.
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, invalidRequest("request body must contain a single JSON object"))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, invalidRequest("model is required"))
		return
	}
	route, ok := h.catalog.route(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, modelNotFound(req.Model))
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, invalidRequest("messages is required"))
		return
	}
	input, err := normalizeInput(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, invalidRequest(err.Error()))
		return
	}

	// The gateway session id and workspace id come from explicit headers or
	// request metadata — never from the OpenAI model field.
	sessionID := strings.TrimSpace(firstNonEmpty(r.Header.Get(h.sessionHeader), metaString(req.Metadata, "session_id")))
	workspaceID := strings.TrimSpace(firstNonEmpty(r.Header.Get(h.workspaceHeader), metaString(req.Metadata, "workspace_id")))

	// Resolve the session. Absent gateway session id -> the gateway generates
	// one. A caller may also provide a stable business-conversation id on its
	// first request; an unknown id is created for that caller, while an existing
	// id owned by another caller remains the same generic 404 as before.
	created := false
	var rec runtime.SessionRecord
	if sessionID == "" {
		sessionID = newSessionID()
	}
	rec, err = h.store.Get(r.Context(), sessionID, callerID)
	if errors.Is(err, runtime.ErrSessionNotFound) {
		if workspaceID == "" {
			writeError(w, http.StatusBadRequest, invalidRequest("workspace_id is required for a new session"))
			return
		}
		rec = runtime.SessionRecord{
			ID:          sessionID,
			ModelID:     req.Model,
			CallerID:    callerID,
			WorkspaceID: workspaceID,
		}
		if createErr := h.store.Create(r.Context(), rec); createErr == nil {
			created = true
			h.clearServerToolIDs(sessionID)
			err = nil
		} else if errors.Is(createErr, runtime.ErrSessionExists) {
			// Concurrent first requests may race to create the caller-provided
			// id. Re-read it and apply the normal ownership/model/workspace
			// checks; never overwrite the winner.
			rec, err = h.store.Get(r.Context(), sessionID, callerID)
		} else if errors.Is(createErr, runtime.ErrSessionCapacity) {
			// Session record cap (global or per-caller) exhausted: the overload
			// contract (O-F10) requires a fast, deterministic 429 — the record
			// was never created, so there is nothing to clean up.
			writeRateLimited(w, rateLimited(string(ScopeSessions), "cap_reached"))
			return
		} else {
			writeError(w, http.StatusInternalServerError, serverError("failed to create session: "+createErr.Error()))
			return
		}
	}
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrSessionForbidden) {
			writeError(w, http.StatusNotFound, sessionNotFound())
			return
		}
		writeError(w, http.StatusInternalServerError, serverError(err.Error()))
		return
	}
	if !created {
		if rec.ModelID != req.Model {
			writeError(w, http.StatusBadRequest, invalidRequest("model does not match session "+sessionID))
			return
		}
		if workspaceID != "" && rec.WorkspaceID != "" && workspaceID != rec.WorkspaceID {
			writeError(w, http.StatusBadRequest, invalidRequest("workspace_id does not match session "+sessionID))
			return
		}
		workspaceID = rec.WorkspaceID
	}
	if h.hasServerToolReplay(sessionID, req.Messages) {
		writeError(w, http.StatusBadRequest, invalidRequest("server-executed tool results must not be submitted by the client"))
		return
	}
	// Echo the gateway session id so the client can resume the session on a
	// later request.
	w.Header().Set(h.sessionHeader, sessionID)
	// Assign a server-owned run id for this turn. The header MUST be set before
	// the stream/non-stream branch: streamTurn calls WriteHeader before the
	// first SSE frame.
	runID := h.newRunID()
	w.Header().Set(h.runHeader, runID)

	// Admission control: check active-run limits before acquiring the turn
	// (O-F10). The release defer is installed IMMEDIATELY after a successful
	// acquire — before BeginTurn and every other fallible step below — so no
	// failure path can leak the slot (leaked slots permanently inflate the
	// counters and eventually 429 every later request).
	if h.admit != nil {
		release, err := h.admit.Acquire(r.Context(), callerID, workspaceID)
		if err != nil {
			var ae *ErrAdmissionRejected
			if errors.As(err, &ae) {
				if created {
					h.deleteSession(r.Context(), sessionID, callerID)
				}
				writeRateLimited(w, rateLimited(string(ae.Scope), string(ae.Reason)))
				return
			}
			writeError(w, http.StatusInternalServerError, serverError(err.Error()))
			return
		}
		defer release()
	}

	// Single active turn per session (the store's BeginTurn is the arbiter).
	err = h.store.BeginTurn(r.Context(), sessionID, callerID)
	if errors.Is(err, runtime.ErrSessionBusy) && h.awaitSettlingTurn(r.Context(), sessionID) {
		err = h.store.BeginTurn(r.Context(), sessionID, callerID)
	}
	if err != nil {
		if created {
			h.deleteSession(r.Context(), sessionID, callerID)
		}
		switch {
		case errors.Is(err, runtime.ErrSessionBusy):
			writeError(w, http.StatusConflict, sessionBusy(""))
		case errors.Is(err, runtime.ErrSessionState):
			writeError(w, http.StatusConflict, sessionBusy("session is closing"))
		case errors.Is(err, runtime.ErrSessionNotFound), errors.Is(err, runtime.ErrSessionForbidden):
			writeError(w, http.StatusNotFound, sessionNotFound())
		default:
			writeError(w, http.StatusInternalServerError, serverError(err.Error()))
		}
		return
	}
	// Every turn refreshes the record's TTL so an active session is never
	// reaped by the prune loop, while an idle session expires and is evicted
	// (with its handle) to bound long-run memory growth.
	if h.recordTTL > 0 {
		if err := h.store.Touch(r.Context(), sessionID, callerID, h.recordTTL); err != nil {
			slog.Debug("openai: refresh session ttl", "session", sessionID, "error", err)
		}
	}

	ts := h.registerTurn(sessionID)
	turnCtx, turnCancel := context.WithCancel(r.Context())
	ts.setCancel(turnCancel)

	// The in-flight gauge and the turn cleanup defer are installed BEFORE the
	// run record is created so every later path — including a record-creation
	// failure — releases exactly what it acquired.
	h.incActiveRuns()
	failed := false
	defer func() {
		h.clearTurn(sessionID, callerID, ts)
		h.decActiveRuns()
		if created && failed {
			h.deleteSession(context.Background(), sessionID, callerID)
		}
	}()

	// Record the run for observability and status correlation. ModelID keeps
	// the PUBLIC model the caller asked for (it may differ from the adapter
	// id when models are mapped); metrics stay keyed by the bounded adapter
	// id. A run that cannot be recorded must not execute: the response would
	// advertise a Run header with no correlatable record (O-F11).
	runStartedAt := h.now()
	if err := h.runs.Create(r.Context(), runtime.RunRecord{
		ID:          runID,
		SessionID:   sessionID,
		CallerID:    callerID,
		WorkspaceID: workspaceID,
		ModelID:     req.Model,
		Status:      runtime.RunStarting,
		StartedAt:   runStartedAt,
	}); err != nil {
		failed = true
		h.logger.Error("openai: run store create", "run_id", runID, "session", sessionID, "error", err)
		writeError(w, http.StatusInternalServerError, serverError("failed to record run"))
		return
	}

	// Server-owned run identity crosses the API boundary on EVERY turn —
	// both the StartRequest and each Input — so the worker/agent side can
	// correlate logs and events with the exact run the response headers
	// advertise (O-F11). sanitizeMetadata strips client-supplied
	// run_id/request_id/trace_id, so these values cannot be forged. Injected
	// BEFORE startReq is built so both share the same metadata map.
	if input.Metadata == nil {
		input.Metadata = make(map[string]string, 3)
	}
	input.Metadata["run_id"] = runID
	input.Metadata["request_id"] = reqID
	if traceID != "" {
		input.Metadata["trace_id"] = traceID
	}

	// Execution goes through the runtime.ExecutionBackend — the API never
	// starts a CLI itself. A live session reuses its existing execution.
	startReq := runtime.StartRequest{
		ModelID:       route.AdapterID,
		ProviderModel: route.ProviderModel,
		SessionID:     sessionID,
		CallerID:      callerID,
		WorkspaceID:   workspaceID,
		Metadata:      input.Metadata,
	}
	if rec.NativeSessionID != "" {
		if startReq.Metadata == nil {
			startReq.Metadata = make(map[string]string)
		}
		startReq.Metadata["native_session_id"] = rec.NativeSessionID
	}
	handle := h.getHandle(sessionID)
	if handle == nil {
		var err error
		handle, err = h.backend.Start(r.Context(), startReq)
		if err != nil {
			failed = true
			// A backend at a configured capacity (e.g. max_workers) must
			// surface as 429 + Retry-After (O-F10), never a 500 after the
			// session/turn state was already built.
			if errors.Is(err, runtime.ErrCapacityExceeded) {
				h.finishRun(runID, route.AdapterID, reqID, traceID, sessionID, callerID, workspaceID, workerStateOf(nil), runStartedAt, runtime.RunFailed, nil, "worker_capacity")
				writeRateLimited(w, rateLimited(string(ScopeWorkers), "max_workers"))
				return
			}
			h.finishRun(runID, route.AdapterID, reqID, traceID, sessionID, callerID, workspaceID, workerStateOf(nil), runStartedAt, runtime.RunFailed, nil, "start_failed")
			writeError(w, http.StatusInternalServerError, serverError("failed to start agent execution: "+err.Error()))
			return
		}
		handle = h.registerHandle(sessionID, handle)
	}

	// Deliver the turn. A Send failure on a handle whose worker/CLI died while
	// the session was IDLE between turns is recovered here: the stale handle is
	// dropped and a FRESH execution is started for the session. Without this, a
	// dead handle stays in the map and every later request to that session fails
	// with "failed to send turn" until the gateway restarts.
	handle, err = h.deliverTurn(turnCtx, sessionID, handle, startReq, input)
	if err != nil {
		failed = true
		// A per-agent concurrency slot (or worker capacity on restart) is
		// full: the overload contract (O-F10) requires a fast 429, not a 500
		// or an unbounded wait.
		if errors.Is(err, runtime.ErrCapacityExceeded) {
			h.finishRun(runID, route.AdapterID, reqID, traceID, sessionID, callerID, workspaceID, workerStateOf(handle), runStartedAt, runtime.RunFailed, nil, "agent_capacity")
			writeRateLimited(w, rateLimited(string(ScopeAgent), "max_concurrency"))
			return
		}
		status := runtime.RunFailed
		if r.Context().Err() != nil {
			// The client is gone; the interrupt is unconfirmed, so the
			// completion outcome is unknown — not a confirmed cancellation.
			status = runtime.RunOutcomeUnknown
		}
		h.finishRun(runID, route.AdapterID, reqID, traceID, sessionID, callerID, workspaceID, workerStateOf(handle), runStartedAt, status, nil, "send_failed")
		if r.Context().Err() != nil {
			return // the client is gone; nothing to write
		}
		writeError(w, http.StatusInternalServerError, serverError("failed to send turn: "+err.Error()))
		return
	}
	// The turn is actually executing now (was delivered to a live worker):
	// record the transition out of RunStarting so an in-flight run is
	// distinguishable from one that never started (O-F11).
	if _, err := h.runs.Update(context.Background(), runID, func(r *runtime.RunRecord) {
		if r.Status == runtime.RunStarting {
			r.Status = runtime.RunRunning
		}
	}); err != nil {
		h.logger.Warn("openai: run status transition to running", "run_id", runID, "error", err)
	}

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		status := h.streamTurn(w, r, turnCtx, req.Model, sessionID, callerID, handle, includeUsage)
		h.finishRun(runID, route.AdapterID, reqID, traceID, sessionID, callerID, workspaceID, workerStateOf(handle), runStartedAt, status, nil, "")
		return
	}
	res := h.aggregateTurn(turnCtx, sessionID, callerID, handle)
	h.writeCompletion(w, r, req.Model, res)
	h.finishRun(runID, route.AdapterID, reqID, traceID, sessionID, callerID, workspaceID, workerStateOf(handle), runStartedAt, runStatusFromTurnResult(res), res.usage, "")
}

// closedHandle is the optional capability an ExecutionHandle implements to
// report that its underlying session/worker has terminated. workerSession
// implements it; a handle whose session is closed must be dropped so a later
// request to that session starts a fresh execution instead of wedging it.
type closedHandle interface {
	// Closed reports whether the underlying session has terminated. Send on a
	// closed handle fails terminally.
	Closed() bool
}

// terminatingHandle exposes the execution's terminal signal. A worker can
// lose its transport a few milliseconds before its supervisor publishes the
// closed state; waiting on this signal closes that recovery race without
// treating ordinary Send errors as terminal.
type terminatingHandle interface {
	Done() <-chan struct{}
}

type terminatingState interface {
	Terminating() bool
}

// deliverTurn sends a turn to the session's execution handle, recovering from
// a DEAD handle — a worker/CLI that exited while the session was idle between
// turns — by dropping the stale handle and starting a FRESH execution for the
// session. This matches the "dead handle → fresh execution on resume" contract
// that the event consumers (aggregateTurn/streamTurn) already implement for
// mid-turn deaths: a closed events channel means the execution terminated and
// the handle must not be reused.
func (h *Handler) deliverTurn(ctx context.Context, sessionID string, handle runtime.ExecutionHandle, startReq runtime.StartRequest, input runtime.Input) (runtime.ExecutionHandle, error) {
	if err := handle.Send(ctx, input); err == nil {
		return handle, nil
	} else if !h.awaitDeadHandle(ctx, handle) {
		// Transient turn error on a live handle: surface it, keep the handle.
		return handle, err
	}

	// The session/worker is gone. Drop the stale handle (the supervisor has
	// already reaped the dead worker and removed its session) and start a fresh
	// execution. For persistent adapters this is the recovery path after a
	// persistent_process (claude-code) a fresh process loses in-process state
	// but native transcript on disk allows resume — strictly better than the
	// permanent 500 that otherwise wedges the session until gateway restart.
	h.dropHandle(sessionID, handle)
	fresh, err := h.backend.Start(ctx, startReq)
	if err != nil {
		return nil, fmt.Errorf("restart execution for session %s: %w", sessionID, err)
	}
	fresh = h.registerHandle(sessionID, fresh)
	if err := fresh.Send(ctx, input); err != nil {
		return fresh, err
	}
	return fresh, nil
}

func (h *Handler) awaitDeadHandle(ctx context.Context, handle runtime.ExecutionHandle) bool {
	if h.deadHandle(handle) {
		return true
	}
	doneHandle, hasDone := handle.(terminatingHandle)
	stateHandle, hasState := handle.(terminatingState)
	if !hasDone || !hasState {
		return false
	}
	publication := time.NewTimer(250 * time.Millisecond)
	defer publication.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for !stateHandle.Terminating() {
		select {
		case <-doneHandle.Done():
			return h.deadHandle(handle)
		case <-ctx.Done():
			return false
		case <-publication.C:
			return false
		case <-poll.C:
		}
	}
	wait := time.NewTimer(turnHandoffTimeout)
	defer wait.Stop()
	select {
	case <-doneHandle.Done():
		return h.deadHandle(handle)
	case <-ctx.Done():
		return false
	case <-wait.C:
		return h.deadHandle(handle)
	}
}

// deadHandle reports whether a Send failure means the underlying session is
// gone (terminal) rather than a transient turn error. When the handle exposes
// no liveness signal we are conservative and treat the failure as transient.
func (h *Handler) deadHandle(handle runtime.ExecutionHandle) bool {
	if ch, ok := handle.(closedHandle); ok {
		return ch.Closed()
	}
	return false
}

func (h *Handler) deleteSession(ctx context.Context, id, callerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.store.Delete(ctx, id, callerID); err != nil {
		slog.Debug("openai: delete session", "session", id, "error", err)
		return
	}
	h.clearServerToolIDs(id)
}

// handleAbort serves POST /v1/sessions/{id}/abort: it cancels the in-flight
// turn (propagating to the worker -> CLI process group) and waits for the turn
// to settle.
func (h *Handler) handleAbort(w http.ResponseWriter, r *http.Request) {
	caller, ok := callerFromRequest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, authError("missing authenticated caller"))
		return
	}
	callerID := caller.ID
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, invalidRequest("missing session id"))
		return
	}
	if _, err := h.store.Get(r.Context(), sessionID, callerID); err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrSessionForbidden) {
			writeError(w, http.StatusNotFound, sessionNotFound())
			return
		}
		writeError(w, http.StatusInternalServerError, serverError(err.Error()))
		return
	}

	h.mu.Lock()
	ts := h.turns[sessionID]
	h.mu.Unlock()

	if ts != nil {
		ts.markSettling()
		// Cancel the turn context so the streaming/aggregation loop unblocks,
		// then wait for its cleanup (EndTurn) to complete.
		ts.cancelTurn()
		h.incQueuedRuns()
		settle := time.NewTimer(turnHandoffTimeout)
		defer settle.Stop()
		select {
		case <-ts.done:
		case <-settle.C:
		}
		h.decQueuedRuns()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

// ---------------------------------------------------------------------------
// Non-streaming response aggregation.
// ---------------------------------------------------------------------------

type turnResult struct {
	content      string
	usage        *usageInfo
	finishReason string
	failed       bool
	errMsg       string
	timedOut     bool
	aborted      bool
	finished     bool // true when EventFinish was received (distinguishes outcome_unknown)
	// abortSettled/timeoutSettled record whether the cancel/timeout was
	// CONFIRMED (abort RPC succeeded and a terminal event drained). An
	// unconfirmed interrupt maps to RunOutcomeUnknown, never
	// RunCancelled/RunTimedOut (final review P1).
	abortSettled   bool
	timeoutSettled bool
}

// aggregateTurn consumes canonical events until the turn ends (EventFinish,
// EventError, channel close, timeout, or cancellation) and aggregates them
// into an OpenAI-shaped completion result. A closed events channel means the
// execution terminated; the dead handle is dropped so a later resume starts a
// fresh execution instead of calling Send on a dead session.
func (h *Handler) aggregateTurn(ctx context.Context, sessionID, callerID string, handle runtime.ExecutionHandle) turnResult {
	var res turnResult
	finishSeen := false
	var grace <-chan time.Time
	var graceTimer *time.Timer
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	var timeout <-chan time.Time
	if h.turnTimeout > 0 {
		t := time.NewTimer(h.turnTimeout)
		defer t.Stop()
		timeout = t.C
	}
	for {
		select {
		case <-ctx.Done():
			// Client disconnect or explicit abort: the turn must be killed at
			// the worker so no orphaned CLI keeps running. Only a settled
			// interrupt (abort confirmed + terminal drained) is a confirmed
			// cancellation.
			res.aborted = true
			h.markTurnSettling(sessionID)
			res.abortSettled = h.abortAndDrain(context.Background(), sessionID, handle)
			return res
		case ev, ok := <-handle.Events():
			if !ok {
				// The execution ended without a finish marker.
				h.dropHandle(sessionID, handle)
				if res.finishReason == "" && !res.failed {
					res.finishReason = "stop"
				}
				return res
			}
			switch ev.Type {
			case runtime.EventText:
				if finishSeen {
					continue
				}
				res.content += ev.Text
			case runtime.EventNativeSession:
				if ev.NativeSessionID != "" {
					_, _ = h.store.Update(context.Background(), sessionID, callerID, func(rec *runtime.SessionRecord) { rec.NativeSessionID = ev.NativeSessionID })
				}
			case runtime.EventToolUse:
				// The autonomous Agent already executes its native tools. Exposing
				// them as OpenAI tool_calls would make an upstream Agent framework
				// execute them again and resubmit the same user turn.
			case runtime.EventToolResult:
				if !finishSeen && ev.Tool != nil {
					h.recordServerToolID(sessionID, ev.Tool.ID)
				}
			case runtime.EventUsage:
				if ev.Usage != nil {
					res.usage = usageFromRuntime(ev.Usage)
				}
			case runtime.EventError:
				res.failed = true
				res.errMsg = ev.Error
				return res
			case runtime.EventFinish:
				if ev.NativeSessionID != "" {
					_, _ = h.store.Update(context.Background(), sessionID, callerID, func(rec *runtime.SessionRecord) { rec.NativeSessionID = ev.NativeSessionID })
				}
				res.finishReason = ev.FinishReason
				finishSeen = true
				res.finished = true
				// Usage may arrive right after the finish marker; drain briefly.
				if graceTimer != nil {
					graceTimer.Stop()
				}
				graceTimer = time.NewTimer(h.usageGrace)
				grace = graceTimer.C
			}
		case <-grace:
			return res
		case <-timeout:
			res.timedOut = true
			res.finishReason = "length"
			h.markTurnSettling(sessionID)
			// Settle and drain the timed-out turn before this session is
			// reused; only a settled interrupt counts as a confirmed timeout.
			res.timeoutSettled = h.abortAndDrain(context.Background(), sessionID, handle)
			return res
		}
	}
}

// runStatusFromTurnResult maps the non-streaming turn outcome to a RunStatus.
// Cancelled/timed-out only count as confirmed when the interrupt settled;
// otherwise the completion outcome is unknown. The
// events-closed-without-finish case is RunOutcomeUnknown, not RunSucceeded.
func runStatusFromTurnResult(res turnResult) runtime.RunStatus {
	switch {
	case res.failed:
		return runtime.RunFailed
	case res.aborted:
		if res.abortSettled {
			return runtime.RunCancelled
		}
		return runtime.RunOutcomeUnknown
	case res.timedOut:
		if res.timeoutSettled {
			return runtime.RunTimedOut
		}
		return runtime.RunOutcomeUnknown
	case !res.finished:
		return runtime.RunOutcomeUnknown
	default:
		return runtime.RunSucceeded
	}
}

// workerStateOf reports the execution handle's terminal state for run
// correlation: whether the worker/execution was still alive, had terminated,
// or never existed when the run finished. It distinguishes worker
// crash/reclaim terminal sources in the run-finished log (O-F11).
func workerStateOf(handle runtime.ExecutionHandle) string {
	if handle == nil {
		return "none"
	}
	if ch, ok := handle.(closedHandle); ok {
		if ch.Closed() {
			return "terminated"
		}
		return "alive"
	}
	return "unknown"
}

// finishRun records the terminal run status, usage, and duration in the run
// store and metrics, and emits the correlated run-finished log line. It is
// safe to call on any terminal path. The log carries the full correlation
// chain required by O-F11: caller, gateway session, workspace, worker state,
// and terminal status; the metrics use only bounded-cardinality labels.
func (h *Handler) finishRun(runID, adapterID, reqID, traceID, sessionID, callerID, workspaceID, workerState string, startedAt time.Time, status runtime.RunStatus, usage *usageInfo, errorCode string) {
	now := h.now()
	duration := now.Sub(startedAt).Seconds()

	_, err := h.runs.Update(context.Background(), runID, func(r *runtime.RunRecord) {
		r.Status = status
		r.FinishedAt = now
		r.ErrorCode = errorCode
		if usage != nil {
			r.Usage = runtime.Usage{
				InputTokens:  usage.PromptTokens,
				OutputTokens: usage.CompletionTokens,
				TotalTokens:  usage.TotalTokens,
			}
		}
	})
	if err != nil {
		h.logger.Warn("openai: run store terminal update", "run_id", runID, "status", status, "error", err)
	}

	labels := map[string]string{"status": string(status), "adapter": adapterID}
	h.metrics.IncCounter("gateway_runs_total", 1, labels)
	h.metrics.ObserveHistogram("gateway_run_duration_seconds", duration, labels)
	switch status {
	case runtime.RunOutcomeUnknown:
		h.metrics.IncCounter("gateway_unknown_outcomes_total", 1, map[string]string{"adapter": adapterID})
	case runtime.RunCancelled:
		h.metrics.IncCounter("gateway_run_aborts_total", 1, map[string]string{"adapter": adapterID})
	case runtime.RunTimedOut:
		h.metrics.IncCounter("gateway_run_timeouts_total", 1, map[string]string{"adapter": adapterID})
	}
	if usage != nil {
		dirLabels := map[string]string{"adapter": adapterID}
		h.metrics.IncCounter("gateway_agent_tokens_total", int64(usage.PromptTokens), withLabel(dirLabels, "direction", "input"))
		h.metrics.IncCounter("gateway_agent_tokens_total", int64(usage.CompletionTokens), withLabel(dirLabels, "direction", "output"))
	}

	h.logger.Info("openai: run finished",
		"run_id", runID,
		"request_id", reqID,
		"trace_id", traceID,
		"caller", callerID,
		"session", sessionID,
		"workspace", workspaceID,
		"adapter", adapterID,
		"worker", workerState,
		"status", status,
		"duration_ms", duration*1000,
		"error_code", errorCode,
	)
}

// withLabel returns a copy of labels with the given key-value added.
func withLabel(labels map[string]string, key, value string) map[string]string {
	result := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		result[k] = v
	}
	result[key] = value
	return result
}

// parseTraceparent extracts the trace id from a W3C traceparent header value.
// Format: version-trace_id-parent_id-trace_flags (e.g.
// "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"). Returns the
// empty string when the header is absent or malformed.
func parseTraceparent(tp string) string {
	if tp == "" {
		return ""
	}
	parts := strings.Split(tp, "-")
	if len(parts) < 4 {
		return ""
	}
	// trace_id is a 32-char hex string; reject all-zero (invalid per spec).
	tid := parts[1]
	if len(tid) != 32 || tid == "00000000000000000000000000000000" {
		return ""
	}
	return tid
}

// completionResponse is the OpenAI non-streaming chat completion body.
type completionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *usageInfo         `json:"usage,omitempty"`
}

type completionChoice struct {
	Index        int                   `json:"index"`
	Message      chatCompletionMessage `json:"message"`
	FinishReason *string               `json:"finish_reason"`
	Logprobs     any                   `json:"logprobs"`
}

type chatCompletionMessage struct {
	Role    string  `json:"role"`
	Content *string `json:"content"`
}

// writeCompletion writes the aggregated non-streaming result. Agent errors use
// the OpenAI error envelope rather than a raw 500 body.
func (h *Handler) writeCompletion(w http.ResponseWriter, r *http.Request, model string, res turnResult) {
	if res.aborted {
		return // the client is gone or the turn was aborted; nothing to send
	}
	if res.failed {
		writeError(w, http.StatusInternalServerError, serverError(res.errMsg))
		return
	}
	if res.timedOut {
		writeError(w, http.StatusInternalServerError, serverError("agent turn timed out"))
		return
	}

	finish := mapFinishReason(res.finishReason)
	content := res.content
	resp := completionResponse{
		ID:      "chatcmpl-" + h.newID(),
		Object:  "chat.completion",
		Created: h.now().Unix(),
		Model:   model,
		Choices: []completionChoice{{
			Index: 0,
			Message: chatCompletionMessage{
				Role:    "assistant",
				Content: &content,
			},
			FinishReason: &finish,
			Logprobs:     nil,
		}},
	}
	if res.usage != nil {
		resp.Usage = res.usage
	}
	writeJSON(w, http.StatusOK, resp)
}

// mapFinishReason converts native Agent completion reasons into OpenAI model
// values. Native tools have already run inside the autonomous Agent, so even a
// tool-related native reason means this OpenAI turn is complete. Returning
// "tool_calls" would cause upstream Agent frameworks to invoke the Gateway
// again with the same user message.
func mapFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "end_turn", "stop", "tool_calls", "tool_call", "function_calls", "function_call", "tool_use", "requires_action":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "":
		return "stop"
	default:
		return "stop"
	}
}
