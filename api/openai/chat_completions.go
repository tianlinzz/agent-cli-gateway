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
	in.Metadata = sanitizeMetadata(req.Metadata)
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
// strips path-bearing keys so a client can never inject a workspace path
// through metadata. Path control belongs solely to the server-side workspace
// resolver.
func sanitizeMetadata(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "workdir", "cwd", "working_directory", "working_dir":
			continue
		}
		switch t := v.(type) {
		case string:
			out[k] = t
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out
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
	ownerID := h.ownerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, authError("missing owner identity header "+h.ownerHeader))
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, invalidRequest("invalid JSON body: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, invalidRequest("model is required"))
		return
	}
	if !h.catalog.has(req.Model) {
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
	// one and returns it in the X-Gateway-Session-Id response header so the
	// client can resume later. An explicit id must resolve to a session owned
	// by this tenant; BOTH an unknown id and a wrong-owner id yield the same
	// generic 404, so a cross-tenant probe never learns whether a session id
	// exists. This is the deliberate Task-2 collapse.
	created := false
	var rec runtime.SessionRecord
	if sessionID == "" {
		sessionID = newSessionID()
		created = true
	}
	if created {
		if workspaceID == "" {
			writeError(w, http.StatusBadRequest, invalidRequest("workspace_id is required for a new session"))
			return
		}
		rec = runtime.SessionRecord{
			ID:          sessionID,
			ModelID:     req.Model,
			OwnerID:     ownerID,
			WorkspaceID: workspaceID,
		}
		if err := h.store.Create(r.Context(), rec); err != nil {
			writeError(w, http.StatusInternalServerError, serverError("failed to create session: "+err.Error()))
			return
		}
	} else {
		var err error
		rec, err = h.store.Get(r.Context(), sessionID, ownerID)
		if err != nil {
			if errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrSessionForbidden) {
				writeError(w, http.StatusNotFound, sessionNotFound())
				return
			}
			writeError(w, http.StatusInternalServerError, serverError(err.Error()))
			return
		}
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
	// Echo the gateway session id so the client can resume the session on a
	// later request.
	w.Header().Set(h.sessionHeader, sessionID)

	// Single active turn per session (the store's BeginTurn is the arbiter).
	if err := h.store.BeginTurn(r.Context(), sessionID, ownerID); err != nil {
		if created {
			h.deleteSession(r.Context(), sessionID, ownerID)
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

	ts := h.registerTurn(sessionID)
	turnCtx, turnCancel := context.WithCancel(r.Context())
	ts.setCancel(turnCancel)

	failed := false
	defer func() {
		h.clearTurn(sessionID, ownerID, ts)
		if created && failed {
			h.deleteSession(context.Background(), sessionID, ownerID)
		}
	}()

	// Execution goes through the runtime.ExecutionBackend — the API never
	// starts a CLI itself. A live session reuses its existing execution.
	handle := h.getHandle(sessionID)
	if handle == nil {
		startReq := runtime.StartRequest{
			ModelID:     req.Model,
			SessionID:   sessionID,
			OwnerID:     ownerID,
			WorkspaceID: workspaceID,
			Metadata:    input.Metadata,
		}
		handle, err = h.backend.Start(r.Context(), startReq)
		if err != nil {
			failed = true
			writeError(w, http.StatusInternalServerError, serverError("failed to start agent execution: "+err.Error()))
			return
		}
		handle = h.registerHandle(sessionID, handle)
	}

	if err := handle.Send(turnCtx, input); err != nil {
		failed = true
		if r.Context().Err() != nil {
			return // the client is gone; nothing to write
		}
		writeError(w, http.StatusInternalServerError, serverError("failed to send turn: "+err.Error()))
		return
	}

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		h.streamTurn(w, r, turnCtx, req.Model, sessionID, handle, includeUsage)
		return
	}
	res := h.aggregateTurn(turnCtx, sessionID, handle)
	h.writeCompletion(w, r, req.Model, res)
}

func (h *Handler) deleteSession(ctx context.Context, id, ownerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.store.Delete(ctx, id, ownerID); err != nil {
		slog.Debug("openai: delete session", "session", id, "error", err)
	}
}

// handleAbort serves POST /v1/sessions/{id}/abort: it cancels the in-flight
// turn (propagating to the worker -> CLI process group) and waits for the turn
// to settle.
func (h *Handler) handleAbort(w http.ResponseWriter, r *http.Request) {
	ownerID := h.ownerID(r)
	if ownerID == "" {
		writeError(w, http.StatusUnauthorized, authError("missing owner identity header "+h.ownerHeader))
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("id"))
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, invalidRequest("missing session id"))
		return
	}
	if _, err := h.store.Get(r.Context(), sessionID, ownerID); err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrSessionForbidden) {
			writeError(w, http.StatusNotFound, sessionNotFound())
			return
		}
		writeError(w, http.StatusInternalServerError, serverError(err.Error()))
		return
	}

	h.mu.Lock()
	handle := h.handles[sessionID]
	ts := h.turns[sessionID]
	h.mu.Unlock()

	if handle != nil {
		h.abortHandle(r.Context(), handle)
	}
	if ts != nil {
		// Cancel the turn context so the streaming/aggregation loop unblocks,
		// then wait for its cleanup (EndTurn) to complete.
		ts.cancelTurn()
		select {
		case <-ts.done:
		case <-time.After(5 * time.Second):
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "aborted"})
}

