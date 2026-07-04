package server

import (
	"context"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/core"
)

func TestLRU_EvictsOldestWhenPerUserLimitReached(t *testing.T) {
	// maxPerUser=2: creating a 3rd session for alice evicts her oldest.
	store := NewSessionStoreWithLimits(map[string]core.Agent{"stub": &stubAgent{"stub"}}, 2, 0)
	ctx := withOwnerContext(context.Background(), "alice")

	s1, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	s2, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	s3, _ := store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})

	// s1 should be evicted; s2, s3 remain.
	if store.HasSession(s1.ID) {
		t.Error("s1 should have been evicted by LRU")
	}
	if !store.HasSession(s2.ID) || !store.HasSession(s3.ID) {
		t.Error("s2 and s3 should still be live")
	}
}

func TestLRU_PerUserIsolation(t *testing.T) {
	// maxPerUser=1: alice and bob each get 1, no cross-eviction.
	store := NewSessionStoreWithLimits(map[string]core.Agent{"stub": &stubAgent{"stub"}}, 1, 0)

	a1, _ := store.CreateSession(withOwnerContext(context.Background(), "alice"), CreateSessionRequest{Agent: "stub"})
	b1, _ := store.CreateSession(withOwnerContext(context.Background(), "bob"), CreateSessionRequest{Agent: "stub"})

	if !store.HasSession(a1.ID) || !store.HasSession(b1.ID) {
		t.Error("per-user LRU should not evict across callers")
	}
}

func TestLRU_DisabledWhenMaxZero(t *testing.T) {
	store := NewSessionStoreWithLimits(map[string]core.Agent{"stub": &stubAgent{"stub"}}, 0, 0)
	ctx := withOwnerContext(context.Background(), "alice")

	for i := 0; i < 10; i++ {
		store.CreateSession(ctx, CreateSessionRequest{Agent: "stub"})
	}
	if got := store.LiveCount(); got != 10 {
		t.Errorf("maxPerUser=0: live count = %d, want 10", got)
	}
}
