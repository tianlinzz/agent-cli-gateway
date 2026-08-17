// Package store provides the bbolt-backed persistent implementations of
// runtime.SessionStore, runtime.RunStore and runtime.IdempotencyStore. One
// embedded bbolt database file holds all record kinds (buckets "sessions",
// "runs" and "idempotency"); records are JSON (session/run records are pure
// value types; the idempotency record's byte-slice body is detached by the
// JSON round-trip).
//
// The semantics align exactly with the in-memory implementations in runtime/:
// owner scoping, the sentinel errors, lazy expiry on Get, Prune exempting
// active turns, single-turn arbitration, and capacity checked atomically at
// Create (a bbolt write transaction is a single-writer CAS: the check and the
// insert share one Update transaction). Persistence adds restart recovery; it
// does NOT add multi-replica coordination — bbolt allows exactly one writer
// process per file, so cross-node CAS still requires a PostgreSQL/Redis
// backend (documented limitation, ROADMAP §4.1).
//
// Dependency direction: store/ → runtime/ + go.etcd.io/bbolt only.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/tianlinzz/agent-cli-gateway/runtime"
)

var (
	bucketSessions    = []byte("sessions")
	bucketRuns        = []byte("runs")
	bucketIdempotency = []byte("idempotency")
)

// Compile-time contract assertions: BoltStore serves the SessionStore
// interface plus the optional cross-owner enumerators used by startup
// reconciliation. The RunStore half is served by the view from Runs() —
// SessionStore.Create and RunStore.Create collide (Go has no method
// overloading), so one type cannot implement both interfaces directly.
var (
	_ runtime.SessionStore      = (*BoltStore)(nil)
	_ runtime.SessionEnumerator = (*BoltStore)(nil)
)

// BoltStore is the persistent SessionStore + RunStore backed by one bbolt
// database file. It is safe for concurrent use (bbolt serializes writers and
// serves concurrent readers). The caller owns Close.
type BoltStore struct {
	db     *bbolt.DB
	limits runtime.SessionLimits
}

// OpenBolt opens (creating if needed) the bbolt database at path and ensures
// the record buckets exist. The file is created 0600; the caller is
// responsible for creating the parent directory with 0700 permissions.
// limits caps session-record growth exactly like
// runtime.NewCappedMemorySessionStore; zero fields mean unlimited.
func OpenBolt(path string, limits runtime.SessionLimits) (*BoltStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 10 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("store: open bbolt %q: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, bucket := range [][]byte{bucketSessions, bucketRuns, bucketIdempotency} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("store: create bucket %q: %w", bucket, err)
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &BoltStore{db: db, limits: limits}, nil
}

// Close flushes and closes the database file.
func (s *BoltStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close bbolt: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// runtime.SessionStore
// ---------------------------------------------------------------------------

func (s *BoltStore) Create(_ context.Context, rec runtime.SessionRecord) error {
	if rec.ID == "" || rec.CallerID == "" {
		return fmt.Errorf("%w: id and owner are required", runtime.ErrInvalidSession)
	}
	if rec.Status == "" {
		rec.Status = runtime.SessionActive
	}
	if !rec.Status.Valid() {
		return fmt.Errorf("%w: status %q", runtime.ErrInvalidSession, rec.Status)
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encode session %q: %w", rec.ID, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		if b.Get([]byte(rec.ID)) != nil {
			return fmt.Errorf("%w: %q", runtime.ErrSessionExists, rec.ID)
		}
		// Capacity is checked inside the same single-writer transaction as the
		// insert, so the cap is an atomic reservation exactly like the memory
		// store's check-under-lock.
		if s.limits.MaxTotal > 0 || s.limits.MaxPerCaller > 0 {
			total, owned := 0, 0
			c := b.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				total++
				if s.limits.MaxPerCaller > 0 {
					var existing runtime.SessionRecord
					if err := json.Unmarshal(v, &existing); err != nil {
						return fmt.Errorf("store: decode session %q: %w", k, err)
					}
					if existing.CallerID == rec.CallerID {
						owned++
					}
				}
			}
			if s.limits.MaxTotal > 0 && total >= s.limits.MaxTotal {
				return fmt.Errorf("%w: global cap %d reached", runtime.ErrSessionCapacity, s.limits.MaxTotal)
			}
			if s.limits.MaxPerCaller > 0 && owned >= s.limits.MaxPerCaller {
				return fmt.Errorf("%w: per-caller cap %d reached for owner", runtime.ErrSessionCapacity, s.limits.MaxPerCaller)
			}
		}
		if err := b.Put([]byte(rec.ID), data); err != nil {
			return fmt.Errorf("store: put session %q: %w", rec.ID, err)
		}
		return nil
	})
}

