package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sentinel errors for idempotency store operations. Callers should match them
// with errors.Is.
var (
	// ErrIdempotencyNotFound means no live record exists for the
	// caller/endpoint/key triple (or it has expired).
	ErrIdempotencyNotFound = errors.New("runtime: idempotency record not found")
	// ErrIdempotencyMismatch means the idempotency key was reused with a
	// different request fingerprint. The API layer maps it to a 409 conflict.
	ErrIdempotencyMismatch = errors.New("runtime: idempotency key reused with a different request")
	// ErrInvalidIdempotency means the record failed validation (empty
	// key/caller/endpoint/fingerprint or unknown status).
	ErrInvalidIdempotency = errors.New("runtime: invalid idempotency record")
)

// IdempotencyStatus is the lifecycle state of one idempotency record.
type IdempotencyStatus string

const (
	// IdempotencyActive means the first request holding the key is in flight.
	// Duplicates are rejected as in-progress while a record is active.
	IdempotencyActive IdempotencyStatus = "active"
	// IdempotencyCompleted means the first request reached a terminal state.
	// Duplicates replay the stored response body when one was saved, otherwise
	// they are rejected with a status reference — never silently re-run.
	IdempotencyCompleted IdempotencyStatus = "completed"
)

// Valid reports whether s is a known IdempotencyStatus.
func (s IdempotencyStatus) Valid() bool {
	switch s {
	case IdempotencyActive, IdempotencyCompleted:
		return true
	}
	return false
}

// IdempotencyRecord binds one Idempotency-Key (scoped to caller + endpoint) to
// the run that first carried it. ResponseBody holds the exact response bytes
// for replay and is only saved for non-streaming terminal responses; a
// completed record without a body is a status reference, not a replayable
// result.
type IdempotencyRecord struct {
	// Key is the client-supplied Idempotency-Key header value.
	Key string
	// CallerID scopes the key: different callers never observe each other's
	// records even when they pick the same key.
	CallerID string
	// Endpoint scopes the key to one API surface (e.g. "/v1/chat/completions").
	Endpoint string
	// Fingerprint is a stable digest of the semantically relevant request
	// payload; reuse of the key with a different fingerprint is a conflict.
	Fingerprint string

	// RunID is the gateway run the first request was admitted as.
	RunID string
	// Status is active while the first request is in flight and completed once
	// it reaches a terminal state.
	Status IdempotencyStatus
	// RunStatus is the terminal gateway run status recorded at Complete
	// (zero while active). It lets a duplicate reference the outcome — in
	// particular outcome_unknown — without consulting the run store.
	RunStatus RunStatus
	// ResponseBody is the exact response body of the first request, saved only
	// for non-streaming terminal responses (SSE streams are not replayable).
	ResponseBody []byte

	// CreatedAt is stamped by Begin when zero; immutable afterwards.
	CreatedAt time.Time
	// ExpiresAt bounds the key's retention. An expired record is invisible to
	// Lookup and re-occupiable by Begin; Prune deletes it physically.
	ExpiresAt time.Time
}

// IdempotencyStore is the metadata store for Idempotency-Key deduplication.
// The in-memory implementation (NewMemoryIdempotencyStore) is the volatile
// default; the bbolt-backed persistent implementation lives in package store.
// Both satisfy this interface without touching the API or worker layers.
// Every read returns a deep copy (ResponseBody is cloned), and every operation
// is scoped to the caller so one tenant can never observe another's keys.
//
// The store guarantees duplicate ADMISSION prevention and result lookup — not
// exactly-once side effects. A turn whose completion outcome is unknown is
// completed without a body and is never implicitly re-run.
type IdempotencyStore interface {
	// Lookup returns a deep copy of the record for the caller/endpoint/key
	// triple. Fails with ErrIdempotencyNotFound when no live record exists; an
	// expired record is treated as absent.
	Lookup(ctx context.Context, caller, endpoint, key string) (IdempotencyRecord, error)

	// Begin atomically claims the key for the caller/endpoint pair:
	//   - no live record: rec is stored (CreatedAt stamped when zero) and
	//     returned with conflict=false;
	//   - a live record with the SAME fingerprint: the existing record is
	//     returned with conflict=true and rec is NOT stored;
	//   - a live record with a DIFFERENT fingerprint: ErrIdempotencyMismatch;
	//   - an expired record: treated as absent and re-occupied by rec.
	// The check and the insert share one critical section, so exactly one of
	// several concurrent Begin calls for the same key wins admission.
	Begin(ctx context.Context, rec IdempotencyRecord) (existing IdempotencyRecord, conflict bool, err error)

	// Complete marks the record for caller/endpoint/key completed, recording
	// the terminal run status and (for replayable non-streaming responses) the
	// exact response body. Fails with ErrIdempotencyNotFound when no live
	// record exists.
	Complete(ctx context.Context, caller, endpoint, key, runID string, runStatus RunStatus, body []byte) error

	// Delete removes the record for caller/endpoint/key. It is used to release
	// a key whose request was rejected BEFORE any execution was admitted
	// (validation/admission/turn-arbitration failures), so the caller may
	// retry the identical request. Idempotent callers must tolerate
	// ErrIdempotencyNotFound.
	Delete(ctx context.Context, caller, endpoint, key string) error

	// Prune physically deletes every record expired as of now and returns the
	// count removed.
	Prune(ctx context.Context, now time.Time) (int, error)
}

