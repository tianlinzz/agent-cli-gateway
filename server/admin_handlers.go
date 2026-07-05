package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// AdminHandlers holds management/inspection HTTP handlers. Unlike the live
// session Handlers (which operate on a caller's ManagedSession), these reach
// the agent singletons directly via SessionStore.Agent to read backend state
// (sessions on disk, history) without creating a live process.
type AdminHandlers struct {
	Store *SessionStore
}

// HandleListSessions responds to GET /sessions?agent={name}.
// Returns sessions known to the agent backend (e.g. ~/.claude/projects/*,
// ~/.codex/sessions/*). agent is required.
func (h *AdminHandlers) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	agentName := r.URL.Query().Get("agent")
	if agentName == "" {
		writeError(w, http.StatusBadRequest, "agent query parameter is required")
		return
	}
	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}

	sessions, err := agent.ListSessions(r.Context())
	if err != nil {
		slog.Warn("list sessions failed", "agent", agentName, "error", err)
		writeError(w, http.StatusInternalServerError, "list sessions failed: "+err.Error())
		return
	}

	items := make([]SessionListItem, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, SessionListItem{
			Agent:        agentName,
			ID:           s.ID,
			Summary:      s.Summary,
			MessageCount: s.MessageCount,
			ModifiedAt:   s.ModifiedAt.Unix(),
			GitBranch:    s.GitBranch,
		})
	}
	writeJSON(w, http.StatusOK, SessionListResponse{Agent: agentName, Sessions: items})
}

// HandleSessionHistory responds to GET /sessions/{agent}/{id}/history?limit=N.
// Requires the agent to implement core.HistoryProvider; otherwise 501.
func (h *AdminHandlers) HandleSessionHistory(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	sessionID := r.PathValue("id")
	limit := parseLimit(r)

	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	hp, ok := agent.(core.HistoryProvider)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not support history: "+agentName)
		return
	}

	entries, err := hp.GetSessionHistory(r.Context(), sessionID, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Warn("get session history failed", "agent", agentName, "session", sessionID, "error", err)
		// Missing session file is a common case — surface as 404.
		status := http.StatusInternalServerError
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, "get history failed: "+err.Error())
		return
	}

	dto := make([]HistoryEntryDTO, 0, len(entries))
	for _, e := range entries {
		dto = append(dto, toHistoryDTO(e))
	}
	writeJSON(w, http.StatusOK, SessionHistoryResponse{Agent: agentName, ID: sessionID, Entries: dto})
}

// HandleDeleteSession responds to DELETE /sessions/{agent}/{id}.
// Requires the agent to implement core.SessionDeleter; otherwise 501.
// This deletes the on-disk transcript file only — it does not abort any live
// gateway session that happens to be using the same id. Call POST
// /session/{id}/abort first if you need to stop a running session.
func (h *AdminHandlers) HandleDeleteSession(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	sessionID := r.PathValue("id")

	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	del, ok := agent.(core.SessionDeleter)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not support session deletion: "+agentName)
		return
	}

	if err := del.DeleteSession(r.Context(), sessionID); err != nil {
		slog.Warn("delete session failed", "agent", agentName, "session", sessionID, "error", err)
		status := http.StatusInternalServerError
		if isNotFound(err) {
			status = http.StatusNotFound
		}
		writeError(w, status, "delete session failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agent": agentName, "id": sessionID})
}

// HandleSessionResume responds to GET /sessions/{agent}/{id}/resume.
// Returns the native CLI resume command for reference. To actually resume via
// the gateway, POST /session with {"agent":"...","sessionId":"<id>"}.
func (h *AdminHandlers) HandleSessionResume(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	sessionID := r.PathValue("id")

	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}

	cmd := ""
	if rc, ok := agent.(core.ResumeCommander); ok {
		cmd = rc.ResumeCommand(sessionID)
	}
	writeJSON(w, http.StatusOK, SessionResumeResponse{
		Agent:         agentName,
		ID:            sessionID,
		ResumeCommand: cmd,
		GatewayHint:   fmt.Sprintf("POST /session with {\"agent\":%q,\"sessionId\":%q} to resume via gateway", agentName, sessionID),
	})
}

// parseLimit reads the optional limit query param. 0/missing/invalid → 0
// (interpreted by agents as "no limit").
func parseLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// isNotFound reports whether err is a "session file not found"-style error.
// Agents use varying message phrasing; match on a case-insensitive substring.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such file")
}

// -----------------------------------------------------------------------------
// Provider live config (GET/PUT /config/agents/{agent}/provider)
// -----------------------------------------------------------------------------
//
// These endpoints read/write the agent's underlying CLI live config files
// (e.g. ~/.claude/settings.json, ~/.codex/auth.json + config.toml). The gateway
// does NOT inject this provider into running sessions — the CLI reads it
// itself on the next session start (cc-switch "file management only" mode).
//
// Note: writing a provider affects ALL callers' subsequent sessions because
// the live config file is shared. This is documented behaviour for v1.

