package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// sseEventPayload is the JSON-serialized form of a core.Event sent as an SSE data line.
// It carries a practical subset of core.Event fields (message.go:233-254) — the fields
// most useful to HTTP consumers. Fields like Questions, cache token counts, Metadata,
// and Synthetic are omitted as they are not needed by the gateway's HTTP API.
type sseEventPayload struct {
	Type         string                 `json:"type"`
	Content      string                 `json:"content,omitempty"`
	ToolName     string                 `json:"toolName,omitempty"`
	ToolInput    string                 `json:"toolInput,omitempty"`
	ToolInputRaw map[string]interface{} `json:"toolInputRaw,omitempty"`
	ToolResult   string                 `json:"toolResult,omitempty"`
	ToolStatus   string                 `json:"toolStatus,omitempty"`
	ToolExitCode *int                   `json:"toolExitCode,omitempty"`
	ToolSuccess  *bool                  `json:"toolSuccess,omitempty"`
	SessionID    string                 `json:"sessionID,omitempty"`
	RequestID    string                 `json:"requestID,omitempty"`
	Done         bool                   `json:"done,omitempty"`
	InputTokens  int                    `json:"inputTokens,omitempty"`
	OutputTokens int                    `json:"outputTokens,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// eventToPayload converts a core.Event to the SSE JSON payload.
func eventToPayload(event core.Event) sseEventPayload {
	p := sseEventPayload{
		Type:         string(event.Type),
		Content:      event.Content,
		ToolName:     event.ToolName,
		ToolInput:    event.ToolInput,
		ToolInputRaw: event.ToolInputRaw,
		ToolResult:   event.ToolResult,
		ToolStatus:   event.ToolStatus,
		ToolExitCode: event.ToolExitCode,
		ToolSuccess:  event.ToolSuccess,
		SessionID:    event.SessionID,
		RequestID:    event.RequestID,
		Done:         event.Done,
		InputTokens:  event.InputTokens,
		OutputTokens: event.OutputTokens,
	}
	if event.Error != nil {
		p.Error = event.Error.Error()
	}
	return p
}

// writeSSEEvent writes one SSE frame to the ResponseWriter.
func writeSSEEvent(w http.ResponseWriter, event core.Event) error {
	payload := eventToPayload(event)
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal SSE event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", data); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeSSEDone writes the terminal SSE frame and closes the stream.
func writeSSEDone(w http.ResponseWriter) {
	fmt.Fprint(w, "event: done\ndata: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// streamSessionEvents consumes the agent session's Events() channel and writes
// each event as an SSE frame. It blocks until the turn ends (result event with
// Done=true) or the context is cancelled.
//
// On a terminal event (result with Done=true, or an error event) the session's
// InFlight flag is cleared via store.SetInFlight so the TTL reaper stops
// exempting it — this matches the spec's "turn-based" in-flight semantics. The
// store/sessionID are optional only in the sense that they're always supplied by
// the production handler; tests that pass nil skip the clear.
//
// NOTE: core.AgentSession.Events() returns a single shared channel. This function
// assumes at most one concurrent consumer per session — the "each turn opens a
// fresh /event connection" design guarantees this. Multiple concurrent SSE clients
// on the same session would split events unpredictably.
func streamSessionEvents(ctx context.Context, w http.ResponseWriter, session core.AgentSession, store *SessionStore, sessionID string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	clearInFlight := func() {
		if store != nil && sessionID != "" {
			store.SetInFlight(sessionID, false)
		}
	}

	events := session.Events()
	for {
		select {
		case <-ctx.Done():
			clearInFlight()
			writeSSEDone(w)
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				// Channel closed — session ended without a result event.
				clearInFlight()
				writeSSEDone(w)
				return nil
			}
			if err := writeSSEEvent(w, event); err != nil {
				return err
			}
			// Terminal events signal end of turn. Clear InFlight so the TTL
			// reaper stops exempting this session. Fire-and-forget prompts
			// (no /event reader) never reach this path — for those, the
			// reaper's 2×TTL grace fallback in reapIdle bounds the leak.
			if event.Type == core.EventResult && event.Done {
				clearInFlight()
				writeSSEDone(w)
				return nil
			}
			if event.Type == core.EventError {
				clearInFlight()
				writeSSEDone(w)
				return nil
			}
		}
	}
}