// NewMemoryIdempotencyStore returns the in-memory IdempotencyStore
// implementation. It is safe for concurrent use.
func NewMemoryIdempotencyStore() IdempotencyStore {
	return &memIdempotencyStore{byKey: make(map[string]IdempotencyRecord)}
}

type memIdempotencyStore struct {
	mu    sync.RWMutex
	byKey map[string]IdempotencyRecord
}

// idemScopeKey builds the map key for the caller/endpoint/key triple. The
// length-prefixed encoding keeps the segments unambiguous.
func idemScopeKey(caller, endpoint, key string) string {
	return fmt.Sprintf("%d:%s%d:%s%d:%s", len(caller), caller, len(endpoint), endpoint, len(key), key)
}

// cloneIdempotencyRecord deep-copies rec (ResponseBody is a byte slice).
func cloneIdempotencyRecord(rec IdempotencyRecord) IdempotencyRecord {
	if rec.ResponseBody != nil {
		rec.ResponseBody = append([]byte(nil), rec.ResponseBody...)
	}
	return rec
}

func idemRecordExpired(rec IdempotencyRecord, now time.Time) bool {
	return !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now)
}

func validateIdempotencyRecord(rec IdempotencyRecord) error {
	if rec.Key == "" || rec.CallerID == "" || rec.Endpoint == "" || rec.Fingerprint == "" {
		return fmt.Errorf("%w: key, caller, endpoint and fingerprint are required", ErrInvalidIdempotency)
	}
	if rec.Status == "" {
		return fmt.Errorf("%w: status is required", ErrInvalidIdempotency)
	}
	if !rec.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidIdempotency, rec.Status)
	}
	return nil
}

func (s *memIdempotencyStore) Lookup(_ context.Context, caller, endpoint, key string) (IdempotencyRecord, error) {
	k := idemScopeKey(caller, endpoint, key)
	s.mu.RLock()
	rec, ok := s.byKey[k]
	s.mu.RUnlock()
	if !ok {
		return IdempotencyRecord{}, fmt.Errorf("%w: %q", ErrIdempotencyNotFound, key)
	}
	if idemRecordExpired(rec, time.Now()) {
		s.purgeExpired(k)
		return IdempotencyRecord{}, fmt.Errorf("%w: %q (expired)", ErrIdempotencyNotFound, key)
	}
	return cloneIdempotencyRecord(rec), nil
}

func (s *memIdempotencyStore) Begin(_ context.Context, rec IdempotencyRecord) (IdempotencyRecord, bool, error) {
	if err := validateIdempotencyRecord(rec); err != nil {
		return IdempotencyRecord{}, false, err
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	k := idemScopeKey(rec.CallerID, rec.Endpoint, rec.Key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byKey[k]; ok && !idemRecordExpired(existing, time.Now()) {
		if existing.Fingerprint != rec.Fingerprint {
			return IdempotencyRecord{}, false, fmt.Errorf("%w: key %q", ErrIdempotencyMismatch, rec.Key)
		}
		return cloneIdempotencyRecord(existing), true, nil
	}
	// Absent or expired: claim (or re-claim) the key.
	s.byKey[k] = cloneIdempotencyRecord(rec)
	return cloneIdempotencyRecord(rec), false, nil
}

func (s *memIdempotencyStore) Complete(_ context.Context, caller, endpoint, key, runID string, runStatus RunStatus, body []byte) error {
	k := idemScopeKey(caller, endpoint, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byKey[k]
	if !ok || idemRecordExpired(rec, time.Now()) {
		return fmt.Errorf("%w: %q", ErrIdempotencyNotFound, key)
	}
	rec.Status = IdempotencyCompleted
	rec.RunStatus = runStatus
	if runID != "" {
		rec.RunID = runID
	}
	rec.ResponseBody = append([]byte(nil), body...)
	s.byKey[k] = rec
	return nil
}

func (s *memIdempotencyStore) Delete(_ context.Context, caller, endpoint, key string) error {
	k := idemScopeKey(caller, endpoint, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[k]; !ok {
		return fmt.Errorf("%w: %q", ErrIdempotencyNotFound, key)
	}
	delete(s.byKey, k)
	return nil
}

func (s *memIdempotencyStore) Prune(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for k, rec := range s.byKey {
		if idemRecordExpired(rec, now) {
			delete(s.byKey, k)
			removed++
		}
	}
	return removed, nil
}

// purgeExpired removes an expired record under a write lock, re-checking
// expiry so a concurrent re-occupation is never clobbered.
func (s *memIdempotencyStore) purgeExpired(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byKey[k]; ok && idemRecordExpired(rec, time.Now()) {
		delete(s.byKey, k)
	}
}