func (s *BoltStore) Get(_ context.Context, id, callerID string) (runtime.SessionRecord, error) {
	var rec runtime.SessionRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		var err error
		rec, err = lookupSession(tx.Bucket(bucketSessions), id, callerID)
		return err
	})
	if err != nil {
		return runtime.SessionRecord{}, err
	}
	// An active turn is never expired; an expired non-turn record is purged
	// lazily here (same semantics as the memory store).
	if sessionPrunable(rec, time.Now()) {
		s.purgeExpiredSession(id, callerID)
		return runtime.SessionRecord{}, fmt.Errorf("%w: %q (expired)", runtime.ErrSessionNotFound, id)
	}
	return rec, nil
}

func (s *BoltStore) Update(_ context.Context, id, callerID string, fn func(*runtime.SessionRecord)) (runtime.SessionRecord, error) {
	if fn == nil {
		return runtime.SessionRecord{}, fmt.Errorf("%w: nil update function", runtime.ErrInvalidSession)
	}
	var out runtime.SessionRecord
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		rec, err := lookupSession(b, id, callerID)
		if err != nil {
			return err
		}
		created := rec.CreatedAt
		fn(&rec)
		// CreatedAt is immutable; UpdatedAt is stamped by the store.
		rec.CreatedAt = created
		rec.UpdatedAt = time.Now()
		if err := putSession(b, rec); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if err != nil {
		return runtime.SessionRecord{}, err
	}
	return out, nil
}

func (s *BoltStore) List(_ context.Context, callerID string) ([]runtime.SessionRecord, error) {
	now := time.Now()
	out := make([]runtime.SessionRecord, 0)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketSessions).ForEach(func(k, v []byte) error {
			var rec runtime.SessionRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("store: decode session %q: %w", k, err)
			}
			if rec.CallerID != callerID || sessionExpired(rec, now) {
				return nil
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sortSessions(out)
	return out, nil
}

func (s *BoltStore) BeginTurn(_ context.Context, id, callerID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		rec, err := lookupSession(b, id, callerID)
		if err != nil {
			return err
		}
		switch rec.Status {
		case runtime.SessionTurnActive:
			return fmt.Errorf("%w: session %q", runtime.ErrSessionBusy, id)
		case runtime.SessionClosing, runtime.SessionClosed:
			return fmt.Errorf("%w: session %q is %q", runtime.ErrSessionState, id, rec.Status)
		}
		rec.Status = runtime.SessionTurnActive
		rec.UpdatedAt = time.Now()
		return putSession(b, rec)
	})
}

func (s *BoltStore) EndTurn(_ context.Context, id, callerID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		rec, err := lookupSession(b, id, callerID)
		if err != nil {
			return err
		}
		if rec.Status != runtime.SessionTurnActive {
			return fmt.Errorf("%w: session %q is %q, want %q", runtime.ErrSessionState, id, rec.Status, runtime.SessionTurnActive)
		}
		rec.Status = runtime.SessionActive
		rec.UpdatedAt = time.Now()
		return putSession(b, rec)
	})
}

func (s *BoltStore) Touch(_ context.Context, id, callerID string, ttl time.Duration) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		rec, err := lookupSession(b, id, callerID)
		if err != nil {
			return err
		}
		rec.UpdatedAt = time.Now()
		if ttl > 0 {
			rec.ExpiresAt = rec.UpdatedAt.Add(ttl)
		}
		return putSession(b, rec)
	})
}

func (s *BoltStore) Delete(_ context.Context, id, callerID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		if _, err := lookupSession(b, id, callerID); err != nil {
			return err
		}
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("store: delete session %q: %w", id, err)
		}
		return nil
	})
}

