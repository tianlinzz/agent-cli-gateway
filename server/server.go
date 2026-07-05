package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// Server is the agent-cli-gateway HTTP gateway server.
type Server struct {
	cfg    config.GatewayConfig
	store  *SessionStore
	server *http.Server
}

// NewServer creates a new gateway HTTP server.
func NewServer(cfg config.GatewayConfig, store *SessionStore) *Server {
	s := &Server{
		cfg:   cfg,
		store: store,
	}

	mux := http.NewServeMux()
	handlers := &Handlers{Store: store}
	admin := &AdminHandlers{Store: store}

	// Register routes using Go 1.22+ pattern routing.
	identityMw := withUserIdentity(cfg.UserIDHeader, cfg.IdentityMode)
	mux.Handle("GET /health", http.HandlerFunc(handlers.HandleHealth))
	mux.Handle("POST /session",
		chain(http.HandlerFunc(handlers.HandleCreateSession), s.withAuth, identityMw))
	mux.Handle("POST /session/{id}/prompt_async",
		chain(http.HandlerFunc(handlers.HandlePromptAsync), s.withAuth, identityMw))
	mux.Handle("GET /event",
		chain(http.HandlerFunc(handlers.HandleEventStream), s.withAuth, identityMw))
	mux.Handle("POST /session/{id}/abort",
		chain(http.HandlerFunc(handlers.HandleAbort), s.withAuth, identityMw))
	mux.Handle("GET /config/providers",
		chain(http.HandlerFunc(handlers.HandleConfigProviders), s.withAuth, identityMw))

	// Management endpoints (PR1: session browsing/history/delete/resume).
	// Same auth + identity chain as live-session routes.
	mux.Handle("GET /sessions",
		chain(http.HandlerFunc(admin.HandleListSessions), s.withAuth, identityMw))
	mux.Handle("GET /sessions/{agent}/{id}/history",
		chain(http.HandlerFunc(admin.HandleSessionHistory), s.withAuth, identityMw))
	mux.Handle("DELETE /sessions/{agent}/{id}",
		chain(http.HandlerFunc(admin.HandleDeleteSession), s.withAuth, identityMw))
	mux.Handle("GET /sessions/{agent}/{id}/resume",
		chain(http.HandlerFunc(admin.HandleSessionResume), s.withAuth, identityMw))

	// Management endpoints (PR2: provider live config).
	mux.Handle("GET /config/agents/{agent}/provider",
		chain(http.HandlerFunc(admin.HandleReadLiveProvider), s.withAuth, identityMw))
	mux.Handle("PUT /config/agents/{agent}/provider",
		chain(http.HandlerFunc(admin.HandleWriteLiveProvider), s.withAuth, identityMw))

	// Management endpoints (PR3: MCP servers).
	mux.Handle("GET /config/agents/{agent}/mcp",
		chain(http.HandlerFunc(admin.HandleListMcpServers), s.withAuth, identityMw))
	mux.Handle("PUT /config/agents/{agent}/mcp/{name}",
		chain(http.HandlerFunc(admin.HandleSaveMcpServer), s.withAuth, identityMw))
	mux.Handle("DELETE /config/agents/{agent}/mcp/{name}",
		chain(http.HandlerFunc(admin.HandleDeleteMcpServer), s.withAuth, identityMw))

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: s.withCORS(mux),
	}

	return s
}

// withAuth wraps a handler with Bearer token authentication.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Token == "" {
			// No token configured — skip auth (development mode).
			next.ServeHTTP(w, r)
			return
		}
		token := extractToken(r)
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// chain composes middlewares right-to-left: chain(h, a, b) executes a(b(h)).
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// withCORS wraps the mux with CORS headers.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-User-Id")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isAllowedOrigin(origin string) bool {
	if len(s.cfg.CORSOrigins) == 0 {
		return true // allow all if not configured
	}
	for _, allowed := range s.cfg.CORSOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}

// extractToken pulls the Bearer token from Authorization header or X-API-Key.
func extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	return r.Header.Get("X-API-Key")
}

// Start begins listening for HTTP requests.
func (s *Server) Start() error {
	slog.Info("agent-cli-gateway starting", "port", s.cfg.Port, "agents", s.store.ListAgents())
	return s.server.ListenAndServe()
}

// Shutdown gracefully stops the server. The session store is closed first so
// every live agent subprocess is terminated (CloseAll) before the HTTP server
// stops accepting connections — fixing the orphaned-child-process bug on
// gateway shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.store != nil {
		s.store.CloseAll()
	}
	return s.server.Shutdown(ctx)
}
