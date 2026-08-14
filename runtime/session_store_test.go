package runtime_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

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
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	rec := sessionRec("s1", "alice")
	rec.NativeSessionID = "native-1"
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, "s1", "alice")
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
}

// TestSessionStoreGetReturnsCopy pins the no-pointer-leak rule: mutating a
// returned record must never mutate the store's internal state.
func TestSessionStoreGetReturnsCopy(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, "s1", "alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got.Status = runtime.SessionClosed
	got.ModelID = "hacked"
	got.WorkspaceID = "escaped"
	got.CallerID = "mallory"

	again, err := store.Get(ctx, "s1", "alice")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if again.Status != runtime.SessionActive || again.ModelID != "alpha" {
		t.Fatalf("store state leaked after mutating returned record: %+v", again)
	}
	if again.WorkspaceID != "ws-1" || again.CallerID != "alice" {
		t.Fatalf("store state leaked after mutating returned record: %+v", again)
	}
}

func TestSessionStoreRejectsDuplicateCreate(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := store.Create(ctx, sessionRec("s1", "bob")); !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("duplicate Create error = %v, want ErrSessionExists", err)
	}
}

func TestSessionStoreRejectsInvalidCreate(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	emptyID := sessionRec("", "alice")
	if err := store.Create(ctx, emptyID); !errors.Is(err, runtime.ErrInvalidSession) {
		t.Fatalf("Create with empty ID error = %v, want ErrInvalidSession", err)
	}
	emptyOwner := sessionRec("s1", "")
	if err := store.Create(ctx, emptyOwner); !errors.Is(err, runtime.ErrInvalidSession) {
		t.Fatalf("Create with empty owner error = %v, want ErrInvalidSession", err)
	}
	badStatus := sessionRec("s1", "alice")
	badStatus.Status = runtime.SessionStatus("nonsense")
	if err := store.Create(ctx, badStatus); !errors.Is(err, runtime.ErrInvalidSession) {
		t.Fatalf("Create with invalid status error = %v, want ErrInvalidSession", err)
	}
}

// TestSessionStoreOwnerIsolation verifies every read and mutation is
// owner-scoped: a request from a different owner must fail with
// ErrSessionForbidden and must never see the session.
func TestSessionStoreOwnerIsolation(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ops := []struct {
		name string
		run  func() error
	}{
		{"Get", func() error { _, err := store.Get(ctx, "s1", "bob"); return err }},
		{"Update", func() error {
			_, err := store.Update(ctx, "s1", "bob", func(*runtime.SessionRecord) {})
			return err
		}},
		{"BeginTurn", func() error { return store.BeginTurn(ctx, "s1", "bob") }},
		{"EndTurn", func() error { return store.EndTurn(ctx, "s1", "bob") }},
		{"Touch", func() error { return store.Touch(ctx, "s1", "bob", time.Minute) }},
		{"Delete", func() error { return store.Delete(ctx, "s1", "bob") }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			if err := op.run(); !errors.Is(err, runtime.ErrSessionForbidden) {
				t.Fatalf("%s by wrong owner error = %v, want ErrSessionForbidden", op.name, err)
			}
		})
	}

	// The session must still exist and be owned by alice after all the
	// rejected operations.
	if _, err := store.Get(ctx, "s1", "alice"); err != nil {
		t.Fatalf("session lost after rejected ops: %v", err)
	}
	// alice must not see bob's sessions and vice versa.
	if err := store.Create(ctx, sessionRec("s2", "bob")); err != nil {
		t.Fatalf("Create(s2, bob): %v", err)
	}
	aliceList, _ := store.List(ctx, "alice")
	if len(aliceList) != 1 || aliceList[0].ID != "s1" {
		t.Fatalf("List(alice) = %+v, want only s1", aliceList)
	}
	bobList, _ := store.List(ctx, "bob")
	if len(bobList) != 1 || bobList[0].ID != "s2" {
		t.Fatalf("List(bob) = %+v, want only s2", bobList)
	}
}

