// Package store_test reruns the runtime in-memory store test surface against
// the bbolt-backed BoltStore (and, as a control, against the memory stores
// themselves) via factories: every case runs on both backends so semantics —
// owner scoping, sentinel errors, TTL/lazy expiry, prune exemptions, turn
// arbitration, atomic capacity — are proven identical.
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/store"
)

type factories struct {
	name string
	// newSessions returns a fresh session store; limits may be zero.
	newSessions func(t *testing.T, limits runtime.SessionLimits) runtime.SessionStore
	// newRuns returns a fresh run store.
	newRuns func(t *testing.T) runtime.RunStore
}

func allFactories() []factories {
	return []factories{
		{
			name: "memory",
			newSessions: func(t *testing.T, limits runtime.SessionLimits) runtime.SessionStore {
				return runtime.NewCappedMemorySessionStore(limits)
			},
			newRuns: func(t *testing.T) runtime.RunStore { return runtime.NewMemoryRunStore() },
		},
		{
			name: "bbolt",
			newSessions: func(t *testing.T, limits runtime.SessionLimits) runtime.SessionStore {
				t.Helper()
				bs, err := store.OpenBolt(t.TempDir()+"/sessions.db", limits)
				if err != nil {
					t.Fatalf("OpenBolt: %v", err)
				}
				t.Cleanup(func() { _ = bs.Close() })
				return bs
			},
			newRuns: func(t *testing.T) runtime.RunStore {
				t.Helper()
				bs, err := store.OpenBolt(t.TempDir()+"/runs.db", runtime.SessionLimits{})
				if err != nil {
					t.Fatalf("OpenBolt: %v", err)
				}
				t.Cleanup(func() { _ = bs.Close() })
				return bs.Runs()
			},
		},
	}
}

func sessionRec(id, owner string) runtime.SessionRecord {
	return runtime.SessionRecord{
		ID:          id,
		ModelID:     "alpha",
		CallerID:    owner,
		WorkspaceID: "ws-1",
		Status:      runtime.SessionActive,
	}
}

func TestSessionStoreCreateAndGet(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			rec := sessionRec("s1", "alice")
			rec.NativeSessionID = "native-1"
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := s.Get(ctx, "s1", "alice")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ID != "s1" || got.CallerID != "alice" || got.ModelID != "alpha" {
				t.Fatalf("Get returned wrong record: %+v", got)
			}
			if got.NativeSessionID != "native-1" {
				t.Fatalf("NativeSessionID = %q, want %q", got.NativeSessionID, "native-1")
			}
			if got.Status != runtime.SessionActive {
				t.Fatalf("Status = %q, want %q", got.Status, runtime.SessionActive)
			}
		})
	}
}

func TestSessionStoreGetReturnsCopy(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := s.Get(ctx, "s1", "alice")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got.Status = runtime.SessionClosed
			got.ModelID = "hacked"
			got.WorkspaceID = "escaped"
			got.CallerID = "mallory"

			again, err := s.Get(ctx, "s1", "alice")
			if err != nil {
				t.Fatalf("second Get: %v", err)
			}
			if again.Status != runtime.SessionActive || again.ModelID != "alpha" ||
				again.WorkspaceID != "ws-1" || again.CallerID != "alice" {
				t.Fatalf("store state leaked after mutating returned record: %+v", again)
			}
		})
	}
}

func TestSessionStoreRejectsDuplicateCreate(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("first Create: %v", err)
			}
			if err := s.Create(ctx, sessionRec("s1", "bob")); !errors.Is(err, runtime.ErrSessionExists) {
				t.Fatalf("duplicate Create error = %v, want ErrSessionExists", err)
			}
		})
	}
}

