package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// adminStubAgent implements core.Agent plus the optional interfaces used by the
// admin handlers: HistoryProvider, SessionDeleter, ResumeCommander,
// LiveConfigProvider. It is separate from mockAgent (used by the live-session
// tests) so the two test families stay independent and the stub stays focused
// on admin behavior.
type adminStubAgent struct {
	name        string
	listErr     error
	sessions    []core.AgentSessionInfo
	history     map[string][]core.HistoryEntry // sessionID → entries
	historyErr  error
	deleteErr   error
	deleteCalls []string
	resumeCmd   string // fixed ResumeCommand output; "" → still implemented, returns ""

	// LiveConfigProvider state
	liveCfg    core.LiveProviderConfig
	liveReadErr  error
	liveWriteErr error
	writeCalls   []core.LiveProviderConfig

	// McpConfigManager state
	mcpServers    map[string]core.McpServerConfig
	mcpListErr    error
	mcpSaveErr    error
	mcpDeleteErr  error
	mcpSaveCalls  []struct {
		name string
		cfg  core.McpServerConfig
	}
	mcpDeleteCalls []string
}

func newAdminStubAgent(name string) *adminStubAgent {
	return &adminStubAgent{
		name:    name,
		history: make(map[string][]core.HistoryEntry),
	}
}

func (a *adminStubAgent) Name() string { return a.name }
func (a *adminStubAgent) Stop() error  { return nil }
func (a *adminStubAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	return nil, errors.New("not used in admin tests")
}
func (a *adminStubAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	return a.sessions, nil
}

