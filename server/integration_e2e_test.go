package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/config"
	"github.com/tianlinzz/agent-cli-gateway/core"
)

// TestE2E_IdentityAndOwnershipThroughRealRouter verifies the full HTTP
// middleware chain (withAuth → withUserIdentity → handler) end-to-end through
// the real http.ServeMux, replacing the manual "start gateway + curl" smoke
// test that can't run on Windows (cmd/gateway pulls in claudecode → runas).
func TestE2E_IdentityAndOwnershipThroughRealRouter(t *testing.T) {
	agents := map[string]core.Agent{"stub": &stubAgent{"stub"}}
	store := NewSessionStore(agents)
	cfg := config.GatewayConfig{
		Port:         0,
		Token:        "secret",
		UserIDHeader: "X-User-Id",
		IdentityMode: "strict",
	}
	srv := NewServer(cfg, store)
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	do := func(path, method, userID, body string) (int, string) {
		var reqBody io.Reader
		if body != "" {
			reqBody = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, reqBody)
		req.Header.Set("Authorization", "Bearer secret")
		if userID != "" {
			req.Header.Set("X-User-Id", userID)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1. Missing X-User-Id in strict mode → 401.
	if code, _ := do("/session", "POST", "", `{"agent":"stub"}`); code != http.StatusUnauthorized {
		t.Errorf("strict missing identity: got %d, want 401", code)
	}

	// 2. alice creates a session → 200.
	code, body := do("/session", "POST", "alice", `{"agent":"stub"}`)
	if code != http.StatusOK {
		t.Fatalf("alice create: got %d body=%s, want 200", code, body)
	}
	// Extract session id (naive substring; CreateSessionResponse.ID).
	aliceID := extractJSONField(body, "id")
	if aliceID == "" {
		t.Fatalf("no session id in response: %s", body)
	}

	// 3. bob tries to send prompt to alice's session → 403.
	code, _ = do("/session/"+aliceID+"/prompt_async", "POST", "bob", `{"parts":[{"type":"text","text":"hi"}]}`)
	if code != http.StatusForbidden {
		t.Errorf("bob cross-caller: got %d, want 403", code)
	}

	// 4. alice sends prompt to her own session → 204 (stub accepts).
	code, _ = do("/session/"+aliceID+"/prompt_async", "POST", "alice", `{"parts":[{"type":"text","text":"hi"}]}`)
	if code != http.StatusNoContent {
		t.Errorf("alice own session: got %d, want 204", code)
	}

	// 5. unknown session → 404.
	code, _ = do("/session/sess_nonexistent/prompt_async", "POST", "alice", `{"parts":[{"type":"text","text":"hi"}]}`)
	if code != http.StatusNotFound {
		t.Errorf("unknown session: got %d, want 404", code)
	}
}

func TestE2E_AnonymousMode_AllowsMissingIdentity(t *testing.T) {
	agents := map[string]core.Agent{"stub": &stubAgent{"stub"}}
	store := NewSessionStore(agents)
	cfg := config.GatewayConfig{
		Port:         0,
		Token:        "secret",
		UserIDHeader: "X-User-Id",
		IdentityMode: "anonymous",
	}
	srv := NewServer(cfg, store)
	ts := httptest.NewServer(srv.server.Handler)
	defer ts.Close()

	// Missing X-User-Id in anonymous mode → request proceeds (200, not 401).
	req, _ := http.NewRequest("POST", ts.URL+"/session", strings.NewReader(`{"agent":"stub"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous missing identity: got %d, want 200", resp.StatusCode)
	}
}

// extractJSONField pulls a string field value from a small JSON body without
// importing encoding/json (keeps the test readable for the specific shape we
// emit). Returns "" if not found.
func extractJSONField(body, field string) string {
	key := `"` + field + `":"`
	i := strings.Index(body, key)
	if i < 0 {
		return ""
	}
	rest := body[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// ensure context import is used (some toolchains warn on unused import when
// _test files evolve). Safe no-op.
var _ = context.Background