func (s *BoltStore) Prune(_ context.Context, now time.Time) ([]string, error) {
	var removed []string
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		// Collect first: deleting while a cursor is positioned on the deleted
		// key is legal in bbolt, but collecting keeps the two phases explicit.
		var ids [][]byte
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec runtime.SessionRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("store: decode session %q: %w", k, err)
			}
			// An expired record is removable only if no turn is in flight on it
			// (active turns are exempt regardless of TTL).
			if sessionPrunable(rec, now) {
				ids = append(ids, append([]byte(nil), k...))
			}
		}
		for _, k := range ids {
			if err := b.Delete(k); err != nil {
				return fmt.Errorf("store: prune session %q: %w", k, err)
			}
			removed = append(removed, string(k))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// ListAllSessions implements runtime.SessionEnumerator for the trusted
// startup reconcile path; it bypasses owner scoping and expiry filtering.
func (s *BoltStore) ListAllSessions(_ context.Context) ([]runtime.SessionRecord, error) {
	out := make([]runtime.SessionRecord, 0)
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketSessions).ForEach(func(k, v []byte) error {
			var rec runtime.SessionRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("store: decode session %q: %w", k, err)
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lookupSession resolves and owner-checks a session within a transaction.
func lookupSession(b *bbolt.Bucket, id, callerID string) (runtime.SessionRecord, error) {
	v := b.Get([]byte(id))
	if v == nil {
		return runtime.SessionRecord{}, fmt.Errorf("%w: %q", runtime.ErrSessionNotFound, id)
	}
	var rec runtime.SessionRecord
	if err := json.Unmarshal(v, &rec); err != nil {
		return runtime.SessionRecord{}, fmt.Errorf("store: decode session %q: %w", id, err)
	}
	if rec.CallerID != callerID {
		return runtime.SessionRecord{}, fmt.Errorf("%w: session %q", runtime.ErrSessionForbidden, id)
	}
	return rec, nil
}

func putSession(b *bbolt.Bucket, rec runtime.SessionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encode session %q: %w", rec.ID, err)
	}
	if err := b.Put([]byte(rec.ID), data); err != nil {
		return fmt.Errorf("store: put session %q: %w", rec.ID, err)
	}
	return nil
}

// purgeExpiredSession removes an expired session, re-checking identity,
// expiry, and that no turn is in flight so a concurrent create/touch or an
// active turn is never clobbered.
func (s *BoltStore) purgeExpiredSession(id, callerID string) {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSessions)
		rec, err := lookupSession(b, id, callerID)
		if err != nil || !sessionPrunable(rec, time.Now()) {
			return nil // gone, re-owned, or revived: nothing to purge
		}
		return b.Delete([]byte(id))
	})
	if err != nil {
		slog.Warn("store: purge expired session", "id", id, "error", err)
	}
}

// sessionExpired mirrors the memory store's expiry rule: a non-zero ExpiresAt
// that has passed.
func sessionExpired(rec runtime.SessionRecord, now time.Time) bool {
	return !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now)
}

// sessionPrunable mirrors the memory store's prune rule: expired and no turn
// in flight.
func sessionPrunable(rec runtime.SessionRecord, now time.Time) bool {
	return sessionExpired(rec, now) && rec.Status != runtime.SessionTurnActive
}

func sortSessions(recs []runtime.SessionRecord) {
	sort.Slice(recs, func(i, j int) bool {
		if !recs[i].CreatedAt.Equal(recs[j].CreatedAt) {
			return recs[i].CreatedAt.Before(recs[j].CreatedAt)
		}
		return recs[i].ID < recs[j].ID
	})
}

// ---------------------------------------------------------------------------
// runtime.RunStore
// ---------------------------------------------------------------------------

// Runs returns this store's runtime.RunStore view over the same underlying
// database (bucket "runs").
func (s *BoltStore) Runs() runtime.RunStore { return runStoreView{s} }

// runStoreView adapts BoltStore to runtime.RunStore.
type runStoreView struct{ s *BoltStore }

var (
	_ runtime.RunStore      = runStoreView{}
	_ runtime.RunEnumerator = runStoreView{}
)

func (v runStoreView) Create(_ context.Context, rec runtime.RunRecord) error {
	return v.s.createRun(rec)
}

func (s *BoltStore) createRun(rec runtime.RunRecord) error {
	if rec.ID == "" {
		return fmt.Errorf("%w: id is required", runtime.ErrInvalidRun)
	}
	if rec.Status != "" && !rec.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", runtime.ErrInvalidRun, rec.Status)
	}
	if rec.Status == "" {
		rec.Status = runtime.RunStarting
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now()
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: encode run %q: %w", rec.ID, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		if b.Get([]byte(rec.ID)) != nil {
			return fmt.Errorf("%w: %s", runtime.ErrRunExists, rec.ID)
		}
		if err := b.Put([]byte(rec.ID), data); err != nil {
			return fmt.Errorf("store: put run %q: %w", rec.ID, err)
		}
		return nil
	})
}

func (v runStoreView) Get(_ context.Context, id string) (runtime.RunRecord, error) {
	var rec runtime.RunRecord
	err := v.s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketRuns).Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("%w: %s", runtime.ErrRunNotFound, id)
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("store: decode run %q: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return rec, nil
}

