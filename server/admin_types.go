package server

import (
	"github.com/tianlinzz/agent-cli-gateway/core"
)

// LiveProviderRequest is the body of PUT /config/agents/{agent}/provider.
// All fields are optional; omitting a field leaves it unchanged is NOT the
// behaviour — the management API writes exactly what is sent. Send "" to
// explicitly clear a field.
type LiveProviderRequest struct {
	APIKey  string            `json:"apiKey,omitempty"`
	BaseURL string            `json:"baseUrl,omitempty"`
	Model   string            `json:"model,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// LiveProviderResponse is the body of GET /config/agents/{agent}/provider and
// mirrors LiveProviderRequest.
type LiveProviderResponse struct {
	Agent   string            `json:"agent"`
	APIKey  string            `json:"apiKey,omitempty"`
	BaseURL string            `json:"baseUrl,omitempty"`
	Model   string            `json:"model,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// toLiveConfig converts the admin DTO to the core type used by agents.
func toLiveConfig(r LiveProviderRequest) core.LiveProviderConfig {
	return core.LiveProviderConfig{
		APIKey:  r.APIKey,
		BaseURL: r.BaseURL,
		Model:   r.Model,
		Env:     r.Env,
	}
}

// fromLiveConfig converts the core type to the admin DTO.
func fromLiveConfig(agent string, c core.LiveProviderConfig) LiveProviderResponse {
	return LiveProviderResponse{
		Agent:   agent,
		APIKey:  c.APIKey,
		BaseURL: c.BaseURL,
		Model:   c.Model,
		Env:     c.Env,
	}
}

// McpServerRequest is the body of PUT /config/agents/{agent}/mcp/{name}.
type McpServerRequest struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// McpServerResponse is one MCP server in GET responses / PUT echoes.
type McpServerResponse struct {
	Agent   string            `json:"agent"`
	Name    string            `json:"name"`
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// McpListResponse is the body of GET /config/agents/{agent}/mcp.
type McpListResponse struct {
	Agent   string                       `json:"agent"`
	Servers map[string]McpServerResponse `json:"servers"`
}

func toMcpServerConfig(r McpServerRequest) core.McpServerConfig {
	return core.McpServerConfig{
		Type:    r.Type,
		Command: r.Command,
		Args:    r.Args,
		Env:     r.Env,
		URL:     r.URL,
	}
}

func toMcpServerResponse(agent, name string, c core.McpServerConfig) McpServerResponse {
	return McpServerResponse{
		Agent:   agent,
		Name:    name,
		Type:    c.Type,
		Command: c.Command,
		Args:    c.Args,
		Env:     c.Env,
		URL:     c.URL,
	}
}

// SessionListItem is one entry in the GET /sessions response.
// Field naming follows cc-switch's SessionMeta (camelCase JSON, Unix-second
// timestamps) so that downstream tooling can reuse the same shape.
type SessionListItem struct {
	Agent        string `json:"agent"`
	ID           string `json:"id"`
	Summary      string `json:"summary,omitempty"`
	MessageCount int    `json:"messageCount,omitempty"`
	ModifiedAt   int64  `json:"modifiedAt,omitempty"` // Unix seconds
	GitBranch    string `json:"gitBranch,omitempty"`
	WorkDir      string `json:"workDir,omitempty"` // absolute cwd the session ran in
}

// SessionListResponse is the body of GET /sessions?agent={name}.
type SessionListResponse struct {
	Agent    string            `json:"agent"`
	Sessions []SessionListItem `json:"sessions"`
}

// WorkspaceListItem is one entry in the GET /workspaces response.
type WorkspaceListItem struct {
	Agent        string `json:"agent"`
	Path         string `json:"path"`
	SessionCount int    `json:"sessionCount"`
	LastActive   int64  `json:"lastActive,omitempty"` // Unix seconds
}

// WorkspaceListResponse is the body of GET /workspaces?agent={name}.
type WorkspaceListResponse struct {
	Agent      string              `json:"agent"`
	Workspaces []WorkspaceListItem `json:"workspaces"`
}

// SessionHistoryResponse is the body of GET /sessions/{agent}/{id}/history.
type SessionHistoryResponse struct {
	Agent   string            `json:"agent"`
	ID      string            `json:"id"`
	Entries []HistoryEntryDTO `json:"entries"`
}

// HistoryEntryDTO is one turn in a conversation history.
type HistoryEntryDTO struct {
	Role      string `json:"role"`
	Kind      string `json:"kind"`
	Phase     string `json:"phase,omitempty"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp,omitempty"` // Unix seconds
}

// SessionResumeResponse is the body of GET /sessions/{agent}/{id}/resume.
// It returns the native CLI resume command for reference and a hint describing
// how to actually resume via the gateway (POST /session with sessionId).
type SessionResumeResponse struct {
	Agent         string `json:"agent"`
	ID            string `json:"id"`
	ResumeCommand string `json:"resumeCommand,omitempty"`
	GatewayHint   string `json:"gatewayHint"`
}

// toHistoryDTO converts a core.HistoryEntry (which carries time.Time) into the
// Unix-second DTO used by the admin API.
func toHistoryDTO(e core.HistoryEntry) HistoryEntryDTO {
	return HistoryEntryDTO{
		Role:      e.Role,
		Kind:      historyEntryKind(e),
		Phase:     e.Phase,
		Content:   e.Content,
		Timestamp: e.Timestamp.Unix(),
	}
}

func historyEntryKind(e core.HistoryEntry) string {
	if e.Kind != "" {
		return e.Kind
	}
	switch e.Role {
	case "user":
		return "user"
	case "assistant":
		return "assistant_final"
	default:
		return e.Role
	}
}