// Optional: HistoryProvider
func (a *adminStubAgent) GetSessionHistory(ctx context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	if a.historyErr != nil {
		return nil, a.historyErr
	}
	entries := a.history[sessionID]
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// Optional: SessionDeleter
func (a *adminStubAgent) DeleteSession(ctx context.Context, sessionID string) error {
	a.deleteCalls = append(a.deleteCalls, sessionID)
	if a.deleteErr != nil {
		return a.deleteErr
	}
	return nil
}

// Optional: ResumeCommander
func (a *adminStubAgent) ResumeCommand(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return a.name + " --resume " + sessionID
}

// Optional: LiveConfigProvider
func (a *adminStubAgent) ReadLiveProvider(ctx context.Context) (core.LiveProviderConfig, error) {
	if a.liveReadErr != nil {
		return core.LiveProviderConfig{}, a.liveReadErr
	}
	return a.liveCfg, nil
}
func (a *adminStubAgent) WriteLiveProvider(ctx context.Context, cfg core.LiveProviderConfig) error {
	a.writeCalls = append(a.writeCalls, cfg)
	if a.liveWriteErr != nil {
		return a.liveWriteErr
	}
	a.liveCfg = cfg
	return nil
}

// Optional: McpConfigManager
func (a *adminStubAgent) ListMcpServers(ctx context.Context) (map[string]core.McpServerConfig, error) {
	if a.mcpListErr != nil {
		return nil, a.mcpListErr
	}
	if a.mcpServers == nil {
		return map[string]core.McpServerConfig{}, nil
	}
	// return a copy so tests can mutate without side effects on the stub
	out := make(map[string]core.McpServerConfig, len(a.mcpServers))
	for k, v := range a.mcpServers {
		out[k] = v
	}
	return out, nil
}
func (a *adminStubAgent) SaveMcpServer(ctx context.Context, name string, cfg core.McpServerConfig) error {
	a.mcpSaveCalls = append(a.mcpSaveCalls, struct {
		name string
		cfg  core.McpServerConfig
	}{name, cfg})
	if a.mcpSaveErr != nil {
		return a.mcpSaveErr
	}
	if a.mcpServers == nil {
		a.mcpServers = make(map[string]core.McpServerConfig)
	}
	a.mcpServers[name] = cfg
	return nil
}
func (a *adminStubAgent) DeleteMcpServer(ctx context.Context, name string) error {
	a.mcpDeleteCalls = append(a.mcpDeleteCalls, name)
	if a.mcpDeleteErr != nil {
		return a.mcpDeleteErr
	}
	if a.mcpServers != nil {
		delete(a.mcpServers, name)
	}
	return nil
}

// adminStubAgentWithoutCapabilities implements only the required Agent
// interface — no HistoryProvider / SessionDeleter / ResumeCommander — to
// exercise the 501 Not Implemented path.
type adminStubAgentBare struct{ name string }

func (a *adminStubAgentBare) Name() string                                            { return a.name }
func (a *adminStubAgentBare) Stop() error                                             { return nil }
func (a *adminStubAgentBare) StartSession(context.Context, string) (core.AgentSession, error) {
	return nil, errors.New("not used")
}
func (a *adminStubAgentBare) ListSessions(context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

func setupAdminStore(agents ...core.Agent) *SessionStore {
	m := make(map[string]core.Agent, len(agents))
	for _, a := range agents {
		m[a.Name()] = a
	}
	return NewSessionStore(m)
}

// -----------------------------------------------------------------------------
// GET /sessions
// -----------------------------------------------------------------------------

func TestHandleListSessions_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	agent.sessions = []core.AgentSessionInfo{
		{ID: "sess-a", Summary: "fix bug", MessageCount: 4, ModifiedAt: time.Unix(1750000000, 0)},
		{ID: "sess-b", Summary: "add tests", MessageCount: 2, ModifiedAt: time.Unix(1750000100, 0)},
	}
	store := setupAdminStore(agent)
	h := &AdminHandlers{Store: store}

	req := httptest.NewRequest(http.MethodGet, "/sessions?agent=claudecode", nil)
	w := httptest.NewRecorder()
	h.HandleListSessions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var resp SessionListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Agent != "claudecode" {
		t.Errorf("agent = %q, want claudecode", resp.Agent)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2", len(resp.Sessions))
	}
	// camelCase + Unix seconds field naming (cc-switch SessionMeta shape).
	if resp.Sessions[0].ID != "sess-a" {
		t.Errorf("sessions[0].id = %q, want sess-a", resp.Sessions[0].ID)
	}
	if resp.Sessions[0].ModifiedAt != 1750000000 {
		t.Errorf("sessions[0].modifiedAt = %d, want 1750000000", resp.Sessions[0].ModifiedAt)
	}
}

func TestHandleListSessions_MissingAgentParam(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	w := httptest.NewRecorder()
	h.HandleListSessions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleListSessions_UnknownAgent(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodGet, "/sessions?agent=gemini", nil)
	w := httptest.NewRecorder()
	h.HandleListSessions(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleListSessions_AgentError(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	agent.listErr = errors.New("scan failed")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodGet, "/sessions?agent=claudecode", nil)
	w := httptest.NewRecorder()
	h.HandleListSessions(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /sessions/{agent}/{id}/history
// -----------------------------------------------------------------------------

func TestHandleSessionHistory_OK(t *testing.T) {
	agent := newAdminStubAgent("codex")
	agent.history["sess-1"] = []core.HistoryEntry{
		{Role: "user", Content: "hello", Timestamp: time.Unix(1750000000, 0)},
		{Role: "assistant", Content: "hi there", Timestamp: time.Unix(1750000010, 0)},
	}
	store := setupAdminStore(agent)
	h := &AdminHandlers{Store: store}

	req := httptest.NewRequest(http.MethodGet, "/sessions/codex/sess-1/history", nil)
	req.SetPathValue("agent", "codex")
	req.SetPathValue("id", "sess-1")
	w := httptest.NewRecorder()
	h.HandleSessionHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var resp SessionHistoryResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(resp.Entries))
	}
	if resp.Entries[0].Role != "user" || resp.Entries[0].Content != "hello" {
		t.Errorf("entries[0] = %+v", resp.Entries[0])
	}
	if resp.Entries[0].Timestamp != 1750000000 {
		t.Errorf("entries[0].timestamp = %d, want 1750000000", resp.Entries[0].Timestamp)
	}
}

func TestHandleSessionHistory_LimitQuery(t *testing.T) {
	agent := newAdminStubAgent("codex")
	agent.history["s"] = []core.HistoryEntry{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply1"},
		{Role: "user", Content: "second"},
	}
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodGet, "/sessions/codex/s/history?limit=2", nil)
	req.SetPathValue("agent", "codex")
	req.SetPathValue("id", "s")
	w := httptest.NewRecorder()
	h.HandleSessionHistory(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp SessionHistoryResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	// limit=2 should return the LAST 2 entries.
	if len(resp.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(resp.Entries))
	}
	if resp.Entries[0].Content != "reply1" {
		t.Errorf("entries[0].content = %q, want reply1", resp.Entries[0].Content)
	}
}

func TestHandleSessionHistory_NotImplemented(t *testing.T) {
	// adminStubAgentBare does NOT implement HistoryProvider.
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}

	req := httptest.NewRequest(http.MethodGet, "/sessions/bare/x/history", nil)
	req.SetPathValue("agent", "bare")
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.HandleSessionHistory(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestHandleSessionHistory_UnknownAgent(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("codex"))}
	req := httptest.NewRequest(http.MethodGet, "/sessions/ghost/x/history", nil)
	req.SetPathValue("agent", "ghost")
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.HandleSessionHistory(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleSessionHistory_NotFoundError(t *testing.T) {
	// An agent returning a "not found"-style error should map to 404.
	agent := newAdminStubAgent("codex")
	agent.historyErr = errors.New("session file not found: ghost")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodGet, "/sessions/codex/ghost/history", nil)
	req.SetPathValue("agent", "codex")
	req.SetPathValue("id", "ghost")
	w := httptest.NewRecorder()
	h.HandleSessionHistory(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for not-found error, got %d", w.Code)
	}
}

// -----------------------------------------------------------------------------
// DELETE /sessions/{agent}/{id}
// -----------------------------------------------------------------------------

func TestHandleDeleteSession_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodDelete, "/sessions/claudecode/sess-x", nil)
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("id", "sess-x")
	w := httptest.NewRecorder()
	h.HandleDeleteSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if len(agent.deleteCalls) != 1 || agent.deleteCalls[0] != "sess-x" {
		t.Errorf("deleteCalls = %v, want [sess-x]", agent.deleteCalls)
	}
}