func (v runStoreView) Update(_ context.Context, id string, fn func(*runtime.RunRecord)) (runtime.RunRecord, error) {
	var out runtime.RunRecord
	err := v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		raw := b.Get([]byte(id))
		if raw == nil {
			return fmt.Errorf("%w: %s", runtime.ErrRunNotFound, id)
		}
		var rec runtime.RunRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("store: decode run %q: %w", id, err)
		}
		fn(&rec)
		if rec.Status != "" && !rec.Status.Valid() {
			return fmt.Errorf("%w: unknown status %q", runtime.ErrInvalidRun, rec.Status)
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("store: encode run %q: %w", rec.ID, err)
		}
		if err := b.Put([]byte(id), data); err != nil {
			return fmt.Errorf("store: put run %q: %w", id, err)
		}
		out = rec
		return nil
	})
	if err != nil {
		return runtime.RunRecord{}, err
	}
	return out, nil
}

func (v runStoreView) ListBySession(_ context.Context, sessionID string) ([]runtime.RunRecord, error) {
	out := make([]runtime.RunRecord, 0)
	err := v.s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketRuns).ForEach(func(k, raw []byte) error {
			var rec runtime.RunRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("store: decode run %q: %w", k, err)
			}
			if rec.SessionID == sessionID {
				out = append(out, rec)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (v runStoreView) Delete(_ context.Context, id string) error {
	return v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		if b.Get([]byte(id)) == nil {
			return fmt.Errorf("%w: %s", runtime.ErrRunNotFound, id)
		}
		if err := b.Delete([]byte(id)); err != nil {
			return fmt.Errorf("store: delete run %q: %w", id, err)
		}
		return nil
	})
}

func (v runStoreView) Prune(_ context.Context, now time.Time, maxAge time.Duration) (int, error) {
	deadline := now.Add(-maxAge)
	removed := 0
	err := v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		var ids [][]byte
		c := b.Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			var rec runtime.RunRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("store: decode run %q: %w", k, err)
			}
			// Non-terminal runs are never pruned.
			if rec.Status.Terminal() && rec.FinishedAt.Before(deadline) {
				ids = append(ids, append([]byte(nil), k...))
			}
		}
		for _, k := range ids {
			if err := b.Delete(k); err != nil {
				return fmt.Errorf("store: prune run %q: %w", k, err)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// ListAllRuns implements runtime.RunEnumerator for the trusted startup
// reconcile path.
func (v runStoreView) ListAllRuns(_ context.Context) ([]runtime.RunRecord, error) {
	out := make([]runtime.RunRecord, 0)
	err := v.s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketRuns).ForEach(func(k, raw []byte) error {
			var rec runtime.RunRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("store: decode run %q: %w", k, err)
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// runtime.IdempotencyStore
// ---------------------------------------------------------------------------

// Idempotency returns this store's runtime.IdempotencyStore view over the same
// underlying database (bucket "idempotency").
func (s *BoltStore) Idempotency() runtime.IdempotencyStore { return idempotencyStoreView{s} }

// idempotencyStoreView adapts BoltStore to runtime.IdempotencyStore.
type idempotencyStoreView struct{ s *BoltStore }

var _ runtime.IdempotencyStore = idempotencyStoreView{}

// idemBoltKey builds the bbolt key for the caller/endpoint/key triple. The
// length-prefixed encoding keeps the segments unambiguous (mirrors the memory
// store's scope key).
func idemBoltKey(caller, endpoint, key string) string {
	return fmt.Sprintf("%d:%s%d:%s%d:%s", len(caller), caller, len(endpoint), endpoint, len(key), key)
}

func idemExpired(rec runtime.IdempotencyRecord, now time.Time) bool {
	return !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now)
}

func (v idempotencyStoreView) Lookup(_ context.Context, caller, endpoint, key string) (runtime.IdempotencyRecord, error) {
	bk := idemBoltKey(caller, endpoint, key)
	var rec runtime.IdempotencyRecord
	err := v.s.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketIdempotency).Get([]byte(bk))
		if raw == nil {
			return fmt.Errorf("%w: %q", runtime.ErrIdempotencyNotFound, key)
		}
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("store: decode idempotency record %q: %w", key, err)
		}
		return nil
	})
	if err != nil {
		return runtime.IdempotencyRecord{}, err
	}
	if idemExpired(rec, time.Now()) {
		v.purgeExpiredIdem(bk)
		return runtime.IdempotencyRecord{}, fmt.Errorf("%w: %q (expired)", runtime.ErrIdempotencyNotFound, key)
	}
	return rec, nil
}

