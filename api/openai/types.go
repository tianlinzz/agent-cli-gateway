// Package openai implements the public OpenAI-compatible HTTP API of the
// agent gateway: model discovery (/v1/models), chat completions
// (/v1/chat/completions, streaming and non-streaming), per-session abort
// (/v1/sessions/{id}/abort) and health probes.
//
// The package is deliberately backend-agnostic: it never imports a concrete
// adapter or agent-CLI package, never parses a native agent protocol, and
// never launches a CLI. Everything is expressed through the canonical runtime
// contract — a runtime.Registry for model discovery, a runtime.SessionStore
// for session metadata/owner isolation, and a runtime.ExecutionBackend for
// execution. wire-up in cmd/gateway plugs in worker.LocalExecutionBackend,
// which forks one nsjail-wrapped worker per session.
package openai

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

const (
	nativeAbortTimeout = 30 * time.Second
	abortDrainTimeout  = nativeAbortTimeout + time.Second
	turnHandoffTimeout = nativeAbortTimeout + 5*time.Second
)

// Options configures the OpenAI API Handler. Every field has a usable default
// (see NewHandler); tests override what they need.
type Options struct {
	// Registry is the adapter registry used for model discovery. Defaults to
	// runtime.DefaultRegistry.
	Registry *runtime.Registry
	// Store is the owner-scoped session metadata store. Defaults to an
	// in-memory store.
	Store runtime.SessionStore
	// Backend is the API layer's ONLY execution path. Required.
	Backend runtime.ExecutionBackend

	// CallerTokens maps bearer tokens to trusted caller IDs. An empty map
	// disables authentication for tests only; production wiring must reject an
	// empty configured caller set before constructing the handler.
	CallerTokens map[string]string
	// SessionHeader is the request header carrying the gateway session id on
	// resume. Default "X-Gateway-Session-Id".
	SessionHeader string
	// WorkspaceHeader is the request header carrying the opaque workspace id.
	// Default "X-Workspace-Id".
	WorkspaceHeader string

	// Enabled reports whether a model/agent id is enabled by configuration.
	// When nil every registered adapter is enabled.
	Enabled func(name string) bool
	Models  map[string][]string

	// TurnTimeout bounds a single turn. Zero means no hard API-side bound
	// (the adapter/worker own their timeouts). NewHandler defaults this to a
	// generous safety net.
	TurnTimeout time.Duration
	// UsageGrace is how long the aggregator keeps draining events after an
	// EventFinish, because usage may arrive after the finish marker. Default
	// 250ms.
	UsageGrace time.Duration

	// Now returns the current time (injectable for deterministic tests).
	Now func() time.Time
}

// Handler serves the OpenAI-compatible HTTP routes. All state is guarded for
// concurrent use: handlers, sessions, and in-flight turns are accessed from
// multiple request goroutines.
type Handler struct {
	registry *runtime.Registry
	store    runtime.SessionStore
	backend  runtime.ExecutionBackend
	catalog  *modelCatalog

	callerTokens    map[string]string
	sessionHeader   string
	workspaceHeader string

	turnTimeout time.Duration
	usageGrace  time.Duration

	newID func() string
	now   func() time.Time

	mu                 sync.Mutex
	handles            map[string]runtime.ExecutionHandle // sessionID -> live execution
	turns              map[string]*turnState              // sessionID -> in-flight turn
	serverToolIDs      map[string]*serverToolLedger       // sessionID -> completed native tool IDs
	serverToolSessions []string                           // insertion order for bounded session eviction
}

// turnState is one in-flight turn on a session, registered so the abort
// endpoint can cancel it and wait for cleanup. cancel is wired by the turn
// after the request context is derived; the abort endpoint may call it before
// or after wiring, so it is mutex-guarded.
type turnState struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	settling atomic.Bool
}

func (ts *turnState) markSettling() { ts.settling.Store(true) }

func (h *Handler) markTurnSettling(sessionID string) {
	h.mu.Lock()
	ts := h.turns[sessionID]
	h.mu.Unlock()
	if ts != nil {
		ts.markSettling()
	}
}

const (
	maxServerToolIDsPerSession = 128
	maxServerToolSessions      = 1024
)

type serverToolLedger struct {
	ids   map[string]struct{}
	order []string
}