func TestSessionStoreRejectsInvalidCreate(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("", "alice")); !errors.Is(err, runtime.ErrInvalidSession) {
				t.Fatalf("Create with empty ID error = %v, want ErrInvalidSession", err)
			}
			if err := s.Create(ctx, sessionRec("s1", "")); !errors.Is(err, runtime.ErrInvalidSession) {
				t.Fatalf("Create with empty owner error = %v, want ErrInvalidSession", err)
			}
			bad := sessionRec("s1", "alice")
			bad.Status = runtime.SessionStatus("nonsense")
			if err := s.Create(ctx, bad); !errors.Is(err, runtime.ErrInvalidSession) {
				t.Fatalf("Create with invalid status error = %v, want ErrInvalidSession", err)
			}
		})
	}
}

func TestSessionStoreOwnerIsolation(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			ops := []struct {
				name string
				run  func() error
			}{
				{"Get", func() error { _, err := s.Get(ctx, "s1", "bob"); return err }},
				{"Update", func() error {
					_, err := s.Update(ctx, "s1", "bob", func(*runtime.SessionRecord) {})
					return err
				}},
				{"BeginTurn", func() error { return s.BeginTurn(ctx, "s1", "bob") }},
				{"EndTurn", func() error { return s.EndTurn(ctx, "s1", "bob") }},
				{"Touch", func() error { return s.Touch(ctx, "s1", "bob", time.Minute) }},
				{"Delete", func() error { return s.Delete(ctx, "s1", "bob") }},
			}
			for _, op := range ops {
				t.Run(op.name, func(t *testing.T) {
					if err := op.run(); !errors.Is(err, runtime.ErrSessionForbidden) {
						t.Fatalf("%s by wrong owner error = %v, want ErrSessionForbidden", op.name, err)
					}
				})
			}

			if _, err := s.Get(ctx, "s1", "alice"); err != nil {
				t.Fatalf("session lost after rejected ops: %v", err)
			}
			if err := s.Create(ctx, sessionRec("s2", "bob")); err != nil {
				t.Fatalf("Create(s2, bob): %v", err)
			}
			aliceList, _ := s.List(ctx, "alice")
			if len(aliceList) != 1 || aliceList[0].ID != "s1" {
				t.Fatalf("List(alice) = %+v, want only s1", aliceList)
			}
			bobList, _ := s.List(ctx, "bob")
			if len(bobList) != 1 || bobList[0].ID != "s2" {
				t.Fatalf("List(bob) = %+v, want only s2", bobList)
			}
		})
	}
}

func TestSessionStoreSingleActiveTurn(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := s.BeginTurn(ctx, "s1", "alice"); err != nil {
				t.Fatalf("first BeginTurn: %v", err)
			}
			rec, _ := s.Get(ctx, "s1", "alice")
			if rec.Status != runtime.SessionTurnActive {
				t.Fatalf("Status after BeginTurn = %q, want %q", rec.Status, runtime.SessionTurnActive)
			}
			if err := s.BeginTurn(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionBusy) {
				t.Fatalf("second BeginTurn error = %v, want ErrSessionBusy", err)
			}
			if err := s.EndTurn(ctx, "s1", "alice"); err != nil {
				t.Fatalf("EndTurn: %v", err)
			}
			rec, _ = s.Get(ctx, "s1", "alice")
			if rec.Status != runtime.SessionActive {
				t.Fatalf("Status after EndTurn = %q, want %q", rec.Status, runtime.SessionActive)
			}
			if err := s.BeginTurn(ctx, "s1", "alice"); err != nil {
				t.Fatalf("BeginTurn after EndTurn: %v", err)
			}
		})
	}
}

func TestSessionStoreRejectsTurnsOnTerminalSessions(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			rec := sessionRec("s1", "alice")
			rec.Status = runtime.SessionClosed
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := s.BeginTurn(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionState) {
				t.Fatalf("BeginTurn on closed session error = %v, want ErrSessionState", err)
			}

			rec.Status = runtime.SessionClosing
			rec.ID = "s2"
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create(s2): %v", err)
			}
			if err := s.BeginTurn(ctx, "s2", "alice"); !errors.Is(err, runtime.ErrSessionState) {
				t.Fatalf("BeginTurn on closing session error = %v, want ErrSessionState", err)
			}
		})
	}
}

