package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2"
	"github.com/tianlinzz/agent-cli-gateway/core"
)

// ManagedSession holds a live agent session and its metadata.
type ManagedSession struct {
	ID           string
	Agent        core.Agent
	Session      core.AgentSession
	AgentName    string
	Model        string
	CreatedAt    time.Time
	OwnerID      string    // caller identity from request context
	LastActivity time.Time // updated on each successful Send
	InFlight     bool      // true while a turn is running (TTL reaper exempts)
}

// SessionStore manages live gateway sessions.
type SessionStore struct {
	mu         sync.RWMutex
	sessions   map[string]*ManagedSession
	agents     map[string]core.Agent  // agent name → singleton agent instance
	agentLocks map[string]*sync.Mutex // per-agent lock serializing config mutation + session start
	// perOwner LRU caches: ownerID → LRU of session IDs (capacity = maxPerUser).
	// nil entries when maxPerUser == 0 (LRU disabled).
	perOwner   map[string]*lru.Cache[string, *ManagedSession]
	maxPerUser int

	idleTTL time.Duration // used by Task 1.5; store now but reaper added later

	evictCh chan *ManagedSession // eviction worker inbox (async Close)
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewSessionStore creates a session store with pre-built agent singletons.
// LRU eviction and idle reaping are disabled (Task 1.5 wires the WithLimits
// version into main.go).
func NewSessionStore(agents map[string]core.Agent) *SessionStore {
	return NewSessionStoreWithLimits(agents, 0, 0)
}

// NewSessionStoreWithLimits builds a store with per-caller LRU (maxPerUser>0)
// and idle TTL reaper (idleTTL>0, reaper itself added in Task 1.5). Either 0
// disables that mechanism.
func NewSessionStoreWithLimits(agents map[string]core.Agent, maxPerUser int, idleTTL time.Duration) *SessionStore {
	agentLocks := make(map[string]*sync.Mutex, len(agents))
	for name := range agents {
		agentLocks[name] = &sync.Mutex{}
	}
	s := &SessionStore{
		sessions:   make(map[string]*ManagedSession),
		agents:     agents,
		agentLocks: agentLocks,
		maxPerUser: maxPerUser,
		idleTTL:    idleTTL,
		perOwner:   make(map[string]*lru.Cache[string, *ManagedSession]),
		evictCh:    make(chan *ManagedSession, 256),
		stopCh:     make(chan struct{}),
	}
	// The eviction worker drains evictCh (used by both LRU eviction and the
	// idle reaper), so it must run whenever EITHER mechanism is active.
	if s.maxPerUser > 0 || s.idleTTL > 0 {
		s.startEvictionWorker()
	}
	if s.idleTTL > 0 {
		s.startReaper()
	}
	return s
}

// LiveCount returns the number of sessions currently in the store.
func (s *SessionStore) LiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// startEvictionWorker drains evictCh and closes sessions asynchronously. The
// slow subprocess Close() must NOT run inside the LRU eviction callback (which
// fires synchronously under s.mu during cache.Add) — that would risk blocking
// the lock holder and re-entering LRU internals. Instead the callback only
// removes the entry from s.sessions synchronously and enqueues the session
// here for async Close.
func (s *SessionStore) startEvictionWorker() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case ms := <-s.evictCh:
				if err := ms.Session.Close(); err != nil {
					slog.Warn("eviction worker: close failed", "id", ms.ID, "error", err)
				}
			case <-s.stopCh:
				return
			}
		}
	}()
}

// evict enqueues a session for async Close(). The sessions-map removal happens
// SYNCHRONOUSLY in the LRU callback (not here) so HasSession stays consistent.
func (s *SessionStore) evict(ms *ManagedSession) {
	select {
	case s.evictCh <- ms:
	default:
		slog.Warn("evict inbox full, deferring close", "id", ms.ID)
	}
}

// startReaper launches a background goroutine that periodically evicts sessions
// idle longer than idleTTL. In-flight turns (InFlight == true) are exempt so a
// long-running prompt is never killed mid-turn. The interval is idleTTL/4,
// clamped to [50ms, 1min], giving at most ~25% extra idle time before eviction.
func (s *SessionStore) startReaper() {
	// Interval = idleTTL/4, clamped to [50ms, 1min].
	interval := s.idleTTL / 4
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.reapIdle()
			case <-s.stopCh:
				return
			}
		}
	}()
}