func (h *Handler) recordServerToolID(sessionID, toolID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(toolID) == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ledger := h.serverToolIDs[sessionID]
	if ledger == nil {
		if len(h.serverToolIDs) >= maxServerToolSessions {
			oldest := h.serverToolSessions[0]
			h.serverToolSessions = h.serverToolSessions[1:]
			delete(h.serverToolIDs, oldest)
		}
		ledger = &serverToolLedger{ids: make(map[string]struct{})}
		h.serverToolIDs[sessionID] = ledger
		h.serverToolSessions = append(h.serverToolSessions, sessionID)
	}
	if _, exists := ledger.ids[toolID]; exists {
		return
	}
	if len(ledger.order) >= maxServerToolIDsPerSession {
		oldest := ledger.order[0]
		ledger.order = ledger.order[1:]
		delete(ledger.ids, oldest)
	}
	ledger.ids[toolID] = struct{}{}
	ledger.order = append(ledger.order, toolID)
}

func (h *Handler) isServerToolID(sessionID, toolID string) bool {
	h.mu.Lock()
	ledger := h.serverToolIDs[sessionID]
	ok := false
	if ledger != nil {
		_, ok = ledger.ids[toolID]
	}
	h.mu.Unlock()
	return ok
}

func (h *Handler) clearServerToolIDs(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.serverToolIDs[sessionID]; !exists {
		return
	}
	delete(h.serverToolIDs, sessionID)
	for index, id := range h.serverToolSessions {
		if id == sessionID {
			h.serverToolSessions = append(h.serverToolSessions[:index], h.serverToolSessions[index+1:]...)
			return
		}
	}
}

func (h *Handler) hasServerToolReplay(sessionID string, messages []ChatMessage) bool {
	for _, message := range messages {
		if message.ToolCallID != "" && h.isServerToolID(sessionID, message.ToolCallID) {
			return true
		}
		for _, toolCall := range message.ToolCalls {
			if toolCall.ID != "" && h.isServerToolID(sessionID, toolCall.ID) {
				return true
			}
		}
	}
	return false
}

