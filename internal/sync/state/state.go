// Package state defines the bridge's only persistence: a small store holding
// Supabase tokens, the per-hash match cache, unmatched cooldowns, the pull
// watermark, and the bridge's stable device identity.
//
// The store is deliberately tiny. Two implementations exist from the start:
// FileStore, a JSON file written atomically with 0600 permissions for
// production, and MemStore, an in-memory store for tests and ephemeral runs.
// Both satisfy the Store interface the engine consumes.
package state

import (
	"errors"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/token"
)

// ErrNotFound is returned when a requested match record is not in the cache.
var ErrNotFound = errors.New("state: no match record for hash")

// MatchRecord is the cached result of resolving one Readest hash to a
// BookOrbit library entry, plus the last progress we pushed so the engine can
// skip redundant uploads.
type MatchRecord struct {
	BookFileID    int64   `json:"bookFileId"`
	BookID        int64   `json:"bookId"`
	LastPushedAt  int64   `json:"lastPushedAt"`  // Unix epoch seconds of last successful push
	LastPushedPct float64 `json:"lastPushedPct"` // 0..1 percentage last pushed

	// LastSeenStatus/LastSeenStatusAt record the most recent *Readest*
	// reading_status value observed ("unread"/"finished"/"abandoned", or ""
	// for a non-decisive/absent one) and when it was seen. This is distinct
	// from LastPushedStatus: an `unread → finished` transition must diff
	// against what was *seen*, not against what was *pushed* (a decisive-no-op
	// `unread` pushes nothing but must still be recorded so the later
	// transition is detected as a change). See the phase-9 design, Decision A.
	LastSeenStatus   string `json:"lastSeenStatus"`
	LastSeenStatusAt int64  `json:"lastSeenStatusAt"` // Unix epoch seconds
	// LastPushedStatus/LastPushedStatusAt record the last BookOrbit token
	// written ("read"/"abandoned", or "" when never pushed / decisive-no-op)
	// and when it was written. The engine's unchanged-skip compares the mapped
	// token against LastPushedStatus; a status-push failure leaves it
	// unadvanced so the next poll retries (decoupled from progress, Decision E).
	LastPushedStatus   string `json:"lastPushedStatus"`
	LastPushedStatusAt int64  `json:"lastPushedStatusAt"` // Unix epoch seconds
}

// Data is the full contents of the store. Tokens are kept alongside sync state
// so both are written atomically in a single save. DeviceID is generated on
// first run and persisted so the bridge keeps one stable identity in BookOrbit.
type Data struct {
	// Token is the persisted Supabase credential set.
	Token token.Token `json:"token"`

	// DeviceID is the stable identifier sent to BookOrbit. Empty until first
	// generation, after which it never changes.
	DeviceID string `json:"deviceId"`

	// WatermarkMs is the Readest pull cursor in epoch milliseconds:
	// max(synced_at, updated_at, deleted_at) across all rows seen.
	WatermarkMs int64 `json:"watermarkMs"`

	// Matches maps a Readest partial-MD5 hash to its BookOrbit resolution.
	Matches map[string]MatchRecord `json:"matches"`

	// Unmatched maps a hash to the Unix epoch second of its last failed
	// match-check, so the engine can cool down before re-attempting.
	Unmatched map[string]int64 `json:"unmatched"`
}

// NewData returns an empty, fully-initialized Data with non-nil maps.
func NewData() Data {
	return Data{
		Matches:   make(map[string]MatchRecord),
		Unmatched: make(map[string]int64),
	}
}

// Store is the persistence contract the engine uses. Implementations must be
// safe for the engine's single-writer use; Load and Save bracket a sync pass.
type Store interface {
	// Load reads the persisted state into memory. A first run (no underlying
	// data) returns an empty NewData() with no error.
	Load() error
	// Save persists the current in-memory state durably and atomically.
	Save() error

	// Token returns the persisted Supabase token set.
	Token() token.Token
	// SetToken records a new token set (called after sign-in or refresh).
	SetToken(t token.Token)

	// DeviceID returns the stable bridge device identity, generating and
	// recording one via genID on first use.
	DeviceID(genID func() string) string

	// Watermark returns the Readest pull cursor in epoch milliseconds.
	Watermark() int64
	// SetWatermark records a new pull cursor.
	SetWatermark(ms int64)

	// Match returns the cached resolution for a hash, or ErrNotFound.
	Match(hash string) (MatchRecord, error)
	// SetMatch records or updates a hash's resolution.
	SetMatch(hash string, rec MatchRecord)
	// DeleteMatch removes a cached resolution (e.g., for a deleted book).
	DeleteMatch(hash string)

	// UnmatchedAt returns the last time the engine recorded this hash as
	// unmatched by BookOrbit (whether the server returned it in
	// resp.Unmatched or omitted it from both response lists — the
	// latter is the common case for books BookOrbit's library has never
	// seen; see docs/adr/phase-6-decision-record.md Addendum 2), and
	// whether the hash is currently in the unmatched set.
	UnmatchedAt(hash string) (at int64, ok bool)
	// SetUnmatched records the given Unix-second time as the most recent
	// match-check result was "no BookOrbit library match for this hash."
	// Such hashes settle into the UnmatchedCooldown recheck gate so the
	// engine does not re-submit them on every poll.
	SetUnmatched(hash string, at int64)
	// ClearUnmatched removes a hash from the unmatched set (after it matches).
	ClearUnmatched(hash string)
}