func TestSessionStoreSingleActiveTurn(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.BeginTurn(ctx, "s1", "alice"); err != nil {
		t.Fatalf("first BeginTurn: %v", err)
	}
	rec, _ := store.Get(ctx, "s1", "alice")
	if rec.Status != runtime.SessionTurnActive {
		t.Fatalf("Status after BeginTurn = %q, want %q", rec.Status, runtime.SessionTurnActive)
	}

	// A second active turn on the same session must be rejected.
	if err := store.BeginTurn(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionBusy) {
		t.Fatalf("second BeginTurn error = %v, want ErrSessionBusy", err)
	}

	// EndTurn clears the active-turn flag and returns the session to active.
	if err := store.EndTurn(ctx, "s1", "alice"); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	rec, _ = store.Get(ctx, "s1", "alice")
	if rec.Status != runtime.SessionActive {
		t.Fatalf("Status after EndTurn = %q, want %q", rec.Status, runtime.SessionActive)
	}
	if err := store.BeginTurn(ctx, "s1", "alice"); err != nil {
		t.Fatalf("BeginTurn after EndTurn: %v", err)
	}
}

func TestSessionStoreRejectsTurnsOnTerminalSessions(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	rec := sessionRec("s1", "alice")
	rec.Status = runtime.SessionClosed
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.BeginTurn(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionState) {
		t.Fatalf("BeginTurn on closed session error = %v, want ErrSessionState", err)
	}

	rec.Status = runtime.SessionClosing
	rec.ID = "s2"
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create(s2): %v", err)
	}
	if err := store.BeginTurn(ctx, "s2", "alice"); !errors.Is(err, runtime.ErrSessionState) {
		t.Fatalf("BeginTurn on closing session error = %v, want ErrSessionState", err)
	}
}

func TestSessionStoreUpdate(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	created := time.Unix(1000, 0)
	rec := sessionRec("s1", "alice")
	rec.CreatedAt = created
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := store.Update(ctx, "s1", "alice", func(r *runtime.SessionRecord) {
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
	// CreatedAt is immutable; UpdatedAt must be stamped forward.
	if !updated.CreatedAt.Equal(created) {
		t.Fatalf("Update changed CreatedAt: %v, want %v", updated.CreatedAt, created)
	}
	if updated.UpdatedAt.Before(created) {
		t.Fatalf("UpdatedAt %v not stamped after creation", updated.UpdatedAt)
	}

	got, _ := store.Get(ctx, "s1", "alice")
	if got.WorkerID != "worker-7" {
		t.Fatalf("Update did not persist to store: %+v", got)
	}
}

func TestSessionStoreTouchAndExpiry(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	rec := sessionRec("s1", "alice")
	rec.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Touch with a TTL extends the expiry and stamps UpdatedAt.
	if err := store.Touch(ctx, "s1", "alice", time.Hour); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _ := store.Get(ctx, "s1", "alice")
	if !got.ExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("Touch did not extend expiry: %v", got.ExpiresAt)
	}

	// Expired records are purged lazily on Get.
	rec.ExpiresAt = time.Now().Add(-time.Second)
	rec.ID = "s-expired"
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create(s-expired): %v", err)
	}
	if _, err := store.Get(ctx, "s-expired", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Get on expired session error = %v, want ErrSessionNotFound", err)
	}

	// Prune physically removes expired records at the given clock time.
	rec.ID = "s-stale"
	rec.ExpiresAt = time.Now().Add(-time.Second)
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create(s-stale): %v", err)
	}
	removed, err := store.Prune(ctx, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "s-stale" {
		t.Fatalf("Prune removed %v, want [s-stale]", removed)
	}
	if _, err := store.Get(ctx, "s-stale", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Get after Prune error = %v, want ErrSessionNotFound", err)
	}
	// A fresh session must survive Prune.
	if _, err := store.Get(ctx, "s1", "alice"); err != nil {
		t.Fatalf("s1 should survive Prune: %v", err)
	}
}