// awaitSettlingTurn waits only for a turn that is already being cancelled or
// timed out. A genuinely active concurrent turn remains an immediate 409.
func (h *Handler) awaitSettlingTurn(ctx context.Context, sessionID string) bool {
	h.mu.Lock()
	ts := h.turns[sessionID]
	h.mu.Unlock()
	if ts == nil {
		// EndTurn may have completed between BeginTurn's busy result and this
		// lookup. Allow one immediate retry to close that publication window.
		return true
	}
	if !ts.settling.Load() {
		return false
	}
	timer := time.NewTimer(turnHandoffTimeout)
	defer timer.Stop()
	select {
	case <-ts.done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (ts *turnState) setCancel(f context.CancelFunc) {
	ts.mu.Lock()
	ts.cancel = f
	ts.mu.Unlock()
}

func (ts *turnState) cancelTurn() {
	ts.mu.Lock()
	f := ts.cancel
	ts.mu.Unlock()
	if f != nil {
		f()
	}
}

// NewHandler builds a Handler, filling in defaults for any unset Options.
func NewHandler(opts Options) *Handler {
	if opts.Registry == nil {
		opts.Registry = runtime.DefaultRegistry()
	}
	if opts.Store == nil {
		opts.Store = runtime.NewMemorySessionStore()
	}
	if opts.SessionHeader == "" {
		opts.SessionHeader = "X-Gateway-Session-Id"
	}
	if opts.WorkspaceHeader == "" {
		opts.WorkspaceHeader = "X-Workspace-Id"
	}
	if opts.TurnTimeout <= 0 {
		opts.TurnTimeout = 10 * time.Minute
	}
	if opts.UsageGrace <= 0 {
		opts.UsageGrace = 250 * time.Millisecond
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	h := &Handler{
		registry:        opts.Registry,
		store:           opts.Store,
		backend:         opts.Backend,
		callerTokens:    cloneStringMap(opts.CallerTokens),
		sessionHeader:   opts.SessionHeader,
		workspaceHeader: opts.WorkspaceHeader,
		turnTimeout:     opts.TurnTimeout,
		usageGrace:      opts.UsageGrace,
		newID:           newRandomID,
		now:             now,
		handles:         make(map[string]runtime.ExecutionHandle),
		turns:           make(map[string]*turnState),
		serverToolIDs:   make(map[string]*serverToolLedger),
	}
	h.catalog = &modelCatalog{reg: opts.Registry, enabled: opts.Enabled, models: opts.Models}
	return h
}

// Routes returns the mounted HTTP handler (auth middleware applied; health
// probes are exempt from auth).
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", h.handleHealthLive)
	mux.HandleFunc("GET /health/ready", h.handleHealthReady)
	mux.HandleFunc("GET /v1/models", h.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", h.handleChatCompletions)
	mux.HandleFunc("POST /v1/sessions/{id}/abort", h.handleAbort)
	return h.authMiddleware(mux)
}

// authMiddleware enforces the bearer token on all /v1/* routes. Health probes
// are intentionally unauthenticated. When no token is configured, auth is
// disabled.
func (h *Handler) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/health/") {
			next.ServeHTTP(w, r)
			return
		}
		if len(h.callerTokens) == 0 {
			ctx := context.WithValue(r.Context(), callerContextKey{}, Caller{ID: "anonymous"})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authz, prefix)
		callerID, ok := h.authenticateCaller(token)
		if token == authz || !ok {
			writeError(w, http.StatusUnauthorized, authError("invalid or missing bearer token"))
			return
		}
		ctx := context.WithValue(r.Context(), callerContextKey{}, Caller{ID: callerID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (h *Handler) authenticateCaller(token string) (string, bool) {
	var callerID string
	found := 0
	for candidate, caller := range h.callerTokens {
		match := subtle.ConstantTimeCompare([]byte(candidate), []byte(token))
		if match == 1 {
			callerID = caller
		}
		found |= match
	}
	return callerID, found == 1
}

type callerContextKey struct{}

// Caller is the authenticated service caller. It is derived from the bearer
// token and cannot be overridden by request headers.
type Caller struct{ ID string }

func callerFromRequest(r *http.Request) (Caller, bool) {
	caller, ok := r.Context().Value(callerContextKey{}).(Caller)
	return caller, ok && strings.TrimSpace(caller.ID) != ""
}

// handleHealthLive is a liveness probe: the process is up.
func (h *Handler) handleHealthLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readinessProvider is the optional interface an execution backend implements
// to report sandbox/preflight readiness. LocalExecutionBackend implements it.
type readinessProvider interface {
	Preflight(ctx context.Context) error
}

// handleHealthReady is a readiness probe: the process is up AND the execution
// backend is ready (nsjail preflight passed). A missing backend or a failed
// preflight yields 503 — the service fails closed rather than serving requests
// that would run unsandboxed.
func (h *Handler) handleHealthReady(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": "no execution backend"})
		return
	}
	if rp, ok := h.backend.(readinessProvider); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := rp.Preflight(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// registerHandle stores a live execution handle for a session, returning any
// pre-existing handle if one is already registered (single execution per
// session).
func (h *Handler) registerHandle(sessionID string, handle runtime.ExecutionHandle) runtime.ExecutionHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	if existing, ok := h.handles[sessionID]; ok {
		return existing
	}
	h.handles[sessionID] = handle
	return handle
}

func (h *Handler) getHandle(sessionID string) runtime.ExecutionHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.handles[sessionID]
}

// dropHandle removes handle from the map only if it is still the registered
// one (so a newer execution is never clobbered by a stale teardown).
func (h *Handler) dropHandle(sessionID string, handle runtime.ExecutionHandle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, ok := h.handles[sessionID]; ok && current == handle {
		delete(h.handles, sessionID)
	}
}

// registerTurn marks a turn as in flight for abort coordination. Each turn
// gets its own turnState; the caller wires the cancel func via setCancel.
func (h *Handler) registerTurn(sessionID string) *turnState {
	ts := &turnState{done: make(chan struct{})}
	h.mu.Lock()
	if prev, ok := h.turns[sessionID]; ok {
		prev.cancelTurn() // defensive: never leave a stale turn canceling nothing
	}
	h.turns[sessionID] = ts
	h.mu.Unlock()
	return ts
}

// clearTurn ends a turn, running EndTurn on the store and closing ts.done so
// an abort endpoint waiting on the turn unblocks.
func (h *Handler) clearTurn(sessionID, callerID string, ts *turnState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := h.store.EndTurn(ctx, sessionID, callerID); err != nil {
		slog.Debug("openai: end turn", "session", sessionID, "error", err)
	}
	cancel()

	h.mu.Lock()
	if cur, ok := h.turns[sessionID]; ok && cur == ts {
		delete(h.turns, sessionID)
	}
	h.mu.Unlock()
	close(ts.done)
}

// abortHandle best-effort aborts an execution with its own bounded context so
// a canceled request context can never wedge the abort RPC.
func (h *Handler) abortHandle(ctx context.Context, handle runtime.ExecutionHandle) error {
	if handle == nil {
		return nil
	}
	actx, cancel := context.WithTimeout(ctx, nativeAbortTimeout)
	defer cancel()
	if err := handle.Abort(actx); err != nil {
		slog.Warn("openai: abort execution", "error", err)
		return err
	}
	return nil
}

// abortAndDrain cancels one turn and consumes its terminal events before the
// session is made available to another request. Persistent executions share a
// single event stream across turns; leaving a cancelled finish event buffered
// would make the next turn terminate immediately with an empty response.
func (h *Handler) abortAndDrain(ctx context.Context, sessionID string, handle runtime.ExecutionHandle) {
	if handle == nil {
		return
	}
	abortResult := make(chan error, 1)
	go func() { abortResult <- h.abortHandle(ctx, handle) }()
	abortDone := (<-chan error)(abortResult)
	events := handle.Events()
	settle := time.NewTimer(abortDrainTimeout)
	defer settle.Stop()
	var grace <-chan time.Time
	terminalDrained := false
	var abortErr error
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				terminalDrained = true
				if abortDone == nil {
					goto complete
				}
				continue
			}
			if ev.Type == runtime.EventFinish || ev.Type == runtime.EventError {
				grace = time.After(h.usageGrace)
			}
		case err := <-abortDone:
			abortErr = err
			abortDone = nil
			if terminalDrained {
				goto complete
			}
		case <-grace:
			grace = nil
			terminalDrained = true
			if abortDone == nil {
				goto complete
			}
		case <-settle.C:
			if abortErr == nil {
				abortErr = fmt.Errorf("abort succeeded but terminal events did not drain")
			}
			goto complete
		}
	}

complete:
	if abortErr == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := handle.Close(cctx); err != nil {
		slog.Warn("openai: close failed execution after abort", "session", sessionID, "error", err)
	}
	h.dropHandle(sessionID, handle)
}

