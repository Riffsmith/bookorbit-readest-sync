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
	// MetadataAmbiguous flags a candidate whose title/author metadata is
	// uncertain (e.g. derived from an unreliable source). Both reference call
	// sites (bookorbit_sweep.lua, bookorbit_book_sync.lua) populate this field
	// on every candidate they build.
	MetadataAmbiguous bool `json:"metadataAmbiguous"`
}

// MatchCheckRequest is the body of POST /koreader/plugin/match-check. Hashes
// lists every digest being resolved; Books carries the metadata candidates in
// the same order of discovery. The array form (not a keyed object) is required
// by the BookOrbit backend. Every match-check payload is device-wrapped (the
// reference plugin dispatches via self:withDevice(payload)), not just
// bulk-progress, so the device fields live directly on this request rather
// than in a separate wrapper type.
type MatchCheckRequest struct {
	Hashes        []string         `json:"hashes"`
	Books         []MatchCandidate `json:"books"`
	DeviceID      string           `json:"deviceId"`
	DeviceModel   string           `json:"deviceModel"`
	PluginVersion string           `json:"pluginVersion"`
	DeviceTime    string           `json:"deviceTime"`
}

// WithMatchCheck returns a MatchCheckRequest with the device wrapper fields
// populated from d and the given hashes/candidates. This is a convenience
// constructor only: Client.MatchCheck stamps the device fields itself
// immediately before dispatch, so a caller may also build a MatchCheckRequest
// by hand and still get a correctly device-wrapped request on the wire.
func (d DeviceInfo) WithMatchCheck(hashes []string, books []MatchCandidate) MatchCheckRequest {
	return MatchCheckRequest{
		Hashes:        hashes,
		Books:         books,
		DeviceID:      d.DeviceID,
		DeviceModel:   d.DeviceModel,
		PluginVersion: d.PluginVersion,
		DeviceTime:    d.DeviceTime,
	}
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
// can use to detect library changes. Matches/Unmatched may arrive absent or
// null on the wire; the client normalizes both to non-nil empty slices.
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
// backend wraps every plugin-endpoint body with these fields (the reference
// plugin's withDevice helper), so every match-check and bulk-progress request
// carries them.
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
// helper and is a convenience constructor only: Client.BulkProgress stamps the
// device fields itself immediately before dispatch (see MatchCheck's doc
// comment for the same rationale).
func (d DeviceInfo) WithDevice(items []ProgressItem) BulkProgressRequest {
	return BulkProgressRequest{
		DeviceID:      d.DeviceID,
		DeviceModel:   d.DeviceModel,
		PluginVersion: d.PluginVersion,
		DeviceTime:    d.DeviceTime,
		Items:         items,
	}
}

// BulkProgressResponse is the server's reply to a bulk progress upload.
// Unmatched is the only field any reference call site reads off this response
// (bookorbit_sweep.lua's stepProgressNext reads body.unmatched to mark a push
// as failed for a hash BookOrbit could not resolve); it may arrive absent or
// null on the wire and is normalized to a non-nil empty slice.
type BulkProgressResponse struct {
	Unmatched []string `json:"unmatched"`
}

// UpdateProgressRequest is the body of PUT /koreader/syncs/progress, the
// kosync-compatible single-book fallback endpoint used when the bulk endpoint
// is unsupported by the target server. Its field naming deliberately does NOT
// match the camelCase device-wrapper shape used by the /koreader/plugin/*
// endpoints above: this endpoint is shared with vanilla KOReader's stock kosync
// plugin and uses kosync's own wire shape, verified against
// bookorbit_api.lua:updateProgress. It carries no device wrapper.
type UpdateProgressRequest struct {
	Document   string  `json:"document"`
	Percentage float64 `json:"percentage"`
	Progress   string  `json:"progress"`
	Device     string  `json:"device"`
	DeviceID   string  `json:"device_id"`
	Timestamp  int64   `json:"timestamp"`
}

// WithUpdateProgress returns an UpdateProgressRequest with the device identity
// fields populated from d. This is a convenience constructor only:
// Client.UpdateProgress stamps Device/DeviceID itself immediately before
// dispatch, overriding whatever the caller supplied; Timestamp is passed
// through unmodified since it is domain data (the reading-progress moment),
// not a freshness field.
func (d DeviceInfo) WithUpdateProgress(document string, percentage float64, progress string, timestamp int64) UpdateProgressRequest {
	return UpdateProgressRequest{
		Document:   document,
		Percentage: percentage,
		Progress:   progress,
		Device:     d.DeviceModel,
		DeviceID:   d.DeviceID,
		Timestamp:  timestamp,
	}
}