// reapIdle scans the live sessions and evicts any that have been idle longer
// than idleTTL, skipping in-flight turns (within a grace window). Collection is
// done under RLock; map mutation and the (async) Close enqueue happen under a
// separate write lock so we never hold the lock across the slow evict path.
//
// In-flight exemption: a session with InFlight=true is exempt while
// now-LastActivity <= 2*idleTTL (protecting a legitimately long-running turn).
// Beyond 2*idleTTL the exemption lapses — this bounds the leak from
// fire-and-forget prompts (caller POSTs prompt_async then never opens /event,
// so the terminal-event clear path in streamSessionEvents never runs and
// InFlight is never reset). Without this fallback such sessions would be
// permanently exempt from the TTL reaper. A turn idle longer than 2×TTL is
// assumed stuck/dead.
func (s *SessionStore) reapIdle() {
	s.mu.RLock()
	now := time.Now()
	var toEvict []*ManagedSession
	for _, ms := range s.sessions {
		age := now.Sub(ms.LastActivity)
		if ms.InFlight && age <= 2*s.idleTTL {
			continue // in-flight and within 2×TTL grace → exempt
		}
		if age > s.idleTTL {
			toEvict = append(toEvict, ms)
		}
	}
	s.mu.RUnlock()
	for _, ms := range toEvict {
		slog.Info("reaper: evicting idle session", "id", ms.ID, "owner", ms.OwnerID, "idle", time.Since(ms.LastActivity))
		// Remove from map synchronously, defer Close to worker.
		s.mu.Lock()
		delete(s.sessions, ms.ID)
		// cache.Remove fires the eviction callback (which calls s.evict,
		// enqueuing the session for async Close) when the key is present.
		// To avoid double-enqueuing Close (and thus double-calling
		// Session.Close), only call s.evict ourselves when the session was
		// NOT in the LRU (present=false) — e.g. when maxPerUser==0 (LRU
		// disabled) or the entry was already absent from the cache.
		inLRU := false
		if cache, ok := s.perOwner[ms.OwnerID]; ok {
			inLRU = cache.Remove(ms.ID)
		}
		s.mu.Unlock()
		if !inLRU {
			s.evict(ms)
		}
	}
}

// CloseAll stops the reaper and eviction worker, then concurrently closes
// every live session. It is the graceful-shutdown path (called by
// Server.Shutdown) and is idempotent. Closing the store first guarantees the
// worker/reaper goroutines have exited before we Close sessions inline, so
// there is no double-close from the async worker.
func (s *SessionStore) CloseAll() {
	// Signal reaper + worker to stop. Idempotent: guard with a closed-flag
	// to avoid panicking on double-close of stopCh.
	s.mu.Lock()
	select {
	case <-s.stopCh:
		// already closed
		s.mu.Unlock()
		return
	default:
		close(s.stopCh)
	}
	sessions := make([]*ManagedSession, 0, len(s.sessions))
	for _, ms := range s.sessions {
		sessions = append(sessions, ms)
	}
	s.sessions = make(map[string]*ManagedSession)
	s.perOwner = make(map[string]*lru.Cache[string, *ManagedSession])
	s.mu.Unlock()

	// Wait for reaper + worker goroutines to observe stopCh and return.
	s.wg.Wait()

	// Now close all sessions concurrently. The worker has stopped, so Close
	// happens inline here (no double-close risk: worker only Closes sessions
	// enqueued via evict, which we no longer call).
	var closeWg sync.WaitGroup
	for _, ms := range sessions {
		closeWg.Add(1)
		go func(ms *ManagedSession) {
			defer closeWg.Done()
			if err := ms.Session.Close(); err != nil {
				slog.Warn("CloseAll: close failed", "id", ms.ID, "error", err)
			}
		}(ms)
	}
	closeWg.Wait()
}

// SetInFlight flips the in-flight flag without touching LastActivity. Used by
// the SSE handler to mark a session as "turn active" while a client is
// streaming, so the idle reaper exempts it.
func (s *SessionStore) SetInFlight(id string, inFlight bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms, ok := s.sessions[id]; ok {
		ms.InFlight = inFlight
	}
}

