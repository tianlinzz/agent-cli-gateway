package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// Handlers holds all HTTP handler methods, bound to a SessionStore.
type Handlers struct {
	Store *SessionStore
}

// HandleHealth responds to GET /health.
func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		OK:     true,
		Agents: h.Store.ListAgents(),
	})
}

// HandleCreateSession responds to POST /session.
func (h *Handlers) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent field is required")
		return
	}

	// Use a detached context — the session must outlive the HTTP request that
	// created it. If r.Context() were used, the session's underlying process
	// would be killed when the response is sent (request context cancelled).
	// But we still need the caller identity for ownership tracking, so carry it
	// over from the request context onto context.Background().
	ctx := context.WithValue(context.Background(), identityKey{}, UserIDFromContext(r.Context()))
	managed, err := h.Store.CreateSession(ctx, req)
	if err != nil {
		slog.Warn("create session failed", "agent", req.Agent, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, CreateSessionResponse{
		ID:        managed.ID,
		Agent:     managed.AgentName,
		Model:     managed.Model,
		CreatedAt: managed.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// HandlePromptAsync responds to POST /session/:id/prompt_async.
func (h *Handlers) HandlePromptAsync(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	userID := UserIDFromContext(r.Context())
	managed, ok := h.Store.GetSessionForOwner(sessionID, userID)
	if !ok {
		if h.Store.HasSession(sessionID) {
			writeError(w, http.StatusForbidden, "session does not belong to caller")
		} else {
			writeError(w, http.StatusNotFound, "session not found: "+sessionID)
		}
		return
	}
	if !managed.Session.Alive() {
		writeError(w, http.StatusGone, "session is no longer alive: "+sessionID)
		return
	}

	var req PromptAsyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// Extract the text from parts.
	prompt := ""
	for _, part := range req.Parts {
		if part.Type == "text" && part.Text != "" {
			if prompt != "" {
				prompt += "\n"
			}
			prompt += part.Text
		}
	}

	if err := managed.Session.Send(prompt, nil, nil); err != nil {
		slog.Warn("send prompt failed", "session", sessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to send prompt: "+err.Error())
		return
	}

	// Mark activity + in-flight for TTL reaper (consumed by Task 1.5).
	h.Store.MarkActivity(sessionID, true)

	w.WriteHeader(http.StatusNoContent)
}

// HandleEventStream responds to GET /event?session=:id.
func (h *Handlers) HandleEventStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "session query parameter is required")
		return
	}
	userID := UserIDFromContext(r.Context())
	managed, ok := h.Store.GetSessionForOwner(sessionID, userID)
	if !ok {
		if h.Store.HasSession(sessionID) {
			writeError(w, http.StatusForbidden, "session does not belong to caller")
		} else {
			writeError(w, http.StatusNotFound, "session not found: "+sessionID)
		}
		return
	}

	// NOTE: SSE connection no longer affects InFlight. Per spec §7.4
	// ("/event 流读取不豁免 TTL"), reading the event stream does NOT exempt a
	// session from the TTL reaper. InFlight is instead set true on prompt
	// dispatch (HandlePromptAsync → MarkActivity(id, true)) and cleared on the
	// terminal result/error event inside streamSessionEvents. This matches the
	// "turn-based" semantics of the reaper's in-flight exemption.

	ctx := r.Context()

	if err := streamSessionEvents(ctx, w, managed.Session, h.Store, sessionID); err != nil {
		slog.Debug("event stream ended", "session", sessionID, "error", err)
	}
}

// HandleAbort responds to POST /session/:id/abort.
func (h *Handlers) HandleAbort(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	userID := UserIDFromContext(r.Context())
	// AbortSession needs an ownership-aware variant OR we check here first.
	if _, ok := h.Store.GetSessionForOwner(sessionID, userID); !ok {
		if h.Store.HasSession(sessionID) {
			writeError(w, http.StatusForbidden, "session does not belong to caller")
		} else {
			writeError(w, http.StatusNotFound, "session not found: "+sessionID)
		}
		return
	}
	ok := h.Store.AbortSession(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found: "+sessionID)
		return
	}
	writeJSON(w, http.StatusOK, AbortResponse{OK: true})
}

// HandleConfigProviders responds to GET /config/providers.
func (h *Handlers) HandleConfigProviders(w http.ResponseWriter, r *http.Request) {
	providerMap := h.Store.CollectProviders(r.Context())
	providers := make([]ProviderEntry, 0, len(providerMap))
	for pid, models := range providerMap {
		entry := ProviderEntry{ID: pid, Models: make(map[string]ModelEntry)}
		for _, m := range models {
			entry.Models[m.Name] = ModelEntry{Name: m.Desc}
		}
		providers = append(providers, entry)
	}
	writeJSON(w, http.StatusOK, ProviderListResponse{Providers: providers})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
