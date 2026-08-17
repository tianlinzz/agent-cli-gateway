package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
	"github.com/tianlinzz/agent-cli-gateway/store"
)

// IdempotencyStore consistency suite: the same cases run against the memory
// implementation and the bbolt-backed view so semantics — conflict signals,
// fingerprint mismatch, expiry/re-occupation, deep copies, caller isolation —
// are proven identical on both backends.

type idemFactories struct {
	name    string
	newIdem func(t *testing.T) runtime.IdempotencyStore
}

func allIdemFactories() []idemFactories {
	return []idemFactories{
		{
			name:    "memory",
			newIdem: func(t *testing.T) runtime.IdempotencyStore { return runtime.NewMemoryIdempotencyStore() },
		},
		{
			name: "bbolt",
			newIdem: func(t *testing.T) runtime.IdempotencyStore {
				t.Helper()
				bs, err := store.OpenBolt(t.TempDir()+"/idem.db", runtime.SessionLimits{})
				if err != nil {
					t.Fatalf("OpenBolt: %v", err)
				}
				t.Cleanup(func() { _ = bs.Close() })
				return bs.Idempotency()
			},
		},
	}
}

func idemRec(key, caller, fp string) runtime.IdempotencyRecord {
	return runtime.IdempotencyRecord{
		Key:         key,
		CallerID:    caller,
		Endpoint:    "/v1/chat/completions",
		Fingerprint: fp,
		RunID:       "run-" + key + "-" + caller,
		Status:      runtime.IdempotencyActive,
		ExpiresAt:   time.Now().Add(time.Hour),
	}
}

func TestIdempotencyBeginAndLookup(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			got, conflict, err := s.Begin(ctx, idemRec("k1", "alice", "fp1"))
			if err != nil || conflict {
				t.Fatalf("Begin = (%v, %v), want no conflict", err, conflict)
			}
			if got.CreatedAt.IsZero() {
				t.Fatal("CreatedAt was not stamped")
			}

			rec, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "k1")
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if rec.Status != runtime.IdempotencyActive || rec.RunID != "run-k1-alice" || rec.Fingerprint != "fp1" {
				t.Fatalf("record = %+v, want active run-k1-alice fp1", rec)
			}

			if _, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "nope"); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Lookup unknown = %v, want ErrIdempotencyNotFound", err)
			}
		})
	}
}

func TestIdempotencyBeginConflictAndMismatch(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			if _, _, err := s.Begin(ctx, idemRec("k1", "alice", "fp1")); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			// Same key + same fingerprint: conflict with the EXISTING record.
			existing, conflict, err := s.Begin(ctx, idemRec("k1", "alice", "fp1"))
			if err != nil || !conflict {
				t.Fatalf("duplicate Begin = (%v, %v), want conflict", err, conflict)
			}
			if existing.RunID != "run-k1-alice" {
				t.Fatalf("existing.RunID = %q, want the first claim's run id", existing.RunID)
			}
			// Same key + different fingerprint: mismatch sentinel.
			dup := idemRec("k1", "alice", "fp2")
			if _, _, err := s.Begin(ctx, dup); !errors.Is(err, runtime.ErrIdempotencyMismatch) {
				t.Fatalf("mismatched Begin = %v, want ErrIdempotencyMismatch", err)
			}
			// A different endpoint or caller never collides.
			other := idemRec("k1", "alice", "fp2")
			other.Endpoint = "/v1/other"
			if _, conflict, err := s.Begin(ctx, other); err != nil || conflict {
				t.Fatalf("other endpoint Begin = (%v, %v), want a fresh claim", err, conflict)
			}
		})
	}
}

func TestIdempotencyExpiredKeyReoccupiable(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			rec := idemRec("k1", "alice", "fp1")
			rec.ExpiresAt = time.Now().Add(-time.Minute) // already expired
			if _, _, err := s.Begin(ctx, rec); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			// Expired records are invisible...
			if _, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "k1"); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Lookup expired = %v, want ErrIdempotencyNotFound", err)
			}
			// ...and the key can be re-occupied, even with a new fingerprint.
			fresh := idemRec("k1", "alice", "fp2")
			got, conflict, err := s.Begin(ctx, fresh)
			if err != nil || conflict {
				t.Fatalf("re-occupy Begin = (%v, %v), want a fresh claim", err, conflict)
			}
			if got.Fingerprint != "fp2" {
				t.Fatalf("Fingerprint = %q, want fp2", got.Fingerprint)
			}
		})
	}
}