// CreateSession starts a new agent session.
func (s *SessionStore) CreateSession(ctx context.Context, req CreateSessionRequest) (*ManagedSession, error) {
	s.mu.RLock()
	agent, ok := s.agents[req.Agent]
	agentLock := s.agentLocks[req.Agent]
	s.mu.RUnlock()
	if !ok || agentLock == nil {
		return nil, fmt.Errorf("unknown agent %q, available: %v", req.Agent, s.ListAgents())
	}

	// Serialize the applyAgentConfig → StartSession sequence per agent type.
	// applyAgentConfig mutates the shared agent singleton, so concurrent
	// CreateSession calls on the same agent would race: one session could
	// start with another's model/workdir/mode.
	agentLock.Lock()
	defer agentLock.Unlock()

	// Apply runtime config to the agent singleton before starting a session.
	applyAgentConfig(agent, req)

	ownerID := UserIDFromContext(ctx)

	// If the client provided a resume session ID (from a previous turn), pass it
	// to StartSession so the agent driver can resume that conversation (e.g.
	// claudecode adds --resume <id>). An empty string means start a fresh session.
	// If resume fails (e.g. session expired), retry once with a fresh session.
	agentSession, err := agent.StartSession(ctx, req.SessionID)
	if err != nil && req.SessionID != "" {
		slog.Warn("resume failed, starting fresh session", "oldSessionId", req.SessionID, "error", err)
		agentSession, err = agent.StartSession(ctx, "")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to start agent session: %w", err)
	}

	// The agent assigns its own real session ID asynchronously — for Claude
	// Code it only arrives in the system/init event AFTER the first prompt is
	// sent. So we can't get it here at StartSession time. Use the client-
	// provided sessionId (if resuming) or a generated gateway ID as the key.
	// The real agent session ID is carried in SSE events (sessionID field) so
	// the adapter can extract and cache it for future resume calls.
	managedID := req.SessionID
	if managedID == "" {
		managedID = generateSessionID()
	}

	// Guard against collision (astronomically unlikely with agent-assigned IDs).
	s.mu.Lock()
	for s.sessions[managedID] != nil {
		managedID = managedID + "_" + generateSessionID()[:8]
	}
	ms := &ManagedSession{
		ID:           managedID,
		Agent:        agent,
		Session:      agentSession,
		AgentName:    req.Agent,
		Model:        req.Model,
		CreatedAt:    time.Now(),
		OwnerID:      ownerID,
		LastActivity: time.Now(),
	}
	s.sessions[managedID] = ms

	// LRU: if maxPerUser > 0, add to the per-owner cache. If this exceeds the
	// cap, the eviction callback fires synchronously inside cache.Add — it
	// removes the evicted session from s.sessions (we hold s.mu here) and
	// enqueues it for async Close. We are still holding s.mu.Lock() while the
	// callback runs, so it must NOT re-acquire s.mu (deadlock) nor perform the
	// slow subprocess Close() inline (would block the lock holder).
	if s.maxPerUser > 0 {
		cache, ok := s.perOwner[ownerID]
		if !ok {
			newCache, err := lru.NewWithEvict[string, *ManagedSession](s.maxPerUser, func(key string, evicted *ManagedSession) {
				// Called synchronously inside cache.Add while we hold s.mu.Lock().
				delete(s.sessions, key)
				s.evict(evicted)
			})
			if err != nil {
				// Should not happen for a valid size; degrade gracefully by
				// leaving LRU disabled for this owner.
				slog.Error("failed to build per-owner LRU, eviction disabled", "owner", ownerID, "error", err)
			} else {
				cache = newCache
				s.perOwner[ownerID] = cache
			}
		}
		if cache != nil {
			cache.Add(managedID, ms)
		}
	}
	s.mu.Unlock()

	slog.Info("session created", "id", managedID, "agent", req.Agent, "model", req.Model)
	return ms, nil
}

// GetSession retrieves a live session by ID.
func (s *SessionStore) GetSession(id string) (*ManagedSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ms, ok := s.sessions[id]
	return ms, ok
}

// GetSessionForOwner returns the session only if it exists AND belongs to
// ownerID. Returns (nil, false) for both "not found" and "not owner" — the
// handler decides the HTTP status (404 vs 403) by calling HasSession.
//
// Per spec §7.3, a successful access also updates the per-owner LRU recency so
// that actively-used sessions are not evicted (LRU, not FIFO). The LRU's Get is
// internally synchronized, so calling it under s.mu.RLock is safe; Get never
// fires the eviction callback, so there's no lock-ordering concern.
func (s *SessionStore) GetSessionForOwner(id, ownerID string) (*ManagedSession, bool) {
	s.mu.RLock()
	ms, ok := s.sessions[id]
	if !ok || ms.OwnerID != ownerID {
		s.mu.RUnlock()
		return nil, false
	}
	s.mu.RUnlock()
	// Touch LRU recency on access. RLock is enough to call cache.Get (the LRU
	// library's Get is internally synchronized); cache != nil guards the
	// maxPerUser==0 case.
	if cache, ok := s.perOwner[ownerID]; ok && cache != nil {
		cache.Get(id) // updates recency; ignore the returned value (we have ms)
	}
	return ms, true
}

// HasSession reports whether a session with the given ID exists at all
// (regardless of owner). Used by handlers to choose 403 vs 404.
func (s *SessionStore) HasSession(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.sessions[id]
	return ok
}

