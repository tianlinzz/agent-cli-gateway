package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

// streamChunk is one SSE data payload of a streaming chat completion.
type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *usageInfo     `json:"usage,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

const (
	maxToolSummaryResultBytes = 2000
)

var (
	bearerSecretPattern = regexp.MustCompile(`(?i)Bearer\s+[^\s,;"}]+`)
	namedSecretPattern  = regexp.MustCompile(`(?im)(authorization|set[_-]?cookie|cookie|access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|client[_-]?secret|password|secret|token)(\s*[:=]\s*)[^\r\n,;]+`)
)

func formatToolExecutionSummary(tool runtime.ToolCall) string {
	icon := "⏺"
	if tool.IsError {
		icon = "⨯"
	}
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		name = "Unknown"
	}
	result := strings.TrimSpace(sanitizeToolSummaryResult(tool.Result))
	if result == "" {
		return icon + " " + name + "\n"
	}
	result = strings.ReplaceAll(result, "\n", "\n    ")
	result = boundUTF8(result, maxToolSummaryResultBytes)
	return icon + " " + name + "\n  ⎿ " + result + "\n"
}

func sanitizeToolSummaryResult(result string) string {
	result = strings.ToValidUTF8(result, "")
	var structured any
	if json.Unmarshal([]byte(result), &structured) == nil {
		redactStructuredSecrets(structured)
		if data, err := json.Marshal(structured); err == nil {
			result = string(data)
		}
	}
	result = bearerSecretPattern.ReplaceAllString(result, "Bearer ***")
	result = namedSecretPattern.ReplaceAllString(result, "$1$2***")
	return boundUTF8(result, maxToolSummaryResultBytes)
}

func boundUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value) + "..."
}

func redactStructuredSecrets(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if isSecretKey(key) {
				current[key] = "***"
				continue
			}
			redactStructuredSecrets(child)
		}
	case []any:
		for _, child := range current {
			redactStructuredSecrets(child)
		}
	}
}

func isSecretKey(key string) bool {
	var normalized strings.Builder
	for _, char := range strings.ToLower(key) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	switch normalized.String() {
	case "authorization", "cookie", "setcookie", "token", "accesstoken", "refreshtoken", "idtoken", "apikey", "clientsecret", "password", "secret":
		return true
	default:
		return false
	}
}

func (c streamChunk) withChoices(choices ...streamChoice) streamChunk {
	c.Choices = choices
	return c
}