// TestSessionStorePruneExemptsActiveTurn is a regression test for the U1
// active-turn prune bug: Prune and lazy Get purge must never remove a session
// that has a turn in flight, even if ExpiresAt has passed. A long turn that
// exceeds the record TTL must keep running, not have its record/handle yanked.
func TestSessionStorePruneExemptsActiveTurn(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()
	rec := sessionRec("active-turn", "alice")
	rec.ExpiresAt = time.Now().Add(-time.Minute) // already past TTL
	if err := store.Create(ctx, rec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Begin a turn: the record is now active AND expired.
	if err := store.BeginTurn(ctx, "active-turn", "alice"); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}

	// Prune must NOT remove the active-turn record.
	removed, err := store.Prune(ctx, time.Now())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("Prune removed active turn: %v", removed)
	}
	// Lazy Get must still return it (not purge/treat as expired).
	got, err := store.Get(ctx, "active-turn", "alice")
	if err != nil {
		t.Fatalf("Get active turn: %v", err)
	}
	if got.Status != runtime.SessionTurnActive {
		t.Fatalf("status = %q, want turn_active", got.Status)
	}

	// Once the turn ends, the (still-expired) record becomes prunable.
	if err := store.EndTurn(ctx, "active-turn", "alice"); err != nil {
		t.Fatalf("EndTurn: %v", err)
	}
	removed, _ = store.Prune(ctx, time.Now())
	if len(removed) != 1 || removed[0] != "active-turn" {
		t.Fatalf("Prune after EndTurn removed %v, want [active-turn]", removed)
	}
}

func TestSessionStoreDelete(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(ctx, "s1", "alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Get after Delete error = %v, want ErrSessionNotFound", err)
	}
	// Deleting again reports not found, not forbidden.
	if err := store.Delete(ctx, "s1", "alice"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("second Delete error = %v, want ErrSessionNotFound", err)
	}
}

// TestSessionStorePruneEvictsExpiredAndReturnsIDs is a regression test for the
// session-record leak: records with a TTL must be physically evicted by Prune
// and their ids returned so the gateway can drop cached handles. Without TTL
// wiring, byID grew by one entry per session forever.
func TestSessionStorePruneEvictsExpiredAndReturnsIDs(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()
	const ttl = time.Hour

	// Create 50 sessions, each touched with a TTL so they have an ExpiresAt.
	for i := 0; i < 50; i++ {
		id := "sess-" + string(rune('a'+i%26)) + strconv.Itoa(i)
		if err := store.Create(ctx, sessionRec(id, "alice")); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
		if err := store.Touch(ctx, id, "alice", ttl); err != nil {
			t.Fatalf("Touch(%s): %v", id, err)
		}
	}
	// One fresh session touched far in the future must survive the sweep.
	if err := store.Create(ctx, sessionRec("survivor", "alice")); err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if err := store.Touch(ctx, "survivor", "alice", 24*time.Hour); err != nil {
		t.Fatalf("Touch(survivor): %v", err)
	}

	// Fast-forward past the 50 sessions' TTL but not the survivor's.
	future := time.Now().Add(2 * ttl)
	removed, err := store.Prune(ctx, future)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 50 {
		t.Fatalf("Prune removed %d ids, want 50 (no monotonic growth)", len(removed))
	}
	if _, err := store.Get(ctx, "survivor", "alice"); err != nil {
		t.Fatalf("survivor should outlive Prune: %v", err)
	}
	// A second sweep at the same clock finds nothing left to evict.
	again, _ := store.Prune(ctx, future)
	if len(again) != 0 {
		t.Fatalf("Prune removed %d on second sweep, want 0", len(again))
	}
}

func TestSessionStoreListScopedAndSorted(t *testing.T) {
	store := runtime.NewMemorySessionStore()
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
		if err := store.Create(ctx, rec); err != nil {
			t.Fatalf("Create(%s): %v", rec.ID, err)
		}
	}

	alice, err := store.List(ctx, "alice")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(alice) != 3 {
		t.Fatalf("List(alice) returned %d sessions, want 3", len(alice))
	}
	for i, want := range []string{"s1", "s2", "s3"} {
		if alice[i].ID != want {
			t.Fatalf("List(alice)[%d].ID = %q, want %q (sorted by CreatedAt)", i, alice[i].ID, want)
		}
	}

	bob, _ := store.List(ctx, "bob")
	if len(bob) != 1 || bob[0].ID != "sX" {
		t.Fatalf("List(bob) = %+v, want only sX", bob)
	}
}

// TestSessionStoreConcurrentBeginTurnOnlyOneWins proves the single-active-turn
// constraint holds under concurrency (run with -race): exactly one of N racing
// BeginTurn calls may win.
func TestSessionStoreConcurrentBeginTurnOnlyOneWins(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	wins := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.BeginTurn(ctx, "s1", "alice"); err == nil {
				wins <- struct{}{}
			}
		}()
	}
	wg.Wait()
	if got := len(wins); got != 1 {
		t.Fatalf("%d concurrent BeginTurn calls won, want exactly 1", got)
	}

	// The session must still be owned by alice and a subsequent EndTurn works.
	if err := store.EndTurn(ctx, "s1", "alice"); err != nil {
		t.Fatalf("EndTurn after race: %v", err)
	}
}

