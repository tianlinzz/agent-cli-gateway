package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Sentinel errors for session store operations. Callers should match them with
// errors.Is. ErrSessionForbidden is deliberately distinct from
// ErrSessionNotFound, but that distinct error is itself an existence oracle: a
// wrong-owner probe learns the session exists. The API layer MUST collapse both
// into one generic not-authorized/not-found response externally.
var (
	// ErrSessionNotFound means the session ID does not exist (or is expired).
	ErrSessionNotFound = errors.New("runtime: session not found")
	// ErrSessionExists means a session with the same ID is already stored.
	ErrSessionExists = errors.New("runtime: session already exists")
	// ErrSessionForbidden means the requester is not the session owner.
	ErrSessionForbidden = errors.New("runtime: session access denied")
	// ErrSessionBusy means a turn is already active on the session.
	ErrSessionBusy = errors.New("runtime: session already has an active turn")
	// ErrSessionState means the session lifecycle state rejects the operation
	// (e.g. BeginTurn on a closing/closed session).
	ErrSessionState = errors.New("runtime: session state does not allow this operation")
	// ErrInvalidSession means the record failed validation (empty ID/owner or
	// unknown status).
	ErrInvalidSession = errors.New("runtime: invalid session record")
	// ErrSessionCapacity means the store's configured session-record capacity
	// (global or per-caller) is exhausted. The API layer maps it to a 429
	// overload response (O-F10). Enforcing the cap inside Create makes the
	// check-and-insert atomic under concurrency.
	ErrSessionCapacity = errors.New("runtime: session capacity exceeded")
)

// SessionStore is the metadata store for gateway sessions. The in-memory
// implementation (NewMemorySessionStore) is the phase-1 store; a future
// PersistentSessionStore (Redis/DB) replaces this interface without touching
// the API or worker layers. Every read returns a deep copy, and every
// operation is scoped to the owner so one tenant can never observe another's
// sessions.
type SessionStore interface {
	// Create stores a new session record. Fails with ErrSessionExists if the
	// ID is already taken, ErrInvalidSession if the record is malformed, and
	// ErrSessionCapacity when a configured global/per-caller record cap is
	// exhausted (the check and the insert are atomic). CreatedAt/UpdatedAt
	// are stamped to now when zero.
	Create(ctx context.Context, rec SessionRecord) error

	// Get returns a deep copy of the session, scoped to callerID. Fails with
	// ErrSessionForbidden if the session belongs to a different owner and
	// ErrSessionNotFound if it does not exist (or is expired).
	Get(ctx context.Context, id, callerID string) (SessionRecord, error)

	// Update applies fn to a deep copy of the session and persists it,
	// scoped to callerID. CreatedAt is preserved; UpdatedAt is stamped to now.
	// Returns the updated copy.
	Update(ctx context.Context, id, callerID string, fn func(*SessionRecord)) (SessionRecord, error)

	// List returns deep copies of all non-expired sessions owned by callerID,
	// ordered by CreatedAt (ties broken by ID) for a deterministic result.
	List(ctx context.Context, callerID string) ([]SessionRecord, error)

	// BeginTurn marks a single active turn on the session. It fails with
	// ErrSessionBusy if a turn is already active and ErrSessionState if the
	// session is closing/closed. Exactly one concurrent BeginTurn wins.
	BeginTurn(ctx context.Context, id, callerID string) error

	// EndTurn clears the active-turn flag (back to SessionActive) and touches
	// UpdatedAt. It fails if the session has no active turn.
	EndTurn(ctx context.Context, id, callerID string) error

	// Touch stamps UpdatedAt and, when ttl > 0, extends ExpiresAt to now+ttl.
	Touch(ctx context.Context, id, callerID string, ttl time.Duration) error

	// Delete removes the session record. Idempotent callers must tolerate
	// ErrSessionNotFound for already-deleted sessions.
	Delete(ctx context.Context, id, callerID string) error

	// Prune physically deletes every expired session as of now and returns the
	// ids of the sessions that were removed (in arbitrary order). The ids are
	// returned to the trusted gateway layer so it can drop any cached handles
	// for the evicted sessions; they carry no cross-tenant information beyond
	// what the gateway already owns.
	Prune(ctx context.Context, now time.Time) ([]string, error)
}