func TestSessionStoreUpdate(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			created := time.Unix(1000, 0)
			rec := sessionRec("s1", "alice")
			rec.CreatedAt = created
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			updated, err := s.Update(ctx, "s1", "alice", func(r *runtime.SessionRecord) {
				r.WorkerID = "worker-7"
				r.NodeID = "node-3"
				r.NativeSessionID = "native-9"
			})
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if updated.WorkerID != "worker-7" || updated.NodeID != "node-3" || updated.NativeSessionID != "native-9" {
				t.Fatalf("Update did not persist fields: %+v", updated)
			}
			if !updated.CreatedAt.Equal(created) {
				t.Fatalf("Update changed CreatedAt: %v, want %v", updated.CreatedAt, created)
			}
			if updated.UpdatedAt.Before(created) {
				t.Fatalf("UpdatedAt %v not stamped after creation", updated.UpdatedAt)
			}
			got, _ := s.Get(ctx, "s1", "alice")
			if got.WorkerID != "worker-7" {
				t.Fatalf("Update did not persist to store: %+v", got)
			}
		})
	}
}

func TestSessionStoreTouchAndExpiry(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			rec := sessionRec("s1", "alice")
			rec.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := s.Touch(ctx, "s1", "alice", time.Hour); err != nil {
				t.Fatalf("Touch: %v", err)
			}
			got, _ := s.Get(ctx, "s1", "alice")
			if !got.ExpiresAt.After(time.Now().Add(30 * time.Minute)) {
				t.Fatalf("Touch did not extend expiry: %v", got.ExpiresAt)
			}

			rec.ExpiresAt = time.Now().Add(-time.Second)
			rec.ID = "s-expired"
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create(s-expired): %v", err)
			}
			if _, err := s.Get(ctx, "s-expired", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Get on expired session error = %v, want ErrSessionNotFound", err)
			}

			rec.ID = "s-stale"
			rec.ExpiresAt = time.Now().Add(-time.Second)
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create(s-stale): %v", err)
			}
			removed, err := s.Prune(ctx, time.Now())
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if len(removed) != 1 || removed[0] != "s-stale" {
				t.Fatalf("Prune removed %v, want [s-stale]", removed)
			}
			if _, err := s.Get(ctx, "s-stale", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Get after Prune error = %v, want ErrSessionNotFound", err)
			}
			if _, err := s.Get(ctx, "s1", "alice"); err != nil {
				t.Fatalf("s1 should survive Prune: %v", err)
			}
		})
	}
}

// TestSessionStorePruneExemptsActiveTurn mirrors the U1 regression test: an
// expired record with a turn in flight must survive Prune and lazy Get.
func TestSessionStorePruneExemptsActiveTurn(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			rec := sessionRec("active-turn", "alice")
			rec.ExpiresAt = time.Now().Add(-time.Minute) // already past TTL
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := s.BeginTurn(ctx, "active-turn", "alice"); err != nil {
				t.Fatalf("BeginTurn: %v", err)
			}

			removed, err := s.Prune(ctx, time.Now())
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if len(removed) != 0 {
				t.Fatalf("Prune removed active turn: %v", removed)
			}
			got, err := s.Get(ctx, "active-turn", "alice")
			if err != nil {
				t.Fatalf("Get active turn: %v", err)
			}
			if got.Status != runtime.SessionTurnActive {
				t.Fatalf("status = %q, want turn_active", got.Status)
			}

			if err := s.EndTurn(ctx, "active-turn", "alice"); err != nil {
				t.Fatalf("EndTurn: %v", err)
			}
			removed, _ = s.Prune(ctx, time.Now())
			if len(removed) != 1 || removed[0] != "active-turn" {
				t.Fatalf("Prune after EndTurn removed %v, want [active-turn]", removed)
			}
		})
	}
}