// TestSessionRecordContainsNoProcessPointers is a structural guard for the
// "no process pointers leak" invariant: every field of SessionRecord must be a
// value type (string, time.Time, enum). Anything pointer-like (ptr, slice,
// map, chan, func, interface, unsafe.Pointer) would let callers mutate store
// internals or escape the process.
func TestSessionRecordContainsNoProcessPointers(t *testing.T) {
	typ := reflect.TypeOf(runtime.SessionRecord{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		switch f.Type.Kind() {
		case reflect.Ptr, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func,
			reflect.Interface, reflect.UnsafePointer:
			t.Errorf("SessionRecord.%s has kind %v: pointer-like fields are forbidden", f.Name, f.Type.Kind())
		}
	}
}

// TestSessionStore_PerCallerCapRejected is a regression test for the Phase 2
// session record cap: a caller at its record cap gets ErrSessionCapacity from
// Create (mapped to 429 by the API layer), while other callers still create.
func TestSessionStore_PerCallerCapRejected(t *testing.T) {
	store := runtime.NewCappedMemorySessionStore(runtime.SessionLimits{MaxPerCaller: 2})
	ctx := context.Background()

	for _, id := range []string{"s1", "s2"} {
		if err := store.Create(ctx, sessionRec(id, "alice")); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	err := store.Create(ctx, sessionRec("s3", "alice"))
	if !errors.Is(err, runtime.ErrSessionCapacity) {
		t.Fatalf("third session for alice: got %v, want ErrSessionCapacity", err)
	}
	// Another caller is unaffected by alice's cap.
	if err := store.Create(ctx, sessionRec("s-bob", "bob")); err != nil {
		t.Fatalf("bob create: %v", err)
	}
	// Deleting a record frees capacity.
	if err := store.Delete(ctx, "s1", "alice"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Create(ctx, sessionRec("s3", "alice")); err != nil {
		t.Fatalf("create after delete: %v", err)
	}
}

// TestSessionStore_GlobalCapRejected verifies the global session record cap
// spans all callers.
func TestSessionStore_GlobalCapRejected(t *testing.T) {
	store := runtime.NewCappedMemorySessionStore(runtime.SessionLimits{MaxTotal: 2})
	ctx := context.Background()

	if err := store.Create(ctx, sessionRec("s1", "alice")); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if err := store.Create(ctx, sessionRec("s2", "bob")); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	err := store.Create(ctx, sessionRec("s3", "carol"))
	if !errors.Is(err, runtime.ErrSessionCapacity) {
		t.Fatalf("third session globally: got %v, want ErrSessionCapacity", err)
	}
}

// TestSessionStore_CapIsAtomicUnderConcurrency is a regression test for the
// Phase 2 review finding that the per-caller cap was a non-atomic
// list-then-create check: concurrent Create calls for the same caller must
// never admit more than MaxPerCaller records.
func TestSessionStore_CapIsAtomicUnderConcurrency(t *testing.T) {
	const (
		attempts = 64
		cap      = 8
	)
	store := runtime.NewCappedMemorySessionStore(runtime.SessionLimits{MaxPerCaller: cap})
	ctx := context.Background()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_ = store.Create(ctx, sessionRec("s-"+strconv.Itoa(i), "alice"))
		}(i)
	}
	close(start)
	wg.Wait()

	recs, err := store.List(ctx, "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != cap {
		t.Fatalf("concurrent creates admitted %d records, want exactly the cap %d", len(recs), cap)
	}
}

// TestSessionStore_UncappedStoreUnlimited verifies the default store keeps
// unlimited semantics (backwards compatibility).
func TestSessionStore_UncappedStoreUnlimited(t *testing.T) {
	store := runtime.NewMemorySessionStore()
	ctx := context.Background()
	for i := 0; i < 32; i++ {
		if err := store.Create(ctx, sessionRec("s"+strconv.Itoa(i), "alice")); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
}
