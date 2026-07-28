package bookorbit

// MatchCandidate describes one book the bridge is trying to resolve to a
// BookOrbit library entry. The reference plugin sends candidates as an array
// of these objects (not a keyed map); the bridge mirrors that exact shape.
type MatchCandidate struct {
	Hash    string `json:"hash"`
	Title   string `json:"title"`
	Authors string `json:"authors"`
	// LastOpen is a Unix epoch second timestamp, or 0 when unknown.
	LastOpen int64 `json:"lastOpen"`
	// Source identifies the origin of the candidate (e.g. "readest").
	Source string `json:"source"`
}

// MatchCheckRequest is the body of POST /koreader/plugin/match-check. Hashes
// lists every digest being resolved; Books carries the metadata candidates in
// the same order of discovery. The array form (not a keyed object) is required
// by the BookOrbit backend.
type MatchCheckRequest struct {
	Hashes []string         `json:"hashes"`
	Books  []MatchCandidate `json:"books"`
}

// Match is a single resolved book: the server mapped a partial-MD5 hash to a
// concrete library file and book record.
type Match struct {
	Hash       string `json:"hash"`
	BookFileID int64  `json:"bookFileId"`
	BookID     int64  `json:"bookId"`
}

// MatchCheckResponse is the server's reply to a match-check. Unmatched lists
// hashes it could not resolve; LibraryVersion is an opaque token the client
// can use to detect library changes. Arrays are always present (never null).
type MatchCheckResponse struct {
	Matches        []Match  `json:"matches"`
	Unmatched      []string `json:"unmatched"`
	LibraryVersion string   `json:"libraryVersion"`
}

// ProgressItem is one book's progress within a bulk upload. Percentage is a
// 0..1 float; Progress is an opaque position string that the bridge leaves
// empty for percentage-only sync; Timestamp is Unix epoch seconds.
type ProgressItem struct {
	Hash       string  `json:"hash"`
	Percentage float64 `json:"percentage"`
	Progress   string  `json:"progress"`
	Timestamp  int64   `json:"timestamp"`
}

// DeviceInfo identifies this bridge instance to the server. The BookOrbit
// backend wraps bulk-progress bodies with these fields (the reference plugin's
// withDevice helper), so every bulk request carries them.
type DeviceInfo struct {
	DeviceID      string `json:"deviceId"`
	DeviceModel   string `json:"deviceModel"`
	PluginVersion string `json:"pluginVersion"`
	// DeviceTime is the local wall clock formatted "2006-01-02 15:04:05".
	DeviceTime string `json:"deviceTime"`
}

// DeviceTimeFormat is the exact layout BookOrbit expects for DeviceTime.
const DeviceTimeFormat = "2006-01-02 15:04:05"

// BulkProgressRequest is the body of POST /koreader/plugin/progress: the
// device wrapper plus the items being pushed.
type BulkProgressRequest struct {
	DeviceID      string         `json:"deviceId"`
	DeviceModel   string         `json:"deviceModel"`
	PluginVersion string         `json:"pluginVersion"`
	DeviceTime    string         `json:"deviceTime"`
	Items         []ProgressItem `json:"items"`
}

// WithDevice returns a BulkProgressRequest with the device wrapper fields
// populated from d and the given items. This mirrors the plugin's withDevice
// helper and keeps the wrapping rule in one place.
func (d DeviceInfo) WithDevice(items []ProgressItem) BulkProgressRequest {
	return BulkProgressRequest{
		DeviceID:      d.DeviceID,
		DeviceModel:   d.DeviceModel,
		PluginVersion: d.PluginVersion,
		DeviceTime:    d.DeviceTime,
		Items:         items,
	}
}

// BulkProgressResponse is the server's reply to a bulk progress upload. The
// exact fields are confirmed in Phase 5 against a live server; it is modeled
// minimally here and treated as best-effort.
type BulkProgressResponse struct {
	// Updated counts items the server accepted, when reported.
	Updated int `json:"updated"`
}