// NewMemorySessionStore returns the in-memory SessionStore implementation. It
// is safe for concurrent use and satisfied by a mutex-guarded map; no
// persistent dependency (Redis/DB) is introduced.
func NewMemorySessionStore() SessionStore {
	return &memSessionStore{byID: make(map[string]SessionRecord)}
}

// SessionLimits caps session-record growth (the Phase 0-deferred, Phase
// 2-required session record cap). Zero-valued fields mean unlimited. The caps
// are enforced atomically inside Create — the check and the insert share one
// lock, so concurrent session creations can never oversubscribe.
type SessionLimits struct {
	// MaxTotal caps the number of live session records across all callers.
	MaxTotal int
	// MaxPerCaller caps the number of live session records one caller may own.
	MaxPerCaller int
}

// NewCappedMemorySessionStore returns the in-memory SessionStore with
// capacity limits enforced at Create time (see SessionLimits).
func NewCappedMemorySessionStore(limits SessionLimits) SessionStore {
	return &memSessionStore{
		byID:   make(map[string]SessionRecord),
		limits: limits,
	}
}

type memSessionStore struct {
	mu     sync.RWMutex
	byID   map[string]SessionRecord
	limits SessionLimits // enforced in Create; zero = unlimited
}

func (s *memSessionStore) Create(ctx context.Context, rec SessionRecord) error {
	if rec.ID == "" || rec.CallerID == "" {
		return fmt.Errorf("%w: id and owner are required", ErrInvalidSession)
	}
	if rec.Status == "" {
		rec.Status = SessionActive
	}
	if !rec.Status.Valid() {
		return fmt.Errorf("%w: status %q", ErrInvalidSession, rec.Status)
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[rec.ID]; ok {
		return fmt.Errorf("%w: %q", ErrSessionExists, rec.ID)
	}
	// Capacity is checked under the same lock as the insert, so the cap is an
	// atomic reservation: concurrent Create calls each observe the committed
	// count, never an in-flight one.
	if s.limits.MaxTotal > 0 && len(s.byID) >= s.limits.MaxTotal {
		return fmt.Errorf("%w: global cap %d reached", ErrSessionCapacity, s.limits.MaxTotal)
	}
	if s.limits.MaxPerCaller > 0 {
		owned := 0
		for _, existing := range s.byID {
			if existing.CallerID == rec.CallerID {
				owned++
			}
		}
		if owned >= s.limits.MaxPerCaller {
			return fmt.Errorf("%w: per-caller cap %d reached for owner", ErrSessionCapacity, s.limits.MaxPerCaller)
		}
	}
	// rec is all value types, so the assignment is already a deep copy.
	s.byID[rec.ID] = rec
	return nil
}

func (s *memSessionStore) Get(ctx context.Context, id, callerID string) (SessionRecord, error) {
	s.mu.RLock()
	rec, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return SessionRecord{}, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	if rec.CallerID != callerID {
		return SessionRecord{}, fmt.Errorf("%w: session %q", ErrSessionForbidden, id)
	}
	// An active turn is never expired: its record may be read while the turn
	// runs even if ExpiresAt has passed (the TTL is refreshed at turn start,
	// but a turn longer than the TTL is still in flight and must not vanish).
	if prunable(rec, time.Now()) {
		s.purgeExpired(id, callerID)
		return SessionRecord{}, fmt.Errorf("%w: %q (expired)", ErrSessionNotFound, id)
	}
	return rec, nil
}

func (s *memSessionStore) Update(ctx context.Context, id, callerID string, fn func(*SessionRecord)) (SessionRecord, error) {
	if fn == nil {
		return SessionRecord{}, fmt.Errorf("%w: nil update function", ErrInvalidSession)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.lookupLocked(id, callerID)
	if err != nil {
		return SessionRecord{}, err
	}
	created := rec.CreatedAt
	fn(&rec)
	// CreatedAt is immutable; UpdatedAt is stamped by the store, not the caller.
	rec.CreatedAt = created
	rec.UpdatedAt = time.Now()
	s.byID[id] = rec
	return rec, nil
}

func (s *memSessionStore) List(ctx context.Context, callerID string) ([]SessionRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	out := make([]SessionRecord, 0, len(s.byID))
	for _, rec := range s.byID {
		if rec.CallerID != callerID {
			continue
		}
		if expired(rec, now) {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *memSessionStore) BeginTurn(ctx context.Context, id, callerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.lookupLocked(id, callerID)
	if err != nil {
		return err
	}
	switch rec.Status {
	case SessionTurnActive:
		return fmt.Errorf("%w: session %q", ErrSessionBusy, id)
	case SessionClosing, SessionClosed:
		return fmt.Errorf("%w: session %q is %q", ErrSessionState, id, rec.Status)
	}
	rec.Status = SessionTurnActive
	rec.UpdatedAt = time.Now()
	s.byID[id] = rec
	return nil
}

func (s *memSessionStore) EndTurn(ctx context.Context, id, callerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.lookupLocked(id, callerID)
	if err != nil {
		return err
	}
	if rec.Status != SessionTurnActive {
		return fmt.Errorf("%w: session %q is %q, want %q", ErrSessionState, id, rec.Status, SessionTurnActive)
	}
	rec.Status = SessionActive
	rec.UpdatedAt = time.Now()
	s.byID[id] = rec
	return nil
}

func (s *memSessionStore) Touch(ctx context.Context, id, callerID string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.lookupLocked(id, callerID)
	if err != nil {
		return err
	}
	rec.UpdatedAt = time.Now()
	if ttl > 0 {
		rec.ExpiresAt = rec.UpdatedAt.Add(ttl)
	}
	s.byID[id] = rec
	return nil
}

func (s *memSessionStore) Delete(ctx context.Context, id, callerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.lookupLocked(id, callerID); err != nil {
		return err
	}
	delete(s.byID, id)
	return nil
}

func (s *memSessionStore) Prune(ctx context.Context, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []string
	for id, rec := range s.byID {
		// An expired record is removable only if no turn is in flight on it:
		// pruning an active turn would delete its metadata and (at the API
		// layer) close the handle of a turn that is still executing. Active
		// turns are exempt regardless of TTL; they are re-evaluated once the
		// turn ends and the record becomes SessionActive again.
		if !prunable(rec, now) {
			continue
		}
		delete(s.byID, id)
		removed = append(removed, id)
	}
	return removed, nil
}

// lookupLocked resolves and owner-checks a session. The caller must hold s.mu.
func (s *memSessionStore) lookupLocked(id, callerID string) (SessionRecord, error) {
	rec, ok := s.byID[id]
	if !ok {
		return SessionRecord{}, fmt.Errorf("%w: %q", ErrSessionNotFound, id)
	}
	if rec.CallerID != callerID {
		return SessionRecord{}, fmt.Errorf("%w: session %q", ErrSessionForbidden, id)
	}
	return rec, nil
}

// purgeExpired removes an expired session under a write lock, re-checking
// identity, expiry, and that no turn is in flight so a concurrent create/touch
// or an active turn is never clobbered.
func (s *memSessionStore) purgeExpired(id, callerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.byID[id]; ok && rec.CallerID == callerID && prunable(rec, time.Now()) {
		delete(s.byID, id)
	}
}

// expired reports whether rec has a non-zero ExpiresAt that has passed.
func expired(rec SessionRecord, now time.Time) bool {
	return !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(now)
}

// prunable reports whether rec is safe to physically remove now: it must be
// expired and must not have a turn in flight.
func prunable(rec SessionRecord, now time.Time) bool {
	return expired(rec, now) && rec.Status != SessionTurnActive
}