func TestSessionStoreDelete(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := s.Delete(ctx, "s1", "alice"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := s.Get(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Get after Delete error = %v, want ErrSessionNotFound", err)
			}
			if err := s.Delete(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("second Delete error = %v, want ErrSessionNotFound", err)
			}
		})
	}
}

func TestSessionStorePruneEvictsExpiredAndReturnsIDs(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()
			const ttl = time.Hour

			for i := 0; i < 50; i++ {
				id := "sess-" + string(rune('a'+i%26)) + strconv.Itoa(i)
				if err := s.Create(ctx, sessionRec(id, "alice")); err != nil {
					t.Fatalf("Create(%s): %v", id, err)
				}
				if err := s.Touch(ctx, id, "alice", ttl); err != nil {
					t.Fatalf("Touch(%s): %v", id, err)
				}
			}
			if err := s.Create(ctx, sessionRec("survivor", "alice")); err != nil {
				t.Fatalf("Create(survivor): %v", err)
			}
			if err := s.Touch(ctx, "survivor", "alice", 24*time.Hour); err != nil {
				t.Fatalf("Touch(survivor): %v", err)
			}

			future := time.Now().Add(2 * ttl)
			removed, err := s.Prune(ctx, future)
			if err != nil {
				t.Fatalf("Prune: %v", err)
			}
			if len(removed) != 50 {
				t.Fatalf("Prune removed %d ids, want 50", len(removed))
			}
			if _, err := s.Get(ctx, "survivor", "alice"); err != nil {
				t.Fatalf("survivor should outlive Prune: %v", err)
			}
			again, _ := s.Prune(ctx, future)
			if len(again) != 0 {
				t.Fatalf("Prune removed %d on second sweep, want 0", len(again))
			}
		})
	}
}

func TestSessionStoreListScopedAndSorted(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			mk := func(id, owner string, created int64) runtime.SessionRecord {
				r := sessionRec(id, owner)
				r.CreatedAt = time.Unix(created, 0)
				return r
			}
			for _, rec := range []runtime.SessionRecord{
				mk("s3", "alice", 300),
				mk("s1", "alice", 100),
				mk("s2", "alice", 200),
				mk("sX", "bob", 50),
			} {
				if err := s.Create(ctx, rec); err != nil {
					t.Fatalf("Create(%s): %v", rec.ID, err)
				}
			}

			alice, err := s.List(ctx, "alice")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(alice) != 3 {
				t.Fatalf("List(alice) returned %d sessions, want 3", len(alice))
			}
			for i, want := range []string{"s1", "s2", "s3"} {
				if alice[i].ID != want {
					t.Fatalf("List(alice)[%d].ID = %q, want %q", i, alice[i].ID, want)
				}
			}
			bob, _ := s.List(ctx, "bob")
			if len(bob) != 1 || bob[0].ID != "sX" {
				t.Fatalf("List(bob) = %+v, want only sX", bob)
			}
		})
	}
}

func TestSessionStoreConcurrentBeginTurnOnlyOneWins(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("Create: %v", err)
			}
			const n = 16
			var wg sync.WaitGroup
			wins := make(chan struct{}, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := s.BeginTurn(ctx, "s1", "alice"); err == nil {
						wins <- struct{}{}
					}
				}()
			}
			wg.Wait()
			if got := len(wins); got != 1 {
				t.Fatalf("%d concurrent BeginTurn calls won, want exactly 1", got)
			}
			if err := s.EndTurn(ctx, "s1", "alice"); err != nil {
				t.Fatalf("EndTurn after race: %v", err)
			}
		})
	}
}