// MarkActivity updates LastActivity to now and sets InFlight. Called after a
// successful Send. (Clearing InFlight happens in the SSE handler on stream end.)
func (s *SessionStore) MarkActivity(id string, inFlight bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms, ok := s.sessions[id]; ok {
		ms.LastActivity = time.Now()
		ms.InFlight = inFlight
	}
}

// AbortSession cancels the current turn (or closes the session if CancelTurn
// is not supported).
func (s *SessionStore) AbortSession(id string) bool {
	s.mu.RLock()
	ms, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if canceller, ok := ms.Session.(core.AgentSessionCanceller); ok {
		if err := canceller.CancelTurn(); err != nil {
			slog.Warn("failed to cancel turn", "session", id, "error", err)
		}
	} else {
		if err := ms.Session.Close(); err != nil {
			slog.Warn("failed to close session during abort", "session", id, "error", err)
		}
	}
	return true
}

// CloseSession terminates a session and removes it from the store.
func (s *SessionStore) CloseSession(id string) {
	s.mu.Lock()
	ms, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if ok {
		if err := ms.Session.Close(); err != nil {
			slog.Warn("failed to close session", "session", id, "error", err)
		}
	}
}

// ListAgents returns the names of all registered agents.
func (s *SessionStore) ListAgents() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.agents))
	for name := range s.agents {
		names = append(names, name)
	}
	return names
}

// Agent returns the singleton agent instance registered under name, plus an
// ok flag. Used by management handlers to reach agent capability interfaces
// (core.HistoryProvider, core.SessionDeleter, etc.) without going through a
// live ManagedSession.
func (s *SessionStore) Agent(name string) (core.Agent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.agents[name]
	return a, ok
}

// resolveWorkDir anchors the client-provided workDir under the gateway process's
// working directory (os.Getwd — /workspace in the container).
//
// f1-web only knows a relative name (e.g. "alice" or "alice/project"); it never
// needs to understand the container's absolute layout. Rules:
//   - empty workDir    → the gateway's own cwd (the agent runs alongside it)
//   - relative path    → joined under cwd, e.g. "alice/proj" → <cwd>/alice/proj
//   - path escapes cwd (e.g. "../etc") or is otherwise unsafe → treated as empty
//
// Absolute paths are intentionally NOT honored: f1-web has no business pointing
// the agent at arbitrary container paths.
func resolveWorkDir(workDir string) string {
	cwd, err := os.Getwd()
	if err != nil {
		slog.Warn("resolveWorkDir: cannot get cwd, using empty workDir", "error", err)
		return ""
	}
	if workDir == "" {
		return cwd
	}
	// Clean and join, then verify the result stays within cwd.
	joined := filepath.Join(cwd, workDir)
	rel, err := filepath.Rel(cwd, joined)
	if err != nil || rel == "." {
		// rel == "." means workDir resolved to cwd itself — fine.
		if err != nil {
			return ""
		}
	}
	if rel != "." && (rel == ".." || len(rel) >= 2 && rel[:2] == "..") {
		// Path escapes cwd — reject, treat as if not provided.
		slog.Warn("resolveWorkDir: workDir escapes cwd, ignoring", "workDir", workDir, "resolved", joined)
		return cwd
	}
	return joined
}

// applyAgentConfig sets runtime model/workdir/mode on the agent if supported.
func applyAgentConfig(agent core.Agent, req CreateSessionRequest) {
	if req.Model != "" {
		if sw, ok := agent.(core.ModelSwitcher); ok {
			sw.SetModel(req.Model)
		}
	}
	workDir := resolveWorkDir(req.WorkDir)
	if workDir != "" {
		// Create the directory on demand. Per-user workDirs (e.g.
		// /workspace/alice/project) don't exist until first use; without this
		// the agent subprocess fails on chdir. Errors are logged but non-fatal.
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			slog.Warn("failed to create workDir, continuing anyway", "workDir", workDir, "error", err)
		}
		if sw, ok := agent.(core.WorkDirSwitcher); ok {
			sw.SetWorkDir(workDir)
		}
	}
	if req.Mode != "" {
		if sw, ok := agent.(core.ModeSwitcher); ok {
			sw.SetMode(req.Mode)
		}
	}
}

// generateSessionID creates a unique session identifier using crypto/rand.
func generateSessionID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp if crypto/rand fails (extremely unlikely).
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("sess_%x", b)
}

// CollectProviders queries all registered agents for available models.
// Returns a map of providerID → model options.
func (s *SessionStore) CollectProviders(ctx context.Context) map[string][]core.ModelOption {
	result := make(map[string][]core.ModelOption)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, agent := range s.agents {
		if sw, ok := agent.(core.ModelSwitcher); ok {
			models := sw.AvailableModels(ctx)
			if len(models) > 0 {
				result[name] = models
			}
		}
	}
	return result
}
