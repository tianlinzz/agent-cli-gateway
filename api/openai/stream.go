package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

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
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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
func (h *Handler) streamTurn(w http.ResponseWriter, r *http.Request, ctx context.Context, model, sessionID, callerID string, handle runtime.ExecutionHandle, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, serverError("streaming not supported by the response writer"))
		return
	}
	// Guard: unless the stream completes normally, the execution must be
	// aborted so a dropped client or a mid-stream write failure never leaves
	// an orphaned agent turn running.
	deferredAbort := true
	defer func() {
		if deferredAbort {
			h.markTurnSettling(sessionID)
			h.abortAndDrain(context.Background(), sessionID, handle)
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
		return
	}

	var (
		usage      *usageInfo
		finish     = ""
		finishSeen = false
		status     = "normal" // "normal" | "error" | "timeout"
	)
	var grace <-chan time.Time
	var timeout <-chan time.Time
	if h.turnTimeout > 0 {
		timeout = time.After(h.turnTimeout)
	}

loop:
	for {
		select {
		case <-ctx.Done():
			// Client disconnect or explicit abort: settle this turn and discard
			// its terminal events before another request can reuse the session.
			h.markTurnSettling(sessionID)
			h.abortAndDrain(context.Background(), sessionID, handle)
			deferredAbort = false
			if r.Context().Err() != nil {
				return // the client is gone; nothing more to write
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
					return
				}
			case runtime.EventToolUse:
				// Native Agent tools are already executed. Do not emit OpenAI
				// delta.tool_calls, which would start a second orchestration loop.
			case runtime.EventNativeSession:
				if ev.NativeSessionID != "" {
					_, _ = h.store.Update(context.Background(), sessionID, callerID, func(rec *runtime.SessionRecord) { rec.NativeSessionID = ev.NativeSessionID })
				}
			case runtime.EventToolResult:
				// Tool results are not surfaced in OpenAI chat responses.
			case runtime.EventUsage:
				if ev.Usage != nil {
					usage = usageFromRuntime(ev.Usage)
				}
			case runtime.EventError:
				if err := writeSSEJSON(w, flusher, errorBody{Error: serverError(ev.Error)}); err != nil {
					return
				}
				status = "error"
				break loop
			case runtime.EventFinish:
				if ev.NativeSessionID != "" {
					_, _ = h.store.Update(context.Background(), sessionID, callerID, func(rec *runtime.SessionRecord) { rec.NativeSessionID = ev.NativeSessionID })
				}
				finish = ev.FinishReason
				finishSeen = true
				// Usage may arrive just after the finish marker; drain briefly.
				grace = time.After(h.usageGrace)
			}
		case <-grace:
			break loop
		case <-timeout:
			finish = "length"
			status = "timeout"
			h.markTurnSettling(sessionID)
			h.abortAndDrain(context.Background(), sessionID, handle)
			deferredAbort = false
			break loop
		}
	}

	if status != "error" {
		// Final chunk with the mapped finish reason.
		fr := mapFinishReason(finish)
		if err := writeChunk(base.withChoices(streamChoice{
			Index:        0,
			Delta:        streamDelta{},
			FinishReason: &fr,
		})); err != nil {
			return
		}
		if includeUsage && usage != nil {
			chunk := base
			chunk.Choices = []streamChoice{}
			chunk.Usage = usage
			if err := writeChunk(chunk); err != nil {
				return
			}
		}
	}
	// On a clean finish or a worker-reported error the turn is over on the
	// worker side, so nothing is left to kill. A timeout means the agent may
	// still be running: the deferred abort kills it after the stream ends.
	if status != "timeout" {
		deferredAbort = false
	}

	// Always terminate the stream with the DONE frame.
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err == nil {
		flusher.Flush()
	}
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
