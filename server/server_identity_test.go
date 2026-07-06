package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// newIdentityTestServer builds a Server with token auth enabled but no real
// agents — we only assert middleware-level routing (401 from identity vs 401
// from auth), which happens before any handler touches the store.
func newIdentityTestServer(t *testing.T) *Server {
	t.Helper()
	store := NewSessionStore(nil)
	return NewServer(config.GatewayConfig{Port: 0, Token: "secret", IdentityMode: "strict"}, store)
}

// Admin/management routes operate on agent singletons for ops/inspection and
// never read UserIDFromContext, so they must NOT require X-User-Id. A request
// with a valid token but no identity header must pass the middleware chain
// (here it returns 4xx from the handler, e.g. 400/404, NOT 401 from identity).
func TestAdminRoutes_DoNotRequireIdentity(t *testing.T) {
	srv := newIdentityTestServer(t)
	// 200 from /health proves the mux wired the handler (health needs no auth).
	rec := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health sanity check: got %d, want 200", rec.Code)
	}

	cases := []struct {
		name   string
		method string
		target string
	}{
		{"list sessions", http.MethodGet, "/sessions?agent=claudecode"},
		{"list workspaces", http.MethodGet, "/workspaces?agent=claudecode"},
		{"session history", http.MethodGet, "/sessions/claudecode/sess-1/history"},
		{"read provider", http.MethodGet, "/config/agents/claudecode/provider"},
		{"list mcp", http.MethodGet, "/config/agents/claudecode/mcp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(c.method, c.target, nil)
			// Valid token, but deliberately NO X-User-Id header.
			req.Header.Set("Authorization", "Bearer secret")
			srv.server.Handler.ServeHTTP(rec, req)
			// Identity middleware would return 401 with body containing
			// "missing identity header". Any other code means we got past it.
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("%s: got 401, want non-401 (identity should not gate admin routes). body=%q", c.target, rec.Body.String())
			}
		})
	}
}

// Live-session routes carry per-caller session ownership (OwnerID, per-owner
// LRU, GetSessionForOwner), so they MUST still require X-User-Id. This guards
// against accidentally loosening the conversation chain when refactoring admin
// routes.
func TestLiveSessionRoutes_StillRequireIdentity(t *testing.T) {
	srv := newIdentityTestServer(t)
	cases := []struct {
		method string
		target string
	}{
		{http.MethodPost, "/session"},
		{http.MethodPost, "/session/sess-1/prompt_async"},
		{http.MethodGet, "/event?session=sess-1"},
		{http.MethodPost, "/session/sess-1/abort"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.target, nil)
		req.Header.Set("Authorization", "Bearer secret") // valid token, no identity
		srv.server.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without X-User-Id: got %d, want 401", c.method, c.target, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "missing identity") {
			t.Errorf("%s %s: 401 body = %q, want it to mention \"missing identity\"", c.method, c.target, body)
		}
	}
}
