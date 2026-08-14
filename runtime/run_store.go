package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Sentinel errors for run store operations.
var (
	// ErrRunNotFound means the run ID does not exist.
	ErrRunNotFound = errors.New("runtime: run not found")
	// ErrRunExists means a run with the same ID is already stored.
	ErrRunExists = errors.New("runtime: run already exists")
	// ErrInvalidRun means the record failed validation (empty ID or unknown status).
	ErrInvalidRun = errors.New("runtime: invalid run record")
)

// RunStore is the metadata store for gateway runs (individual Agent turns).
// The in-memory implementation (NewMemoryRunStore) is the phase-2 store; a
// future persistent store (Redis/DB) replaces this interface without touching
// the API or worker layers. Every read returns a deep copy.
type RunStore interface {
	// Create stores a new run record. Fails with ErrRunExists if the ID is
	// already taken and ErrInvalidRun if the record is malformed.
	// StartedAt is stamped to now when zero.
	Create(ctx context.Context, rec RunRecord) error

	// Get returns a deep copy of the run. Fails with ErrRunNotFound if it does
	// not exist.
	Get(ctx context.Context, id string) (RunRecord, error)

	// Update applies fn to a deep copy of the run and persists it. StartedAt is
	// preserved. Returns the updated copy. Fails with ErrRunNotFound if the run
	// does not exist.
	Update(ctx context.Context, id string, fn func(*RunRecord)) (RunRecord, error)

	// ListBySession returns deep copies of all runs for a session, ordered by
	// StartedAt (ties broken by ID).
	ListBySession(ctx context.Context, sessionID string) ([]RunRecord, error)

	// Delete removes the run record. Idempotent callers must tolerate
	// ErrRunNotFound for already-deleted runs.
	Delete(ctx context.Context, id string) error

	// Prune deletes every run that reached a terminal status before maxAge and
	// returns the count removed. Non-terminal runs are never pruned.
	Prune(ctx context.Context, now time.Time, maxAge time.Duration) (int, error)
}

// NewMemoryRunStore returns the in-memory RunStore implementation.
func NewMemoryRunStore() RunStore {
	return &memRunStore{byID: make(map[string]RunRecord)}
}

type memRunStore struct {
	mu   sync.RWMutex
	byID map[string]RunRecord
}

func (s *memRunStore) Create(_ context.Context, rec RunRecord) error {
	if rec.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidRun)
	}
	if rec.Status != "" && !rec.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidRun, rec.Status)
	}
	if rec.Status == "" {
		rec.Status = RunStarting
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[rec.ID]; exists {
		return fmt.Errorf("%w: %s", ErrRunExists, rec.ID)
	}
	s.byID[rec.ID] = rec
	return nil
}

func (s *memRunStore) Get(_ context.Context, id string) (RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byID[id]
	if !ok {
		return RunRecord{}, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	return rec, nil // value copy
}

func (s *memRunStore) Update(_ context.Context, id string, fn func(*RunRecord)) (RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return RunRecord{}, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	fn(&rec)
	if rec.Status != "" && !rec.Status.Valid() {
		return RunRecord{}, fmt.Errorf("%w: unknown status %q", ErrInvalidRun, rec.Status)
	}
	s.byID[id] = rec
	return rec, nil
}

func (s *memRunStore) ListBySession(_ context.Context, sessionID string) ([]RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []RunRecord
	for _, rec := range s.byID {
		if rec.SessionID == sessionID {
			result = append(result, rec)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].StartedAt.Equal(result[j].StartedAt) {
			return result[i].StartedAt.Before(result[j].StartedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}

func (s *memRunStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	delete(s.byID, id)
	return nil
}

func (s *memRunStore) Prune(_ context.Context, now time.Time, maxAge time.Duration) (int, error) {
	deadline := now.Add(-maxAge)
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, rec := range s.byID {
		if rec.Status.Terminal() && rec.FinishedAt.Before(deadline) {
			delete(s.byID, id)
			removed++
		}
	}
	return removed, nil
}
