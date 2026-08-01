package readest

import (
	"encoding/json"
	"fmt"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/util"
)

// DummyHash is the sentinel book hash the Readest server emits on an initial
// `since=0` pull. It carries no real book and must always be filtered out.
const DummyHash = "00000000000000000000000000000000"

// BookRow is one row of the Readest `GET /sync?type=books` response. Field
// names mirror the snake_case database columns the server returns; only the
// fields the bridge needs are modeled. Timestamps arrive as ISO-8601 strings.
type BookRow struct {
	BookHash  string          `json:"book_hash"`
	MetaHash  string          `json:"meta_hash"`
	Title     string          `json:"title"`
	Author    string          `json:"author"`
	Format    string          `json:"format"`
	Progress  json.RawMessage `json:"progress"` // JSON tuple [cur, total]; may be null or a string
	UpdatedAt string          `json:"updated_at"`
	DeletedAt string          `json:"deleted_at"`
	SyncedAt  string          `json:"synced_at"`
	CreatedAt string          `json:"created_at"`
	// ReadingStatus is the Readest cloud row's reading status token
	// ("unread" | "reading" | "finished" | "abandoned", or "" when unset).
	// It already arrives on the bulk-pull response the engine consumes, so
	// no new endpoint is required; it is stored raw, exactly like the other
	// scalar fields.
	ReadingStatus string `json:"reading_status"`
	// ReadingStatusUpdatedAt is the ISO-8601 timestamp at which the reading
	// status last changed, stored raw (not pre-converted to ms) to match the
	// convention of UpdatedAt/DeletedAt/SyncedAt/CreatedAt. It is converted
	// on demand via util.ISOToMs when a caller needs it; the v1 status-sync
	// engine does not use it in watermark math (status pushes are decoupled
	// from the progress watermark per the Phase 9 design).
	ReadingStatusUpdatedAt string `json:"reading_status_updated_at"`
}

// BooksResponse is the envelope of `GET /sync?type=books`. The server returns
// a JSON object with a `books` array.
type BooksResponse struct {
	Books []BookRow `json:"books"`
}

// IsDummy reports whether the row is the server's sentinel placeholder hash.
func (r BookRow) IsDummy() bool {
	return r.BookHash == DummyHash
}

// IsDeleted reports whether the row has a non-null, non-empty deleted_at. The
// bridge ignores deleted books: they are never pushed, and any cached match is
// treated as stale by the engine.
func (r BookRow) IsDeleted() bool {
	return r.DeletedAt != "" && r.DeletedAt != "null"
}

// ProgressTuple decodes the raw `progress` field into (cur, total). The field
// is normally a JSON array like [42, 250], but the server has been observed to
// send it as a string containing JSON or as null; this method normalizes those
// forms. The second return value reports whether a usable two-element numeric
// tuple was found.
func (r BookRow) ProgressTuple() (cur, total float64, ok bool) {
	return decodeProgressTuple(r.Progress)
}

// decodeProgressTuple extracts (cur, total) from the wire form, tolerating a
// JSON array, a stringified JSON array, or null/absent.
func decodeProgressTuple(raw json.RawMessage) (cur, total float64, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, 0, false
	}

	// If the server wrapped the tuple in a string, unwrap it first.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return 0, 0, false
		}
		raw = json.RawMessage(asString)
	}

	var tuple []float64
	if err := json.Unmarshal(raw, &tuple); err != nil {
		return 0, 0, false
	}
	if len(tuple) != 2 {
		return 0, 0, false
	}
	return tuple[0], tuple[1], true
}

// Percentage converts the row's progress tuple into a 0..1 ratio using the
// shared guarded calculation. It returns ok=false for rows with no usable
// tuple or a non-positive total, matching the engine's "skip this row" path.
func (r BookRow) Percentage() (pct float64, ok bool) {
	cur, total, has := r.ProgressTuple()
	if !has {
		return 0, false
	}
	p, err := util.Percent(cur, total)
	if err != nil {
		return 0, false
	}
	return p, true
}

// WatermarkMs returns the pull-cursor contribution of this row in epoch
// milliseconds: max(synced_at, updated_at, deleted_at). synced_at is the
// server-authoritative value and wins when present. Unparseable timestamps are
// treated as zero so a single bad row cannot stall the watermark.
func (r BookRow) WatermarkMs() int64 {
	synced, _ := util.ISOToMs(r.SyncedAt)
	updated, _ := util.ISOToMs(r.UpdatedAt)
	deleted, _ := util.ISOToMs(r.DeletedAt)
	return util.MaxMs(synced, updated, deleted)
}

// UpdatedMs returns the row's updated_at in epoch milliseconds, or an error if
// it cannot be parsed. Used as the BookOrbit progress timestamp source.
func (r BookRow) UpdatedMs() (int64, error) {
	ms, err := util.ISOToMs(r.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("readest: row %s: %w", r.BookHash, err)
	}
	return ms, nil
}