// HandleReadLiveProvider responds to GET /config/agents/{agent}/provider.
func (h *AdminHandlers) HandleReadLiveProvider(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	lcp, ok := agent.(core.LiveConfigProvider)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not expose live provider config: "+agentName)
		return
	}
	cfg, err := lcp.ReadLiveProvider(r.Context())
	if err != nil {
		slog.Warn("read live provider failed", "agent", agentName, "error", err)
		writeError(w, http.StatusInternalServerError, "read provider failed: "+err.Error())
		return
	}
	// Redact the API key in the response by default? No — this is an admin
	// endpoint that exists specifically to manage the key, and the caller has
	// already authenticated via the gateway token. Surface it verbatim.
	writeJSON(w, http.StatusOK, fromLiveConfig(agentName, cfg))
}

// HandleWriteLiveProvider responds to PUT /config/agents/{agent}/provider.
// The request body is the full provider config; all fields present are
// written, missing/empty fields are cleared (NOT merged with existing).
func (h *AdminHandlers) HandleWriteLiveProvider(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	lcp, ok := agent.(core.LiveConfigProvider)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not expose live provider config: "+agentName)
		return
	}

	var req LiveProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	cfg := toLiveConfig(req)
	if err := lcp.WriteLiveProvider(r.Context(), cfg); err != nil {
		slog.Warn("write live provider failed", "agent", agentName, "error", err)
		writeError(w, http.StatusInternalServerError, "write provider failed: "+err.Error())
		return
	}

	// Read back so the caller sees the canonical on-disk result.
	got, err := lcp.ReadLiveProvider(r.Context())
	if err != nil {
		// Write succeeded but read-back failed — still report success, just
		// without a body echo (the write is what matters).
		writeJSON(w, http.StatusOK, map[string]any{"agent": agentName, "ok": true})
		return
	}
	writeJSON(w, http.StatusOK, fromLiveConfig(agentName, got))
}

// -----------------------------------------------------------------------------
// MCP servers (GET /config/agents/{agent}/mcp, PUT/DELETE .../mcp/{name})
// -----------------------------------------------------------------------------

// HandleListMcpServers responds to GET /config/agents/{agent}/mcp.
func (h *AdminHandlers) HandleListMcpServers(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	mm, ok := agent.(core.McpConfigManager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not expose mcp config: "+agentName)
		return
	}
	servers, err := mm.ListMcpServers(r.Context())
	if err != nil {
		slog.Warn("list mcp servers failed", "agent", agentName, "error", err)
		writeError(w, http.StatusInternalServerError, "list mcp failed: "+err.Error())
		return
	}
	out := make(map[string]McpServerResponse, len(servers))
	for name, c := range servers {
		out[name] = toMcpServerResponse(agentName, name, c)
	}
	writeJSON(w, http.StatusOK, McpListResponse{Agent: agentName, Servers: out})
}

// HandleSaveMcpServer responds to PUT /config/agents/{agent}/mcp/{name}.
func (h *AdminHandlers) HandleSaveMcpServer(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "mcp server name is required in the path")
		return
	}
	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	mm, ok := agent.(core.McpConfigManager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not expose mcp config: "+agentName)
		return
	}

	var req McpServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	cfg := toMcpServerConfig(req)
	if err := mm.SaveMcpServer(r.Context(), name, cfg); err != nil {
		slog.Warn("save mcp server failed", "agent", agentName, "name", name, "error", err)
		// Validation errors come back as plain messages; surface as 400.
		status := http.StatusInternalServerError
		if isValidationError(err) {
			status = http.StatusBadRequest
		}
		writeError(w, status, "save mcp failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toMcpServerResponse(agentName, name, cfg))
}

// HandleDeleteMcpServer responds to DELETE /config/agents/{agent}/mcp/{name}.
func (h *AdminHandlers) HandleDeleteMcpServer(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "mcp server name is required in the path")
		return
	}
	agent, ok := h.Store.Agent(agentName)
	if !ok {
		writeError(w, http.StatusNotFound, "agent not found: "+agentName)
		return
	}
	mm, ok := agent.(core.McpConfigManager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "agent does not expose mcp config: "+agentName)
		return
	}

	if err := mm.DeleteMcpServer(r.Context(), name); err != nil {
		slog.Warn("delete mcp server failed", "agent", agentName, "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, "delete mcp failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agentName, "name": name, "deleted": true})
}

// isValidationError distinguishes agent-side validation errors (we sent back
// messages like "stdio mcp server requires a command") from real I/O failures,
// so the handler can map them to 400 instead of 500.
func isValidationError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "requires") || strings.Contains(msg, "is required")
}