func TestHandleDeleteSession_NotImplemented(t *testing.T) {
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}

	req := httptest.NewRequest(http.MethodDelete, "/sessions/bare/x", nil)
	req.SetPathValue("agent", "bare")
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.HandleDeleteSession(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestHandleDeleteSession_NotFoundError(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	agent.deleteErr = errors.New("session file not found: ghost")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodDelete, "/sessions/claudecode/ghost", nil)
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("id", "ghost")
	w := httptest.NewRecorder()
	h.HandleDeleteSession(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// -----------------------------------------------------------------------------
// GET /sessions/{agent}/{id}/resume
// -----------------------------------------------------------------------------

func TestHandleSessionResume_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodGet, "/sessions/claudecode/sess-1/resume", nil)
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("id", "sess-1")
	w := httptest.NewRecorder()
	h.HandleSessionResume(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp SessionResumeResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ResumeCommand != "claudecode --resume sess-1" {
		t.Errorf("resumeCommand = %q", resp.ResumeCommand)
	}
	if resp.GatewayHint == "" {
		t.Error("gatewayHint should not be empty")
	}
}

func TestHandleSessionResume_UnknownAgent(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodGet, "/sessions/ghost/x/resume", nil)
	req.SetPathValue("agent", "ghost")
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.HandleSessionResume(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleSessionResume_AgentWithoutResumeCommander(t *testing.T) {
	// adminStubAgentBare does not implement ResumeCommander → resumeCommand
	// should be empty but the endpoint still returns 200.
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}

	req := httptest.NewRequest(http.MethodGet, "/sessions/bare/x/resume", nil)
	req.SetPathValue("agent", "bare")
	req.SetPathValue("id", "x")
	w := httptest.NewRecorder()
	h.HandleSessionResume(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp SessionResumeResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.ResumeCommand != "" {
		t.Errorf("resumeCommand = %q, want empty (no ResumeCommander)", resp.ResumeCommand)
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func TestParseLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"5", 5},
		{"0", 0},
		{"-1", 0},
		{"abc", 0},
		{"100", 100},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/x?limit="+c.in, nil)
		if got := parseLimit(r); got != c.want {
			t.Errorf("parseLimit(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	if !isNotFound(errors.New("session not found")) {
		t.Error("expected true for 'not found'")
	}
	if !isNotFound(errors.New("open: no such file or directory")) {
		t.Error("expected true for 'no such file'")
	}
	if isNotFound(errors.New("disk full")) {
		t.Error("expected false for unrelated error")
	}
	if isNotFound(nil) {
		t.Error("expected false for nil")
	}
}

// -----------------------------------------------------------------------------
// GET / PUT /config/agents/{agent}/provider
// -----------------------------------------------------------------------------

func TestHandleReadLiveProvider_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	agent.liveCfg = core.LiveProviderConfig{
		APIKey:  "sk-test",
		BaseURL: "https://relay.example.com",
		Model:   "claude-sonnet-4",
	}
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodGet, "/config/agents/claudecode/provider", nil)
	req.SetPathValue("agent", "claudecode")
	w := httptest.NewRecorder()
	h.HandleReadLiveProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var resp LiveProviderResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.APIKey != "sk-test" {
		t.Errorf("apiKey = %q", resp.APIKey)
	}
	if resp.BaseURL != "https://relay.example.com" {
		t.Errorf("baseUrl = %q", resp.BaseURL)
	}
}

func TestHandleReadLiveProvider_NotImplemented(t *testing.T) {
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}

	req := httptest.NewRequest(http.MethodGet, "/config/agents/bare/provider", nil)
	req.SetPathValue("agent", "bare")
	w := httptest.NewRecorder()
	h.HandleReadLiveProvider(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestHandleReadLiveProvider_UnknownAgent(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodGet, "/config/agents/ghost/provider", nil)
	req.SetPathValue("agent", "ghost")
	w := httptest.NewRecorder()
	h.HandleReadLiveProvider(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleWriteLiveProvider_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	body := `{"apiKey":"sk-new","baseUrl":"https://relay.example.com","model":"claude-sonnet-4"}`
	req := httptest.NewRequest(http.MethodPut, "/config/agents/claudecode/provider", strings.NewReader(body))
	req.SetPathValue("agent", "claudecode")
	w := httptest.NewRecorder()
	h.HandleWriteLiveProvider(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	// Write was forwarded.
	if len(agent.writeCalls) != 1 {
		t.Fatalf("writeCalls = %d, want 1", len(agent.writeCalls))
	}
	got := agent.writeCalls[0]
	if got.APIKey != "sk-new" || got.BaseURL != "https://relay.example.com" || got.Model != "claude-sonnet-4" {
		t.Errorf("forwarded cfg = %+v", got)
	}
	// Response echoes back the canonical on-disk state (read-after-write).
	var resp LiveProviderResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.APIKey != "sk-new" {
		t.Errorf("response apiKey = %q", resp.APIKey)
	}
}

func TestHandleWriteLiveProvider_InvalidJSON(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodPut, "/config/agents/claudecode/provider", strings.NewReader("{bad"))
	req.SetPathValue("agent", "claudecode")
	w := httptest.NewRecorder()
	h.HandleWriteLiveProvider(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleWriteLiveProvider_AgentError(t *testing.T) {
	agent := newAdminStubAgent("codex")
	agent.liveWriteErr = errors.New("disk full")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	body := `{"apiKey":"sk-x"}`
	req := httptest.NewRequest(http.MethodPut, "/config/agents/codex/provider", strings.NewReader(body))
	req.SetPathValue("agent", "codex")
	w := httptest.NewRecorder()
	h.HandleWriteLiveProvider(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

func TestHandleWriteLiveProvider_NotImplemented(t *testing.T) {
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}
	req := httptest.NewRequest(http.MethodPut, "/config/agents/bare/provider", strings.NewReader(`{}`))
	req.SetPathValue("agent", "bare")
	w := httptest.NewRecorder()
	h.HandleWriteLiveProvider(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestToLiveConfig_RoundTrip(t *testing.T) {
	original := LiveProviderRequest{
		APIKey:  "k",
		BaseURL: "u",
		Model:   "m",
		Env:     map[string]string{"FOO": "bar"},
	}
	core_ := toLiveConfig(original)
	back := fromLiveConfig("x", core_)
	if back.Agent != "x" || back.APIKey != "k" || back.BaseURL != "u" || back.Model != "m" || back.Env["FOO"] != "bar" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

// -----------------------------------------------------------------------------
// GET /config/agents/{agent}/mcp
// PUT   /config/agents/{agent}/mcp/{name}
// DELETE /config/agents/{agent}/mcp/{name}
// -----------------------------------------------------------------------------

func TestHandleListMcpServers_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	agent.mcpServers = map[string]core.McpServerConfig{
		"fs": {Command: "npx", Args: []string{"-y", "server"}},
	}
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodGet, "/config/agents/claudecode/mcp", nil)
	req.SetPathValue("agent", "claudecode")
	w := httptest.NewRecorder()
	h.HandleListMcpServers(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var resp McpListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Servers) != 1 {
		t.Fatalf("servers len = %d, want 1", len(resp.Servers))
	}
	if resp.Servers["fs"].Command != "npx" {
		t.Errorf("fs.command = %q", resp.Servers["fs"].Command)
	}
}

func TestHandleListMcpServers_NotImplemented(t *testing.T) {
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}
	req := httptest.NewRequest(http.MethodGet, "/config/agents/bare/mcp", nil)
	req.SetPathValue("agent", "bare")
	w := httptest.NewRecorder()
	h.HandleListMcpServers(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestHandleListMcpServers_UnknownAgent(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodGet, "/config/agents/ghost/mcp", nil)
	req.SetPathValue("agent", "ghost")
	w := httptest.NewRecorder()
	h.HandleListMcpServers(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleSaveMcpServer_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	body := `{"command":"npx","args":["-y","server"],"env":{"ROOT":"/tmp"}}`
	req := httptest.NewRequest(http.MethodPut, "/config/agents/claudecode/mcp/fs", strings.NewReader(body))
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("name", "fs")
	w := httptest.NewRecorder()
	h.HandleSaveMcpServer(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if len(agent.mcpSaveCalls) != 1 || agent.mcpSaveCalls[0].name != "fs" {
		t.Errorf("saveCalls = %+v", agent.mcpSaveCalls)
	}
	saved := agent.mcpSaveCalls[0].cfg
	if saved.Command != "npx" || len(saved.Args) != 2 || saved.Env["ROOT"] != "/tmp" {
		t.Errorf("forwarded cfg = %+v", saved)
	}
	// Response echoes back.
	var resp McpServerResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Name != "fs" || resp.Command != "npx" {
		t.Errorf("response = %+v", resp)
	}
}

func TestHandleSaveMcpServer_InvalidJSON(t *testing.T) {
	h := &AdminHandlers{Store: setupAdminStore(newAdminStubAgent("claudecode"))}
	req := httptest.NewRequest(http.MethodPut, "/config/agents/claudecode/mcp/fs", strings.NewReader("{bad"))
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("name", "fs")
	w := httptest.NewRecorder()
	h.HandleSaveMcpServer(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleSaveMcpServer_ValidationError(t *testing.T) {
	// stdio without command → agent returns validation error → handler maps to 400.
	agent := newAdminStubAgent("claudecode")
	agent.mcpSaveErr = errors.New("stdio mcp server requires a command")
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodPut, "/config/agents/claudecode/mcp/bad", strings.NewReader(`{}`))
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("name", "bad")
	w := httptest.NewRecorder()
	h.HandleSaveMcpServer(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for validation error, got %d", w.Code)
	}
}

func TestHandleDeleteMcpServer_OK(t *testing.T) {
	agent := newAdminStubAgent("claudecode")
	agent.mcpServers = map[string]core.McpServerConfig{"fs": {Command: "npx"}}
	h := &AdminHandlers{Store: setupAdminStore(agent)}

	req := httptest.NewRequest(http.MethodDelete, "/config/agents/claudecode/mcp/fs", nil)
	req.SetPathValue("agent", "claudecode")
	req.SetPathValue("name", "fs")
	w := httptest.NewRecorder()
	h.HandleDeleteMcpServer(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(agent.mcpDeleteCalls) != 1 || agent.mcpDeleteCalls[0] != "fs" {
		t.Errorf("deleteCalls = %v", agent.mcpDeleteCalls)
	}
	if _, stillThere := agent.mcpServers["fs"]; stillThere {
		t.Error("fs was not deleted from stub state")
	}
}

func TestHandleDeleteMcpServer_NotImplemented(t *testing.T) {
	bare := &adminStubAgentBare{name: "bare"}
	h := &AdminHandlers{Store: setupAdminStore(bare)}
	req := httptest.NewRequest(http.MethodDelete, "/config/agents/bare/mcp/x", nil)
	req.SetPathValue("agent", "bare")
	req.SetPathValue("name", "x")
	w := httptest.NewRecorder()
	h.HandleDeleteMcpServer(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", w.Code)
	}
}

func TestIsValidationError(t *testing.T) {
	if !isValidationError(errors.New("stdio mcp server requires a command")) {
		t.Error("expected true for 'requires'")
	}
	if !isValidationError(errors.New("mcp server name is required")) {
		t.Error("expected true for 'is required'")
	}
	if isValidationError(errors.New("disk full")) {
		t.Error("expected false for unrelated error")
	}
}