func TestSessionStorePerCallerCapRejected(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{MaxPerCaller: 2})
			ctx := context.Background()

			for _, id := range []string{"s1", "s2"} {
				if err := s.Create(ctx, sessionRec(id, "alice")); err != nil {
					t.Fatalf("create %s: %v", id, err)
				}
			}
			if err := s.Create(ctx, sessionRec("s3", "alice")); !errors.Is(err, runtime.ErrSessionCapacity) {
				t.Fatalf("third session for alice: got %v, want ErrSessionCapacity", err)
			}
			if err := s.Create(ctx, sessionRec("s-bob", "bob")); err != nil {
				t.Fatalf("bob create: %v", err)
			}
			if err := s.Delete(ctx, "s1", "alice"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if err := s.Create(ctx, sessionRec("s3", "alice")); err != nil {
				t.Fatalf("create after delete: %v", err)
			}
		})
	}
}

func TestSessionStoreGlobalCapRejected(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newSessions(t, runtime.SessionLimits{MaxTotal: 2})
			ctx := context.Background()

			if err := s.Create(ctx, sessionRec("s1", "alice")); err != nil {
				t.Fatalf("create s1: %v", err)
			}
			if err := s.Create(ctx, sessionRec("s2", "bob")); err != nil {
				t.Fatalf("create s2: %v", err)
			}
			if err := s.Create(ctx, sessionRec("s3", "carol")); !errors.Is(err, runtime.ErrSessionCapacity) {
				t.Fatalf("third session globally: got %v, want ErrSessionCapacity", err)
			}
		})
	}
}

// TestSessionStoreCapIsAtomicUnderConcurrency proves the bbolt single-writer
// transaction gives the same atomic check-and-insert the memory store gets
// from its mutex: concurrent creates never oversubscribe the cap.
func TestSessionStoreCapIsAtomicUnderConcurrency(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			const (
				attempts = 64
				cap      = 8
			)
			s := f.newSessions(t, runtime.SessionLimits{MaxPerCaller: cap})
			ctx := context.Background()

			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < attempts; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					_ = s.Create(ctx, sessionRec("s-"+strconv.Itoa(i), "alice"))
				}(i)
			}
			close(start)
			wg.Wait()

			recs, err := s.List(ctx, "alice")
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(recs) != cap {
				t.Fatalf("concurrent creates admitted %d records, want exactly the cap %d", len(recs), cap)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RunStore conformance
// ---------------------------------------------------------------------------

func TestRunStoreCreateAndGet(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()

			rec := runtime.RunRecord{
				ID:          "run-1",
				SessionID:   "sess-1",
				CallerID:    "caller-a",
				WorkspaceID: "ws-1",
				ModelID:     "codex",
				Status:      runtime.RunStarting,
			}
			if err := s.Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := s.Get(ctx, "run-1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ID != "run-1" || got.SessionID != "sess-1" || got.Status != runtime.RunStarting {
				t.Fatalf("Get returned %+v", got)
			}
			if got.StartedAt.IsZero() {
				t.Fatal("StartedAt should be stamped to now when zero")
			}
		})
	}
}

func TestRunStoreCreateDuplicate(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()

			rec := runtime.RunRecord{ID: "run-1", SessionID: "s", CallerID: "c"}
			if err := s.Create(ctx, rec); err != nil {
				t.Fatal(err)
			}
			if err := s.Create(ctx, rec); !errors.Is(err, runtime.ErrRunExists) {
				t.Fatalf("expected ErrRunExists, got %v", err)
			}
		})
	}
}

func TestRunStoreCreateInvalid(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()

			if err := s.Create(ctx, runtime.RunRecord{}); !errors.Is(err, runtime.ErrInvalidRun) {
				t.Fatalf("empty ID should be ErrInvalidRun, got %v", err)
			}
			if err := s.Create(ctx, runtime.RunRecord{ID: "r", Status: "bogus"}); !errors.Is(err, runtime.ErrInvalidRun) {
				t.Fatalf("bad status should be ErrInvalidRun, got %v", err)
			}
		})
	}
}