// newRandomID returns a random 24-hex-char id (used for chat completion ids).
func newRandomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is effectively fatal; fall back to time entropy.
		// Callers prepend their own prefix ("chatcmpl-", "sess-"), so this must
		// return a bare value to avoid a double prefix.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// newSessionID returns a gateway session identifier.
func newSessionID() string {
	return "sess-" + newRandomID()
}

// constantTimeEqual compares two strings in constant time (hashing first so
// length differences do not leak).
func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// ---------------------------------------------------------------------------
// OpenAI wire types. Field names and shapes match the OpenAI Chat Completions
// API exactly.
// ---------------------------------------------------------------------------

// ChatCompletionRequest is the public request shape for POST /v1/chat/completions.
// Only the fields the gateway understands are declared; unknown fields are
// ignored by encoding/json. There is deliberately NO workdir/path field:
// clients reference workspaces only by opaque workspace_id.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ChatTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream,omitempty"`
	// StreamOptions toggles the usage chunk on streaming responses.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	// Metadata carries opaque gateway metadata (session_id, workspace_id, ...).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// StreamOptions mirrors OpenAI's stream_options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage is one conversation message.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // string, or an array of text content parts
	Name       string         `json:"name,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ChatTool is a tool definition (OpenAI function form).
type ChatTool struct {
	Type     string       `json:"type,omitempty"` // "function"
	Function ChatFunction `json:"function"`
}

// ChatFunction is the function definition inside a ChatTool.
type ChatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatToolCall is a tool call on an assistant message.
type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type,omitempty"`
	Function ChatFunctionCall `json:"function"`
}

// ChatFunctionCall references a function invocation; Arguments is a JSON
// string per the OpenAI shape.
type ChatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// usageInfo is the OpenAI usage object.
type usageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func usageFromRuntime(u *runtime.Usage) *usageInfo {
	if u == nil {
		return nil
	}
	return &usageInfo{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: u.TotalTokens}
}
