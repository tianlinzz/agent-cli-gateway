package server

// CreateSessionRequest is the body of POST /session.
type CreateSessionRequest struct {
	Title        string                 `json:"title,omitempty"`
	Agent        string                 `json:"agent"`
	WorkDir      string                 `json:"workDir,omitempty"`
	Model        string                 `json:"model,omitempty"`
	Mode         string                 `json:"mode,omitempty"`
	SystemPrompt string                 `json:"systemPrompt,omitempty"`
	// SessionID, if set, tells the agent driver to resume an existing
	// conversation (e.g. claudecode adds --resume <id>). Empty = fresh session.
	SessionID string                 `json:"sessionId,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// CreateSessionResponse is the body of POST /session (200).
type CreateSessionResponse struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Model     string `json:"model,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// PromptAsyncRequest is the body of POST /session/:id/prompt_async.
type PromptAsyncRequest struct {
	Parts []PromptPart `json:"parts"`
}

// PromptPart is one part of a prompt.
type PromptPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// AbortResponse is the body of POST /session/:id/abort.
type AbortResponse struct {
	OK bool `json:"ok"`
}

// HealthResponse is the body of GET /health.
type HealthResponse struct {
	OK     bool     `json:"ok"`
	Agents []string `json:"agents,omitempty"`
}

// ProviderListResponse is the body of GET /config/providers.
type ProviderListResponse struct {
	Providers []ProviderEntry `json:"providers"`
}

// ProviderEntry is one provider in the discovery response.
type ProviderEntry struct {
	ID     string                `json:"id"`
	Models map[string]ModelEntry `json:"models"`
}

// ModelEntry describes one model.
type ModelEntry struct {
	Name string `json:"name,omitempty"`
}