func TestRunStoreUpdate(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()

			rec := runtime.RunRecord{ID: "run-1", SessionID: "s", CallerID: "c", Status: runtime.RunStarting}
			if err := s.Create(ctx, rec); err != nil {
				t.Fatal(err)
			}
			updated, err := s.Update(ctx, "run-1", func(r *runtime.RunRecord) {
				r.Status = runtime.RunSucceeded
				r.FinishedAt = time.Now()
			})
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if updated.Status != runtime.RunSucceeded {
				t.Fatalf("status = %q, want succeeded", updated.Status)
			}
			if updated.StartedAt.IsZero() {
				t.Fatal("StartedAt should be preserved")
			}
			// An update to an unknown status is rejected and not persisted.
			if _, err := s.Update(ctx, "run-1", func(r *runtime.RunRecord) {
				r.Status = "bogus"
			}); !errors.Is(err, runtime.ErrInvalidRun) {
				t.Fatalf("bad status update should be ErrInvalidRun, got %v", err)
			}
			got, _ := s.Get(ctx, "run-1")
			if got.Status != runtime.RunSucceeded {
				t.Fatalf("rejected update was persisted: %q", got.Status)
			}
		})
	}
}

func TestRunStoreUpdateNotFound(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			_, err := s.Update(context.Background(), "nope", func(r *runtime.RunRecord) {})
			if !errors.Is(err, runtime.ErrRunNotFound) {
				t.Fatalf("expected ErrRunNotFound, got %v", err)
			}
		})
	}
}

func TestRunStoreGetReturnsDeepCopy(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()

			if err := s.Create(ctx, runtime.RunRecord{ID: "run-1", SessionID: "s", CallerID: "c"}); err != nil {
				t.Fatal(err)
			}
			got, _ := s.Get(ctx, "run-1")
			got.Status = runtime.RunFailed
			again, _ := s.Get(ctx, "run-1")
			if again.Status == runtime.RunFailed {
				t.Fatal("mutating the returned copy changed the stored record")
			}
		})
	}
}

func TestRunStoreListBySession(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()

			for i, id := range []string{"run-3", "run-1", "run-2"} {
				rec := runtime.RunRecord{ID: id, SessionID: "sess-a", CallerID: "c"}
				rec.StartedAt = time.Unix(int64(i), 0)
				if err := s.Create(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Create(ctx, runtime.RunRecord{ID: "run-x", SessionID: "sess-b", CallerID: "c"}); err != nil {
				t.Fatal(err)
			}
			runs, err := s.ListBySession(ctx, "sess-a")
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 3 {
				t.Fatalf("len = %d, want 3", len(runs))
			}
			if runs[0].ID != "run-3" || runs[1].ID != "run-1" || runs[2].ID != "run-2" {
				t.Fatalf("order = %s, %s, %s", runs[0].ID, runs[1].ID, runs[2].ID)
			}
		})
	}
}