// streamTurn serves POST /v1/chat/completions with stream=true. It converts
// canonical events into OpenAI SSE chunks: role intro, content deltas, a final
// finish_reason chunk, optional usage chunk, and a final
// "data: [DONE]" frame. Client disconnect and explicit abort cancel the turn
// and terminate the stream. A closed events channel (execution terminated)
// drops the dead handle so a later resume starts fresh.
//
// adapterID selects the effective per-agent deadline (O-F09b). A turn that
// exceeds its deadline is presented as an SSE error frame with the unified
// turn_timeout code — never as a normal completion chunk with
// finish_reason:"length" (a deadline is not a token-length limit).
//
// The returned status is a CONFIRMED terminal state only: a cancelled or
// timed-out stream whose interrupt did not settle (abort RPC failed or the
// terminal event never drained) returns RunOutcomeUnknown instead of
// RunCancelled/RunTimedOut (final review P1).
func (h *Handler) streamTurn(w http.ResponseWriter, r *http.Request, ctx context.Context, model, adapterID, sessionID, callerID string, handle runtime.ExecutionHandle, includeUsage bool) (runStatus runtime.RunStatus) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, serverError("streaming not supported by the response writer"))
		return runtime.RunFailed
	}
	// Guard: unless the stream completes normally, the execution must be
	// aborted so a dropped client or a mid-stream write failure never leaves
	// an orphaned agent turn running. The abort runs in this defer (after the
	// return value is set), so an unsettled interrupt downgrades the status
	// to RunOutcomeUnknown via the named return.
	deferredAbort := true
	defer func() {
		if deferredAbort {
			h.markTurnSettling(sessionID)
			if !h.abortAndDrain(context.Background(), sessionID, handle) {
				runStatus = runtime.RunOutcomeUnknown
			}
		}
	}()
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id := "chatcmpl-" + h.newID()
	created := h.now().Unix()
	base := streamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model}

	writeChunk := func(c streamChunk) error {
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Role intro chunk (OpenAI sends the assistant role in the first delta).
	if err := writeChunk(base.withChoices(streamChoice{
		Index: 0,
		Delta: streamDelta{Role: "assistant"},
	})); err != nil {
		return runtime.RunCancelled
	}

	var (
		usage      *usageInfo
		finish     = ""
		finishSeen = false
		status     = "normal" // "normal" | "error" | "timeout"
		started    = make(map[string]runtime.ToolCall)
		completed  = make(map[string]struct{})
	)
	runStatus = runtime.RunSucceeded
	var grace <-chan time.Time
	var graceTimer *time.Timer
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	var timeout <-chan time.Time
	if d := h.turnDeadline(adapterID); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client disconnect or explicit abort: settle this turn and discard
			// its terminal events before another request can reuse the session.
			// Only a settled interrupt is a confirmed cancellation.
			h.markTurnSettling(sessionID)
			settled := h.abortAndDrain(context.Background(), sessionID, handle)
			deferredAbort = false
			if settled {
				runStatus = runtime.RunCancelled
			} else {
				runStatus = runtime.RunOutcomeUnknown
			}
			if r.Context().Err() != nil {
				return runStatus // the client is gone; nothing more to write
			}
			// Explicit abort with the client still connected: terminate the
			// stream gracefully.
			finish = "stop"
			break loop
		case ev, ok := <-handle.Events():
			if !ok {
				// The execution ended without a finish marker; drop the dead
				// handle so a later resume starts a fresh execution.
				h.dropHandle(sessionID, handle)
				if finish == "" {
					finish = "stop"
				}
				runStatus = runtime.RunOutcomeUnknown
				break loop
			}
			switch ev.Type {
			case runtime.EventText:
				if finishSeen || ev.Text == "" {
					continue
				}
				if err := writeChunk(base.withChoices(streamChoice{
					Index: 0,
					Delta: streamDelta{Content: ev.Text},
				})); err != nil {
					return runtime.RunCancelled
				}
			case runtime.EventReasoning:
				if finishSeen || ev.Reasoning == nil || ev.Reasoning.Text == "" {
					continue
				}
				if err := writeChunk(base.withChoices(streamChoice{
					Index: 0,
					Delta: streamDelta{ReasoningContent: ev.Reasoning.Text},
				})); err != nil {
					return runtime.RunCancelled
				}
			case runtime.EventToolUse:
				// Native Agent tools are already executed. Do not emit OpenAI
				// client-executable tool call deltas, which would start a second
				// orchestration loop.
				if ev.Tool != nil && ev.Tool.ID != "" {
					started[ev.Tool.ID] = *ev.Tool
				}
			case runtime.EventNativeSession:
				if ev.NativeSessionID != "" {
					_, _ = h.store.Update(context.Background(), sessionID, callerID, func(rec *runtime.SessionRecord) { rec.NativeSessionID = ev.NativeSessionID })
				}
			case runtime.EventToolResult:
				if finishSeen || ev.Tool == nil || ev.Tool.ID == "" {
					continue
				}
				if _, seen := completed[ev.Tool.ID]; seen {
					slog.Warn("openai: duplicate native tool result", "tool_id", ev.Tool.ID, "session", sessionID)
					continue
				}
				tool := *ev.Tool
				if prior, ok := started[tool.ID]; ok {
					if tool.Name == "" {
						tool.Name = prior.Name
					}
					if tool.Arguments == nil {
						tool.Arguments = prior.Arguments
					}
				}
				summary := formatToolExecutionSummary(tool)
				completed[tool.ID] = struct{}{}
				h.recordServerToolID(sessionID, tool.ID)
				if err := writeChunk(base.withChoices(streamChoice{
					Index: 0,
					Delta: streamDelta{ReasoningContent: summary},
				})); err != nil {
					return runtime.RunCancelled
				}
			case runtime.EventUsage:
				if ev.Usage != nil {
					usage = usageFromRuntime(ev.Usage)
				}
			case runtime.EventError:
				// A worker-boundary deadline whose abort did not settle: the
				// turn's completion outcome is unknown. Present the unified
				// timeout error; the worker already aborted (and escalated)
				// natively, so no further API-side abort is needed.
				if ev.Error == runtime.TurnDeadlineExceeded {
					status = "timeout"
					runStatus = runtime.RunOutcomeUnknown
					deferredAbort = false
					break loop
				}
				if err := writeSSEJSON(w, flusher, errorBody{Error: serverError(ev.Error)}); err != nil {
					return runStatus
				}
				status = "error"
				runStatus = runtime.RunFailed
				break loop
			case runtime.EventFinish:
				if ev.NativeSessionID != "" {
					_, _ = h.store.Update(context.Background(), sessionID, callerID, func(rec *runtime.SessionRecord) { rec.NativeSessionID = ev.NativeSessionID })
				}
				// The worker boundary aborted this turn at its deadline and
				// the abort settled: a confirmed timeout, never a completion.
				if ev.FinishReason == runtime.FinishReasonTimeout {
					status = "timeout"
					runStatus = runtime.RunTimedOut
					deferredAbort = false
					break loop
				}
				finish = ev.FinishReason
				finishSeen = true
				// Usage may arrive just after the finish marker; drain briefly.
				if graceTimer != nil {
					graceTimer.Stop()
				}
				graceTimer = time.NewTimer(h.usageGrace)
				grace = graceTimer.C
			}
		case <-grace:
			break loop
		case <-timeout:
			status = "timeout"
			h.markTurnSettling(sessionID)
			settled := h.abortAndDrain(context.Background(), sessionID, handle)
			deferredAbort = false
			if settled {
				runStatus = runtime.RunTimedOut
			} else {
				runStatus = runtime.RunOutcomeUnknown
			}
			break loop
		}
	}

	if status == "normal" {
		// Final chunk with the mapped finish reason.
		fr := mapFinishReason(finish)
		if err := writeChunk(base.withChoices(streamChoice{
			Index:        0,
			Delta:        streamDelta{},
			FinishReason: &fr,
		})); err != nil {
			return runStatus
		}
		if includeUsage && usage != nil {
			chunk := base
			chunk.Choices = []streamChoice{}
			chunk.Usage = usage
			if err := writeChunk(chunk); err != nil {
				return runStatus
			}
		}
	} else if status == "timeout" {
		// Unified timeout presentation (O-F09b): the same error shape the
		// non-streaming surface returns with HTTP 504. Never a normal
		// completion chunk — a deadline is not a token-length limit.
		if err := writeSSEJSON(w, flusher, errorBody{Error: turnTimeoutError()}); err != nil {
			return runStatus
		}
	}
	// A timeout via the API timer has already drained and disabled the
	// deferred abort in its branch; the worker-reported timeout paths set
	// deferredAbort=false for the same reason. Nothing is left to kill.
	if status != "timeout" {
		deferredAbort = false
	}

	// Always terminate the stream with the DONE frame.
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
		flusher.Flush()
	}
	return runStatus
}

// writeSSEJSON writes one "data: <json>\n\n" event.
func writeSSEJSON(w io.Writer, flusher http.Flusher, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
