package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/token"
)

// exerciseStore runs the common Store contract against any implementation.
func exerciseStore(t *testing.T, s Store) {
	t.Helper()

	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Token round-trip.
	tok := token.Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: 100, ExpiresIn: 3600}
	s.SetToken(tok)
	if got := s.Token(); got != tok {
		t.Errorf("Token = %+v, want %+v", got, tok)
	}

	// Device ID generation is stable.
	id1 := s.DeviceID(func() string { return "gen-uuid" })
	id2 := s.DeviceID(func() string { return "other" })
	if id1 != "gen-uuid" || id2 != "gen-uuid" {
		t.Errorf("DeviceID not stable: %q then %q", id1, id2)
	}

	// Watermark.
	s.SetWatermark(12345)
	if got := s.Watermark(); got != 12345 {
		t.Errorf("Watermark = %d, want 12345", got)
	}

	// Match records.
	if _, err := s.Match("hash1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Match missing err = %v, want ErrNotFound", err)
	}
	rec := MatchRecord{BookFileID: 7, BookID: 9, LastPushedAt: 50, LastPushedPct: 0.5}
	s.SetMatch("hash1", rec)
	got, err := s.Match("hash1")
	if err != nil || got != rec {
		t.Errorf("Match = %+v, %v; want %+v", got, err, rec)
	}

	// Status-sync fields (Phase 9): the two seen/pushed pairs round-trip
	// alongside the progress fields. Setting a decisive `unread` records
	// LastSeenStatus without touching LastPushedStatus (decisive-but-no-op),
	// and a later `finished → read` push records both pairs.
	rec2 := MatchRecord{
		BookFileID:         7,
		BookID:             9,
		LastPushedAt:       50,
		LastPushedPct:      1.0,
		LastSeenStatus:     "finished",
		LastSeenStatusAt:   300,
		LastPushedStatus:   "read",
		LastPushedStatusAt: 310,
	}
	s.SetMatch("hash2", rec2)
	got2, err := s.Match("hash2")
	if err != nil || got2 != rec2 {
		t.Errorf("Match (status fields) = %+v, %v; want %+v", got2, err, rec2)
	}

	s.DeleteMatch("hash1")
	if _, err := s.Match("hash1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after DeleteMatch err = %v, want ErrNotFound", err)
	}

	// Unmatched cooldown.
	if _, ok := s.UnmatchedAt("hashX"); ok {
		t.Error("UnmatchedAt should report false for unknown hash")
	}
	s.SetUnmatched("hashX", 999)
	if at, ok := s.UnmatchedAt("hashX"); !ok || at != 999 {
		t.Errorf("UnmatchedAt = %d,%v; want 999,true", at, ok)
	}
	s.ClearUnmatched("hashX")
	if _, ok := s.UnmatchedAt("hashX"); ok {
		t.Error("ClearUnmatched did not remove hash")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func TestMemStore(t *testing.T) {
	exerciseStore(t, NewMemStore())
}

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "bridge-state.json")
	s := NewFileStore(path)
	exerciseStore(t, s)

	// Reload from disk into a fresh store and confirm persistence.
	s2 := NewFileStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload Load: %v", err)
	}
	if got := s2.Watermark(); got != 12345 {
		t.Errorf("persisted watermark = %d, want 12345", got)
	}
	if got := s2.Token().AccessToken; got != "a" {
		t.Errorf("persisted access token = %q, want a", got)
	}
	if got := s2.DeviceID(nil); got != "gen-uuid" {
		t.Errorf("persisted device id = %q, want gen-uuid", got)
	}
	// The Phase 9 seen/pushed status pairs survive a reload from disk.
	if rec, err := s2.Match("hash2"); err != nil {
		t.Fatalf("reload Match hash2: %v", err)
	} else if rec.LastSeenStatus != "finished" || rec.LastSeenStatusAt != 300 ||
		rec.LastPushedStatus != "read" || rec.LastPushedStatusAt != 310 {
		t.Errorf("persisted status fields = %+v, want finished/300 read/310", rec)
	}
}

func TestFileStorePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	s := NewFileStore(path)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file perm = %o, want 600", perm)
	}
}

func TestFileStoreAtomicityLeavesValidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-state.json")
	s := NewFileStore(path)
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// The file must always be valid JSON after a save.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	// No leftover temp files in the directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file %q after save", e.Name())
		}
	}
}

func TestFileStoreMissingFileIsNotError(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "nope.json"))
	if err := s.Load(); err != nil {
		t.Errorf("Load on missing file = %v, want nil", err)
	}
}