func TestRunStorePrune(t *testing.T) {
	for _, f := range allFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newRuns(t)
			ctx := context.Background()
			now := time.Now()
			old := now.Add(-2 * time.Hour)

			mustCreate := func(rec runtime.RunRecord) {
				t.Helper()
				if err := s.Create(ctx, rec); err != nil {
					t.Fatalf("Create(%s): %v", rec.ID, err)
				}
			}
			mustCreate(runtime.RunRecord{ID: "old-done", SessionID: "s", CallerID: "c", Status: runtime.RunSucceeded, StartedAt: old, FinishedAt: old})
			mustCreate(runtime.RunRecord{ID: "old-running", SessionID: "s", CallerID: "c", Status: runtime.RunRunning, StartedAt: old})
			mustCreate(runtime.RunRecord{ID: "recent-done", SessionID: "s", CallerID: "c", Status: runtime.RunFailed, StartedAt: now, FinishedAt: now})

			removed, err := s.Prune(ctx, now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if removed != 1 {
				t.Fatalf("removed = %d, want 1", removed)
			}
			if _, err := s.Get(ctx, "old-done"); !errors.Is(err, runtime.ErrRunNotFound) {
				t.Fatal("old-done should have been pruned")
			}
			if _, err := s.Get(ctx, "old-running"); err != nil {
				t.Fatal("old-running (non-terminal) should NOT have been pruned")
			}
			if _, err := s.Get(ctx, "recent-done"); err != nil {
				t.Fatal("recent-done should NOT have been pruned")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// bbolt-specific persistence: records survive a close/reopen cycle.
// ---------------------------------------------------------------------------

// TestBoltStorePersistsAcrossReopen is the core restart-durability proof:
// sessions and runs written before Close are readable after reopening the
// same database file.
func TestBoltStorePersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/gateway.db"
	ctx := context.Background()

	bs, err := store.OpenBolt(path, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	rec := sessionRec("s1", "alice")
	rec.NativeSessionID = "native-1"
	if err := bs.Create(ctx, rec); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if err := bs.Runs().Create(ctx, runtime.RunRecord{ID: "run-1", SessionID: "s1", CallerID: "alice", Status: runtime.RunSucceeded, FinishedAt: time.Now()}); err != nil {
		t.Fatalf("Create run: %v", err)
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.OpenBolt(path, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got, err := reopened.Get(ctx, "s1", "alice")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.NativeSessionID != "native-1" || got.ModelID != "alpha" {
		t.Fatalf("session lost fields across reopen: %+v", got)
	}
	run, err := reopened.Runs().Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("Get run after reopen: %v", err)
	}
	if run.Status != runtime.RunSucceeded || run.SessionID != "s1" {
		t.Fatalf("run lost fields across reopen: %+v", run)
	}
}

// TestBoltStoreCapacitySurvivesReopen pins that the cap counts persisted
// records, not just ones created in this process: after a reopen the cap is
// still enforced against what is on disk.
func TestBoltStoreCapacitySurvivesReopen(t *testing.T) {
	path := t.TempDir() + "/gateway.db"
	ctx := context.Background()

	bs, err := store.OpenBolt(path, runtime.SessionLimits{MaxTotal: 1})
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	if err := bs.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := store.OpenBolt(path, runtime.SessionLimits{MaxTotal: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.Create(ctx, sessionRec("s2", "bob")); !errors.Is(err, runtime.ErrSessionCapacity) {
		t.Fatalf("Create after reopen at cap error = %v, want ErrSessionCapacity", err)
	}
}

// TestBoltStoreConcurrentMixedOperations exercises concurrent readers and the
// single writer (run under -race): sessions are created, listed, touched and
// turned concurrently without corruption.
func TestBoltStoreConcurrentMixedOperations(t *testing.T) {
	bs, err := store.OpenBolt(t.TempDir()+"/gateway.db", runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	defer func() { _ = bs.Close() }()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := fmt.Sprintf("owner-%d", i%2)
			id := fmt.Sprintf("s-%d", i)
			if err := bs.Create(ctx, sessionRec(id, owner)); err != nil {
				t.Errorf("Create(%s): %v", id, err)
				return
			}
			if _, err := bs.List(ctx, owner); err != nil {
				t.Errorf("List(%s): %v", owner, err)
			}
			if err := bs.BeginTurn(ctx, id, owner); err != nil {
				t.Errorf("BeginTurn(%s): %v", id, err)
			}
			if err := bs.Runs().Create(ctx, runtime.RunRecord{ID: "run-" + id, SessionID: id, CallerID: owner}); err != nil {
				t.Errorf("Create run: %v", err)
			}
			if _, err := bs.Runs().ListBySession(ctx, id); err != nil {
				t.Errorf("ListBySession(%s): %v", id, err)
			}
			if err := bs.EndTurn(ctx, id, owner); err != nil {
				t.Errorf("EndTurn(%s): %v", id, err)
			}
		}(i)
	}
	wg.Wait()
}