// ---------------------------------------------------------------------------
// Non-streaming response aggregation.
// ---------------------------------------------------------------------------

type turnResult struct {
	content      string
	toolCalls    []openAIToolCall
	usage        *usageInfo
	finishReason string
	failed       bool
	errMsg       string
	timedOut     bool
	aborted      bool
}

// aggregateTurn consumes canonical events until the turn ends (EventFinish,
// EventError, channel close, timeout, or cancellation) and aggregates them
// into an OpenAI-shaped completion result. A closed events channel means the
// execution terminated; the dead handle is dropped so a later resume starts a
// fresh execution instead of calling Send on a dead session.
func (h *Handler) aggregateTurn(ctx context.Context, sessionID string, handle runtime.ExecutionHandle) turnResult {
	var res turnResult
	finishSeen := false
	var grace <-chan time.Time
	var timeout <-chan time.Time
	if h.turnTimeout > 0 {
		timeout = time.After(h.turnTimeout)
	}
	for {
		select {
		case <-ctx.Done():
			// Client disconnect or explicit abort: the turn must be killed at
			// the worker so no orphaned CLI keeps running.
			res.aborted = true
			h.abortHandle(context.Background(), handle)
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
			case runtime.EventToolUse:
				if ev.Tool != nil {
					res.toolCalls = append(res.toolCalls, toOpenAIToolCall(*ev.Tool, len(res.toolCalls)))
				}
			case runtime.EventToolResult:
				// Tool results are not surfaced in OpenAI chat responses.
			case runtime.EventUsage:
				if ev.Usage != nil {
					res.usage = usageFromRuntime(ev.Usage)
				}
			case runtime.EventError:
				res.failed = true
				res.errMsg = ev.Error
				return res
			case runtime.EventFinish:
				res.finishReason = ev.FinishReason
				finishSeen = true
				// Usage may arrive right after the finish marker; drain briefly.
				grace = time.After(h.usageGrace)
			}
		case <-grace:
			return res
		case <-timeout:
			res.timedOut = true
			res.finishReason = "length"
			// The agent may still be running; kill it after reporting.
			h.abortHandle(context.Background(), handle)
			return res
		}
	}
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
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
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

	finish := mapFinishReason(res.finishReason, len(res.toolCalls))
	var content *string
	if res.content == "" && len(res.toolCalls) > 0 {
		content = nil // OpenAI sends null content when the message is tool-only
	} else {
		c := res.content
		content = &c
	}
	resp := completionResponse{
		ID:      "chatcmpl-" + h.newID(),
		Object:  "chat.completion",
		Created: h.now().Unix(),
		Model:   model,
		Choices: []completionChoice{{
			Index: 0,
			Message: chatCompletionMessage{
				Role:      "assistant",
				Content:   content,
				ToolCalls: res.toolCalls,
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

// toOpenAIToolCall converts a canonical tool call into the OpenAI response
// shape, generating a stable id when the agent supplied none.
func toOpenAIToolCall(tc runtime.ToolCall, index int) openAIToolCall {
	id := tc.ID
	if id == "" {
		id = fmt.Sprintf("call_%d", index)
	}
	args := ""
	if tc.Arguments != nil {
		if b, err := json.Marshal(tc.Arguments); err == nil {
			args = string(b)
		}
	}
	return openAIToolCall{
		ID:   id,
		Type: "function",
		Function: openAIFunctionCall{
			Name:      tc.Name,
			Arguments: args,
		},
	}
}

// mapFinishReason converts canonical finish reasons into OpenAI values. OpenAI
// clients use finish_reason to decide whether to continue the tool loop, so a
// concrete canonical reason wins whenever it is non-empty: end_turn/stop map to
// "stop" and known tool-related reasons map to "tool_calls". Only when no
// canonical reason was provided do we synthesize "tool_calls" from the fact that
// the turn ended on a tool use. Unknown reasons fall back to the safe "stop"
// default — the valid OpenAI values are exactly stop, length, tool_calls and
// content_filter, and we never pass arbitrary agent strings through.
func mapFinishReason(reason string, toolCalls int) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "end_turn", "stop":
		return "stop"
	case "max_tokens", "length":
		return "length"
	case "tool_calls", "tool_call", "function_calls", "function_call", "tool_use", "requires_action":
		return "tool_calls"
	case "":
		// No explicit reason: synthesize only when a tool call was the last
		// action of the turn.
		if toolCalls > 0 {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}