func TestIdempotencyCompleteAndBodyCopy(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			if _, _, err := s.Begin(ctx, idemRec("k1", "alice", "fp1")); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			body := []byte(`{"ok":true}`)
			if err := s.Complete(ctx, "alice", "/v1/chat/completions", "k1", "run-k1-alice", runtime.RunSucceeded, body); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			// Mutating the caller's slice must not corrupt the stored body.
			body[0] = 'X'

			rec, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "k1")
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if rec.Status != runtime.IdempotencyCompleted || rec.RunStatus != runtime.RunSucceeded {
				t.Fatalf("status = %q/%q, want completed/succeeded", rec.Status, rec.RunStatus)
			}
			if string(rec.ResponseBody) != `{"ok":true}` {
				t.Fatalf("body = %q, want the original bytes", rec.ResponseBody)
			}
			// The returned body is a copy too.
			rec.ResponseBody[0] = 'Y'
			again, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "k1")
			if err != nil {
				t.Fatalf("Lookup again: %v", err)
			}
			if string(again.ResponseBody) != `{"ok":true}` {
				t.Fatalf("body after mutation = %q, want the original bytes", again.ResponseBody)
			}

			// Completing an unknown or expired key fails with the sentinel.
			if err := s.Complete(ctx, "alice", "/v1/chat/completions", "nope", "r", runtime.RunFailed, nil); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Complete unknown = %v, want ErrIdempotencyNotFound", err)
			}
			exp := idemRec("k2", "alice", "fp1")
			exp.ExpiresAt = time.Now().Add(-time.Minute)
			if _, _, err := s.Begin(ctx, exp); err != nil {
				t.Fatalf("Begin expired: %v", err)
			}
			if err := s.Complete(ctx, "alice", "/v1/chat/completions", "k2", "r", runtime.RunFailed, nil); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Complete expired = %v, want ErrIdempotencyNotFound", err)
			}
		})
	}
}

func TestIdempotencyDelete(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			if _, _, err := s.Begin(ctx, idemRec("k1", "alice", "fp1")); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if err := s.Delete(ctx, "alice", "/v1/chat/completions", "k1"); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "k1"); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Lookup after Delete = %v, want ErrIdempotencyNotFound", err)
			}
			if err := s.Delete(ctx, "alice", "/v1/chat/completions", "k1"); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("second Delete = %v, want ErrIdempotencyNotFound", err)
			}
			// The released key is immediately reusable.
			if _, conflict, err := s.Begin(ctx, idemRec("k1", "alice", "fp9")); err != nil || conflict {
				t.Fatalf("Begin after Delete = (%v, %v), want a fresh claim", err, conflict)
			}
		})
	}
}

func TestIdempotencyPrune(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()
			now := time.Now()

			live := idemRec("live", "alice", "fp1")
			live.ExpiresAt = now.Add(time.Hour)
			dead := idemRec("dead", "alice", "fp2")
			dead.ExpiresAt = now.Add(-time.Minute)
			for _, rec := range []runtime.IdempotencyRecord{live, dead} {
				if _, _, err := s.Begin(ctx, rec); err != nil {
					t.Fatalf("Begin %s: %v", rec.Key, err)
				}
			}
			removed, err := s.Prune(ctx, now)
			if err != nil || removed != 1 {
				t.Fatalf("Prune = (%d, %v), want 1 removed", removed, err)
			}
			if _, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "live"); err != nil {
				t.Fatalf("live record pruned: %v", err)
			}
			if _, err := s.Lookup(ctx, "alice", "/v1/chat/completions", "dead"); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Lookup pruned = %v, want ErrIdempotencyNotFound", err)
			}
		})
	}
}