func (v idempotencyStoreView) Begin(_ context.Context, rec runtime.IdempotencyRecord) (runtime.IdempotencyRecord, bool, error) {
	if rec.Key == "" || rec.CallerID == "" || rec.Endpoint == "" || rec.Fingerprint == "" {
		return runtime.IdempotencyRecord{}, false, fmt.Errorf("%w: key, caller, endpoint and fingerprint are required", runtime.ErrInvalidIdempotency)
	}
	if rec.Status == "" {
		return runtime.IdempotencyRecord{}, false, fmt.Errorf("%w: status is required", runtime.ErrInvalidIdempotency)
	}
	if !rec.Status.Valid() {
		return runtime.IdempotencyRecord{}, false, fmt.Errorf("%w: unknown status %q", runtime.ErrInvalidIdempotency, rec.Status)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	bk := idemBoltKey(rec.CallerID, rec.Endpoint, rec.Key)
	var out runtime.IdempotencyRecord
	conflict := false
	err := v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIdempotency)
		if raw := b.Get([]byte(bk)); raw != nil {
			var existing runtime.IdempotencyRecord
			if err := json.Unmarshal(raw, &existing); err != nil {
				return fmt.Errorf("store: decode idempotency record %q: %w", rec.Key, err)
			}
			if !idemExpired(existing, time.Now()) {
				if existing.Fingerprint != rec.Fingerprint {
					return fmt.Errorf("%w: key %q", runtime.ErrIdempotencyMismatch, rec.Key)
				}
				out = existing
				conflict = true
				return nil
			}
			// Expired: fall through and re-occupy the key.
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("store: encode idempotency record %q: %w", rec.Key, err)
		}
		if err := b.Put([]byte(bk), data); err != nil {
			return fmt.Errorf("store: put idempotency record %q: %w", rec.Key, err)
		}
		out = rec
		return nil
	})
	if err != nil {
		return runtime.IdempotencyRecord{}, false, err
	}
	// JSON round-trips already detach ResponseBody from any caller-owned slice.
	return out, conflict, nil
}

func (v idempotencyStoreView) Complete(_ context.Context, caller, endpoint, key, runID string, runStatus runtime.RunStatus, body []byte) error {
	bk := idemBoltKey(caller, endpoint, key)
	return v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIdempotency)
		raw := b.Get([]byte(bk))
		if raw == nil {
			return fmt.Errorf("%w: %q", runtime.ErrIdempotencyNotFound, key)
		}
		var rec runtime.IdempotencyRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("store: decode idempotency record %q: %w", key, err)
		}
		if idemExpired(rec, time.Now()) {
			return fmt.Errorf("%w: %q (expired)", runtime.ErrIdempotencyNotFound, key)
		}
		rec.Status = runtime.IdempotencyCompleted
		rec.RunStatus = runStatus
		if runID != "" {
			rec.RunID = runID
		}
		rec.ResponseBody = append([]byte(nil), body...)
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("store: encode idempotency record %q: %w", key, err)
		}
		if err := b.Put([]byte(bk), data); err != nil {
			return fmt.Errorf("store: put idempotency record %q: %w", key, err)
		}
		return nil
	})
}

func (v idempotencyStoreView) Delete(_ context.Context, caller, endpoint, key string) error {
	bk := idemBoltKey(caller, endpoint, key)
	return v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIdempotency)
		if b.Get([]byte(bk)) == nil {
			return fmt.Errorf("%w: %q", runtime.ErrIdempotencyNotFound, key)
		}
		if err := b.Delete([]byte(bk)); err != nil {
			return fmt.Errorf("store: delete idempotency record %q: %w", key, err)
		}
		return nil
	})
}

func (v idempotencyStoreView) Prune(_ context.Context, now time.Time) (int, error) {
	removed := 0
	err := v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIdempotency)
		var ids [][]byte
		c := b.Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			var rec runtime.IdempotencyRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return fmt.Errorf("store: decode idempotency record %q: %w", k, err)
			}
			if idemExpired(rec, now) {
				ids = append(ids, append([]byte(nil), k...))
			}
		}
		for _, k := range ids {
			if err := b.Delete(k); err != nil {
				return fmt.Errorf("store: prune idempotency record %q: %w", k, err)
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// purgeExpiredIdem removes an expired record, re-checking expiry inside the
// write transaction so a concurrent re-occupation is never clobbered.
func (v idempotencyStoreView) purgeExpiredIdem(bk string) {
	err := v.s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIdempotency)
		raw := b.Get([]byte(bk))
		if raw == nil {
			return nil
		}
		var rec runtime.IdempotencyRecord
		if err := json.Unmarshal(raw, &rec); err != nil || !idemExpired(rec, time.Now()) {
			return nil // corrupt or re-occupied: leave it alone
		}
		return b.Delete([]byte(bk))
	})
	if err != nil {
		slog.Warn("store: purge expired idempotency record", "error", err)
	}
}
