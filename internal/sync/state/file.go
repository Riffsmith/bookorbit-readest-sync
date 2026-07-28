package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/user/bookorbit-readest-sync/internal/token"
)

// filePerm is the on-disk permission for the state file. It holds credentials
// (Supabase tokens, derived device identity), so it is owner-read/write only.
const filePerm = 0o600

// FileStore persists Data to a JSON file. Saves are atomic: the payload is
// written to a temporary file in the same directory and renamed over the
// target, so a crash mid-save cannot leave a half-written state file.
type FileStore struct {
	path string
	mu   sync.Mutex
	data Data
}

// NewFileStore creates a FileStore at the given path. It does not touch the
// disk until Load or Save is called.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, data: NewData()}
}

// Path returns the file path backing this store.
func (s *FileStore) Path() string {
	return s.path
}

// Load reads the state file. A missing file is not an error — it leaves the
// store at its empty initial state, which is the normal first-run condition.
// A file that exists but cannot be parsed returns an error.
func (s *FileStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.data = NewData()
			return nil
		}
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}

	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("state: parse %s: %w", s.path, err)
	}
	// Re-normalize nil maps so setters never panic on a partially-written file.
	if d.Matches == nil {
		d.Matches = make(map[string]MatchRecord)
	}
	if d.Unmatched == nil {
		d.Unmatched = make(map[string]int64)
	}
	s.data = d
	return nil
}

// Save writes the current state atomically with 0600 permissions. It creates
// the parent directory if needed.
func (s *FileStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveLocked performs the atomic write. The caller must hold s.mu.
func (s *FileStore) saveLocked() error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("state: mkdir %s: %w", dir, err)
		}
	}

	payload, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".bridge-state-*.tmp")
	if err != nil {
		return fmt.Errorf("state: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail out before the rename.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: chmod temp: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close temp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("state: rename over %s: %w", s.path, err)
	}
	// Ensure the final file carries the intended permissions even if a prior
	// file existed with different ones.
	if err := os.Chmod(s.path, filePerm); err != nil {
		return fmt.Errorf("state: chmod %s: %w", s.path, err)
	}
	return nil
}

// Token returns the persisted Supabase token set.
func (s *FileStore) Token() token.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Token
}

// SetToken records a new token set.
func (s *FileStore) SetToken(t token.Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Token = t
}

// DeviceID returns the stable device identity, generating and recording one
// via genID on first use.
func (s *FileStore) DeviceID(genID func() string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.DeviceID == "" && genID != nil {
		s.data.DeviceID = genID()
	}
	return s.data.DeviceID
}

// Watermark returns the Readest pull cursor in epoch milliseconds.
func (s *FileStore) Watermark() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.WatermarkMs
}

// SetWatermark records a new pull cursor.
func (s *FileStore) SetWatermark(ms int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.WatermarkMs = ms
}

// Match returns the cached resolution for a hash, or ErrNotFound.
func (s *FileStore) Match(hash string) (MatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data.Matches[hash]
	if !ok {
		return MatchRecord{}, ErrNotFound
	}
	return rec, nil
}

// SetMatch records or updates a hash's resolution.
func (s *FileStore) SetMatch(hash string, rec MatchRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Matches[hash] = rec
}

// DeleteMatch removes a cached resolution.
func (s *FileStore) DeleteMatch(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Matches, hash)
}

// UnmatchedAt returns the last failed match-check time for a hash and whether
// it is in the unmatched set.
func (s *FileStore) UnmatchedAt(hash string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.data.Unmatched[hash]
	return at, ok
}

// SetUnmatched records a failed match-check at the given time.
func (s *FileStore) SetUnmatched(hash string, at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Unmatched[hash] = at
}

// ClearUnmatched removes a hash from the unmatched set.
func (s *FileStore) ClearUnmatched(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Unmatched, hash)
}