func TestIdempotencyCallerIsolation(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			if _, _, err := s.Begin(ctx, idemRec("k1", "alice", "fp1")); err != nil {
				t.Fatalf("Begin alice: %v", err)
			}
			// The same key under another caller is an independent claim, even
			// with a different fingerprint.
			if _, conflict, err := s.Begin(ctx, idemRec("k1", "bob", "fp2")); err != nil || conflict {
				t.Fatalf("Begin bob = (%v, %v), want an independent claim", err, conflict)
			}
			if _, err := s.Lookup(ctx, "carol", "/v1/chat/completions", "k1"); !errors.Is(err, runtime.ErrIdempotencyNotFound) {
				t.Fatalf("Lookup as carol = %v, want ErrIdempotencyNotFound", err)
			}
		})
	}
}

func TestIdempotencyConcurrentBeginOnlyOneWins(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			const n = 32
			var wg sync.WaitGroup
			var mu sync.Mutex
			claims, conflicts := 0, 0
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, conflict, err := s.Begin(ctx, idemRec("k1", "alice", "fp1"))
					if err != nil {
						t.Errorf("Begin: %v", err)
						return
					}
					mu.Lock()
					if conflict {
						conflicts++
					} else {
						claims++
					}
					mu.Unlock()
				}()
			}
			wg.Wait()
			if claims != 1 || conflicts != n-1 {
				t.Fatalf("claims = %d, conflicts = %d, want 1 claim and %d conflicts", claims, conflicts, n-1)
			}
		})
	}
}

func TestIdempotencyValidation(t *testing.T) {
	for _, f := range allIdemFactories() {
		t.Run(f.name, func(t *testing.T) {
			s := f.newIdem(t)
			ctx := context.Background()

			bad := idemRec("k1", "alice", "fp1")
			bad.Key = ""
			if _, _, err := s.Begin(ctx, bad); !errors.Is(err, runtime.ErrInvalidIdempotency) {
				t.Fatalf("empty key Begin = %v, want ErrInvalidIdempotency", err)
			}
			bad = idemRec("k1", "alice", "")
			if _, _, err := s.Begin(ctx, bad); !errors.Is(err, runtime.ErrInvalidIdempotency) {
				t.Fatalf("empty fingerprint Begin = %v, want ErrInvalidIdempotency", err)
			}
			bad = idemRec("k1", "alice", "fp1")
			bad.Status = "bogus"
			if _, _, err := s.Begin(ctx, bad); !errors.Is(err, runtime.ErrInvalidIdempotency) {
				t.Fatalf("bad status Begin = %v, want ErrInvalidIdempotency", err)
			}
		})
	}
}

// TestBoltIdempotencyPersistsAcrossReopen is the bbolt-only persistence proof:
// a completed record (including its replay body) survives a process restart.
func TestBoltIdempotencyPersistsAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/idem.db"
	ctx := context.Background()

	bs, err := store.OpenBolt(path, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	if _, _, err := bs.Idempotency().Begin(ctx, idemRec("k1", "alice", "fp1")); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := bs.Idempotency().Complete(ctx, "alice", "/v1/chat/completions", "k1", "run-k1-alice", runtime.RunSucceeded, []byte(`{"replayed":true}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := bs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	bs2, err := store.OpenBolt(path, runtime.SessionLimits{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = bs2.Close() }()
	rec, err := bs2.Idempotency().Lookup(ctx, "alice", "/v1/chat/completions", "k1")
	if err != nil {
		t.Fatalf("Lookup after reopen: %v", err)
	}
	if rec.Status != runtime.IdempotencyCompleted || rec.RunStatus != runtime.RunSucceeded || string(rec.ResponseBody) != `{"replayed":true}` {
		t.Fatalf("record after reopen = %+v, want completed with body", rec)
	}
	// A duplicate Begin after a restart still reports the conflict.
	if _, conflict, err := bs2.Idempotency().Begin(ctx, idemRec("k1", "alice", "fp1")); err != nil || !conflict {
		t.Fatalf("Begin after reopen = (%v, %v), want conflict", err, conflict)
	}
}
