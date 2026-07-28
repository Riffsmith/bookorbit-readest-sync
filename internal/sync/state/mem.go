package state

import (
	"sync"

	"github.com/user/bookorbit-readest-sync/internal/token"
)

// MemStore is an in-memory Store for tests and ephemeral runs. It satisfies
// the same interface as FileStore but keeps everything in RAM and never
// touches disk; Load and Save are no-ops.
type MemStore struct {
	mu   sync.Mutex
	data Data
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{data: NewData()}
}

// Load is a no-op for the in-memory store.
func (s *MemStore) Load() error { return nil }

// Save is a no-op for the in-memory store.
func (s *MemStore) Save() error { return nil }

// Token returns the stored token set.
func (s *MemStore) Token() token.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Token
}

// SetToken records a new token set.
func (s *MemStore) SetToken(t token.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Token = t
}

// DeviceID returns the stable device identity, generating one via genID on
// first use.
func (s *MemStore) DeviceID(genID func() string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.DeviceID == "" && genID != nil {
		s.data.DeviceID = genID()
	}
	return s.data.DeviceID
}

// Watermark returns the pull cursor in epoch milliseconds.
func (s *MemStore) Watermark() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.WatermarkMs
}

// SetWatermark records a new pull cursor.
func (s *MemStore) SetWatermark(ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.WatermarkMs = ms
}

// Match returns the cached resolution for a hash, or ErrNotFound.
func (s *MemStore) Match(hash string) (MatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data.Matches[hash]
	if !ok {
		return MatchRecord{}, ErrNotFound
	}
	return rec, nil
}

// SetMatch records or updates a hash's resolution.
func (s *MemStore) SetMatch(hash string, rec MatchRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Matches[hash] = rec
}

// DeleteMatch removes a cached resolution.
func (s *MemStore) DeleteMatch(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Matches, hash)
}

// UnmatchedAt returns the last failed match-check time for a hash and whether
// it is in the unmatched set.
func (s *MemStore) UnmatchedAt(hash string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.data.Unmatched[hash]
	return at, ok
}

// SetUnmatched records a failed match-check at the given time.
func (s *MemStore) SetUnmatched(hash string, at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Unmatched[hash] = at
}

// ClearUnmatched removes a hash from the unmatched set.
func (s *MemStore) ClearUnmatched(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Unmatched, hash)
}
