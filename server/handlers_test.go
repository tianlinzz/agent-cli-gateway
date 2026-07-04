package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

func setupTestStore(t *testing.T) *SessionStore {
	t.Helper()
	agent := newMockAgent("claudecode")
	return NewSessionStore(map[string]core.Agent{"claudecode": agent})
}

func TestHandleHealth(t *testing.T) {
	store := setupTestStore(t)
	h := &Handlers{Store: store}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	h.HandleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp HealthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Error("expected OK=true")
	}
	if len(resp.Agents) == 0 {
		t.Error("expected at least one agent listed")
	}
}

func TestCreateSessionAndPromptAsync(t *testing.T) {
	store := setupTestStore(t)
	h := &Handlers{Store: store}

	// Create session.
	body := `{"agent":"claudecode"}`
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleCreateSession(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("create session: expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var sessResp CreateSessionResponse
	if err := json.NewDecoder(w.Body).Decode(&sessResp); err != nil {
		t.Fatal(err)
	}
	if sessResp.ID == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Send prompt_async.
	promptBody := `{"parts":[{"type":"text","text":"hello"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/session/"+sessResp.ID+"/prompt_async", strings.NewReader(promptBody))
	req2.SetPathValue("id", sessResp.ID)
	w2 := httptest.NewRecorder()
	h.HandlePromptAsync(w2, req2)

	if w2.Code != http.StatusNoContent {
		t.Fatalf("prompt_async: expected 204, got %d", w2.Code)
	}
}

func TestCreateSessionUnknownAgent(t *testing.T) {
	store := setupTestStore(t)
	h := &Handlers{Store: store}

	body := `{"agent":"nonexistent"}`
	req := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleCreateSession(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown agent, got %d", w.Code)
	}
}

func TestEventStream(t *testing.T) {
	store := setupTestStore(t)
	h := &Handlers{Store: store}

	// Create session + send prompt first.
	managed, err := store.CreateSession(context.Background(), CreateSessionRequest{Agent: "claudecode"})
	if err != nil {
		t.Fatal(err)
	}
	_ = managed.Session.Send("test prompt", nil, nil)

	// Connect to event stream.
	req := httptest.NewRequest(http.MethodGet, "/event?session="+managed.ID, nil)
	w := httptest.NewRecorder()
	h.HandleEventStream(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Fatal("expected SSE data lines in response")
	}
	if !strings.Contains(body, "echo: test prompt") {
		t.Fatalf("expected echo response in SSE body, got: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatal("expected [DONE] terminator in SSE body")
	}
}

func TestAbort(t *testing.T) {
	store := setupTestStore(t)
	h := &Handlers{Store: store}

	managed, err := store.CreateSession(context.Background(), CreateSessionRequest{Agent: "claudecode"})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/session/"+managed.ID+"/abort", nil)
	req.SetPathValue("id", managed.ID)
	w := httptest.NewRecorder()
	h.HandleAbort(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp AbortResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Error("expected OK=true")
	}
}

func TestAbortNotFound(t *testing.T) {
	store := setupTestStore(t)
	h := &Handlers{Store: store}

	req := httptest.NewRequest(http.MethodPost, "/session/nonexistent/abort", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	h.HandleAbort(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
