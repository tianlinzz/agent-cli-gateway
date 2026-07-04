package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

// closingSession records how many times Close was called.
type closingSession struct {
	mu     sync.Mutex
	closes int
}

func (c *closingSession) Send(string, []core.ImageAttachment, []core.FileAttachment) error {
	return nil
}
func (c *closingSession) RespondPermission(string, core.PermissionResult) error { return nil }
func (c *closingSession) Events() <-chan core.Event                             { return nil }
func (c *closingSession) CurrentSessionID() string                              { return "" }
func (c *closingSession) Alive() bool                                           { return true }
func (c *closingSession) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}
func (c *closingSession) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// closingAgent returns a distinct closingSession per StartSession call.
type closingAgent struct{}

func (closingAgent) Name() string { return "closing" }
func (closingAgent) StartSession(ctx context.Context, id string) (core.AgentSession, error) {
	return &closingSession{}, nil
}
func (closingAgent) ListSessions(context.Context) ([]core.AgentSessionInfo, error) { return nil, nil }
func (closingAgent) Stop() error                                                   { return nil }

func TestCloseAll_KillsAllSessions(t *testing.T) {
	store := NewSessionStore(map[string]core.Agent{
		"a": closingAgent{},
		"b": closingAgent{},
	})
	ctx := withOwnerContext(context.Background(), "u")
	msA, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "a"})
	msB, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "b"})
	csA := msA.Session.(*closingSession)
	csB := msB.Session.(*closingSession)

	store.CloseAll()

	if csA.closeCount() == 0 || csB.closeCount() == 0 {
		t.Error("CloseAll must close every live session")
	}
	if store.LiveCount() != 0 {
		t.Error("CloseAll must empty the store")
	}
}

func TestReaper_EvictsIdleSession_ExemptsInFlight(t *testing.T) {
	// idleTTL very short so the reaper ticks quickly.
	store := NewSessionStoreWithLimits(map[string]core.Agent{"stub": &stubAgent{"stub"}}, 0, 50*time.Millisecond)
	defer store.CloseAll()
	ctx := withOwnerContext(context.Background(), "u")

	idle, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	running, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	store.MarkActivity(running.ID, true) // in-flight → exempt within 2×TTL grace

	// Poll for the idle session's eviction (replaces a fixed sleep — more
	// robust on a loaded runner, addressing final-review finding I6). Meanwhile
	// keep refreshing the running session's LastActivity so it stays within the
	// 2×TTL in-flight grace window — this models a genuinely active turn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && store.HasSession(idle.ID) {
		store.MarkActivity(running.ID, true) // keep the running turn fresh
		time.Sleep(10 * time.Millisecond)
	}

	// idle should have been evicted; running (in-flight, kept fresh) survives.
	if store.HasSession(idle.ID) {
		t.Error("idle session should have been reaped")
	}
	if !store.HasSession(running.ID) {
		t.Error("in-flight session should be exempt from reaper")
	}
}

// TestReaper_CloseCalledOnceForLRUSession is a regression test for final-review
// finding I1: reapIdle previously called cache.Remove (which fires the eviction
// callback → s.evict, enqueuing Close) AND then s.evict again, double-enqueuing
// the session for async Close and calling Session.Close() twice.
//
// With maxPerUser=1 the reaped session IS in the LRU, so cache.Remove fires the
// callback. The fix makes reapIdle skip its own s.evict in that case. The
// reaper's single enqueue → worker Close once. CloseAll then runs, but the
// reaped session was already deleted from the map before CloseAll's snapshot, so
// CloseAll does not double-close it. Expected: exactly one Close.
func TestReaper_CloseCalledOnceForLRUSession(t *testing.T) {
	store := NewSessionStoreWithLimits(map[string]core.Agent{"closing": closingAgent{}}, 1, 50*time.Millisecond)
	defer store.CloseAll()
	ctx := withOwnerContext(context.Background(), "u")
	ms, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "closing"})
	cs := ms.Session.(*closingSession)

	// Let the reaper run multiple cycles past idleTTL.
	time.Sleep(400 * time.Millisecond)

	// CloseAll stops the reaper/worker and closes survivors. The reaped session
	// was removed from the map before CloseAll snapshots, so it isn't in the
	// survivor set — CloseAll won't touch it. Thus closeCount reflects only the
	// single enqueue from the reaper path.
	store.CloseAll()

	if got := cs.closeCount(); got != 1 {
		t.Errorf("reaper should Close an LRU-tracked session exactly once; got %d", got)
	}
}

// TestLRU_AccessUpdatesRecency is a regression test for final-review finding
// I2: GetSessionForOwner previously never touched the per-owner LRU, so
// eviction was FIFO by creation order instead of LRU by access recency.
//
// maxPerUser=2. Create s1, s2. Access s1 (promote recency). Create s3 → the LRU
// must evict s2 (least-recently-used), not s1. Before the fix s1 was evicted
// (FIFO by creation).
func TestLRU_AccessUpdatesRecency(t *testing.T) {
	store := NewSessionStoreWithLimits(map[string]core.Agent{"stub": &stubAgent{"stub"}}, 2, 0)
	defer store.CloseAll()
	ctx := withOwnerContext(context.Background(), "alice")
	s1, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	s2, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	// Access s1 so it is more recently-used than s2.
	if _, ok := store.GetSessionForOwner(s1.ID, "alice"); !ok {
		t.Fatal("get s1 failed")
	}
	// Create s3 → should evict s2 (LRU), not s1.
	s3, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	if !store.HasSession(s1.ID) {
		t.Error("s1 should survive (was accessed recently)")
	}
	if store.HasSession(s2.ID) {
		t.Error("s2 should be evicted (least recently used)")
	}
	if !store.HasSession(s3.ID) {
		t.Error("s3 should be live (just created)")
	}
}

// TestReaper_EvictsStuckInFlightAfterGracePeriod is a regression test for
// final-review finding I3: a fire-and-forget prompt (prompt_async with no /event
// reader) sets InFlight=true and never clears it (the terminal-event clear path
// in streamSessionEvents never runs). Without the 2×TTL grace fallback the
// session would be permanently exempt from the TTL reaper.
//
// Here we mark a session in-flight and never clear it. The reaper must still
// reap it once LastActivity is older than 2×idleTTL.
func TestReaper_EvictsStuckInFlightAfterGracePeriod(t *testing.T) {
	store := NewSessionStoreWithLimits(map[string]core.Agent{"stub": &stubAgent{"stub"}}, 0, 50*time.Millisecond)
	defer store.CloseAll()
	ctx := withOwnerContext(context.Background(), "u")
	ms, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	store.MarkActivity(ms.ID, true) // mark in-flight, never cleared (fire-and-forget)

	// Wait well past 2×TTL (100ms) + several reaper intervals.
	time.Sleep(500 * time.Millisecond)
	if store.HasSession(ms.ID) {
		t.Error("stuck in-flight session past 2×TTL grace should be reaped")
	}
}
