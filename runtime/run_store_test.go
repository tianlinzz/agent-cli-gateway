package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunStore_CreateAndGet(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	rec := RunRecord{
		ID:          "run-1",
		SessionID:   "sess-1",
		CallerID:    "caller-a",
		WorkspaceID: "ws-1",
		ModelID:     "codex",
		Status:      RunStarting,
	}
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "run-1" || got.SessionID != "sess-1" || got.Status != RunStarting {
		t.Fatalf("Get returned %+v", got)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("StartedAt should be stamped to now when zero")
	}
}

func TestRunStore_CreateDuplicate(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	rec := RunRecord{ID: "run-1", SessionID: "s", CallerID: "c"}
	if err := store.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	err := store.Create(ctx, rec)
	if !errors.Is(err, ErrRunExists) {
		t.Fatalf("expected ErrRunExists, got %v", err)
	}
}

func TestRunStore_CreateInvalid(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	if err := store.Create(ctx, RunRecord{}); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("empty ID should be ErrInvalidRun, got %v", err)
	}
	if err := store.Create(ctx, RunRecord{ID: "r", Status: "bogus"}); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("bad status should be ErrInvalidRun, got %v", err)
	}
}

func TestRunStore_Update(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	rec := RunRecord{ID: "run-1", SessionID: "s", CallerID: "c", Status: RunStarting}
	if err := store.Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update(ctx, "run-1", func(r *RunRecord) {
		r.Status = RunSucceeded
		r.FinishedAt = time.Now()
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != RunSucceeded {
		t.Fatalf("status = %q, want succeeded", updated.Status)
	}
	if updated.StartedAt.IsZero() {
		t.Fatal("StartedAt should be preserved")
	}
}

func TestRunStore_UpdateNotFound(t *testing.T) {
	store := NewMemoryRunStore()
	_, err := store.Update(context.Background(), "nope", func(r *RunRecord) {})
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
}

func TestRunStore_GetReturnsDeepCopy(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	if err := store.Create(ctx, RunRecord{ID: "run-1", SessionID: "s", CallerID: "c"}); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, "run-1")
	got.Status = RunFailed
	again, _ := store.Get(ctx, "run-1")
	if again.Status == RunFailed {
		t.Fatal("mutating the returned copy changed the stored record")
	}
}

func TestRunStore_ListBySession(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	for i, id := range []string{"run-3", "run-1", "run-2"} {
		rec := RunRecord{ID: id, SessionID: "sess-a", CallerID: "c"}
		rec.StartedAt = time.Unix(int64(i), 0)
		if err := store.Create(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	// Different session
	if err := store.Create(ctx, RunRecord{ID: "run-x", SessionID: "sess-b", CallerID: "c"}); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ListBySession(ctx, "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("len = %d, want 3", len(runs))
	}
	// Ordered by StartedAt
	if runs[0].ID != "run-3" || runs[1].ID != "run-1" || runs[2].ID != "run-2" {
		t.Fatalf("order = %s, %s, %s", runs[0].ID, runs[1].ID, runs[2].ID)
	}
}

func TestRunStore_Prune(t *testing.T) {
	store := NewMemoryRunStore()
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	store.Create(ctx, RunRecord{ID: "old-done", SessionID: "s", CallerID: "c", Status: RunSucceeded, StartedAt: old, FinishedAt: old})
	store.Create(ctx, RunRecord{ID: "old-running", SessionID: "s", CallerID: "c", Status: RunRunning, StartedAt: old})
	store.Create(ctx, RunRecord{ID: "recent-done", SessionID: "s", CallerID: "c", Status: RunFailed, StartedAt: now, FinishedAt: now})

	removed, err := store.Prune(ctx, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := store.Get(ctx, "old-done"); !errors.Is(err, ErrRunNotFound) {
		t.Fatal("old-done should have been pruned")
	}
	if _, err := store.Get(ctx, "old-running"); err != nil {
		t.Fatal("old-running (non-terminal) should NOT have been pruned")
	}
	if _, err := store.Get(ctx, "recent-done"); err != nil {
		t.Fatal("recent-done should NOT have been pruned")
	}
}

func TestRunStatus_Terminal(t *testing.T) {
	terminal := []RunStatus{RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunOutcomeUnknown}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	nonTerminal := []RunStatus{RunStarting, RunRunning}
	for _, s := range nonTerminal {
		if s.Terminal() {
			t.Errorf("%q should NOT be terminal", s)
		}
	}
}
