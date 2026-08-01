package bookorbit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMatchCheckRequestShape(t *testing.T) {
	// The BookOrbit backend requires `books` as an array (not a keyed map).
	req := MatchCheckRequest{
		Hashes: []string{"h1"},
		Books:  []MatchCandidate{{Hash: "h1", Title: "T", Authors: "A", LastOpen: 100, Source: "file"}},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Hashes []string          `json:"hashes"`
		Books  []json.RawMessage `json:"books"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("books field must decode as an array: %v", err)
	}
	if len(decoded.Books) != 1 {
		t.Fatalf("books array length = %d, want 1", len(decoded.Books))
	}
}

func TestMatchCheckRequestDeviceFieldsAtTopLevel(t *testing.T) {
	// Every match-check payload is device-wrapped (bookorbit_api.lua dispatches
	// via self:withDevice(payload) inside matchCheck itself), not just
	// bulk-progress. The device fields must sit at the top level, sibling to
	// hashes/books, not nested.
	req := DeviceInfo{
		DeviceID:      "dev-1",
		DeviceModel:   "readest-bridge",
		PluginVersion: "0.1.0",
		DeviceTime:    "2026-01-02 03:04:05",
	}.WithMatchCheck([]string{"h1"}, []MatchCandidate{{Hash: "h1"}})

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hashes", "books", "deviceId", "deviceModel", "pluginVersion", "deviceTime"} {
		if _, ok := m[key]; !ok {
			t.Errorf("match-check request missing top-level field %q", key)
		}
	}
}

func TestMatchCandidateMetadataAmbiguousRoundTrips(t *testing.T) {
	for _, ambiguous := range []bool{true, false} {
		cand := MatchCandidate{Hash: "h1", MetadataAmbiguous: ambiguous}
		raw, err := json.Marshal(cand)
		if err != nil {
			t.Fatal(err)
		}
		var decoded MatchCandidate
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.MetadataAmbiguous != ambiguous {
			t.Errorf("MetadataAmbiguous round-trip = %v, want %v", decoded.MetadataAmbiguous, ambiguous)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["metadataAmbiguous"]; !ok {
			t.Error("candidate JSON missing metadataAmbiguous field")
		}
	}
}

func TestWithDeviceWrapsItems(t *testing.T) {
	d := DeviceInfo{
		DeviceID:      "id-1",
		DeviceModel:   "readest-bridge",
		PluginVersion: "0.1.0",
		DeviceTime:    "2026-01-02 03:04:05",
	}
	items := []ProgressItem{{Hash: "h1", Percentage: 0.5, Progress: "", Timestamp: 1000}}
	req := d.WithDevice(items)

	if req.DeviceID != "id-1" || req.DeviceModel != "readest-bridge" || req.PluginVersion != "0.1.0" {
		t.Errorf("device wrapper not propagated: %+v", req)
	}
	if req.DeviceTime != "2026-01-02 03:04:05" {
		t.Errorf("device time = %q", req.DeviceTime)
	}
	if len(req.Items) != 1 || req.Items[0].Percentage != 0.5 {
		t.Errorf("items not propagated: %+v", req.Items)
	}

	// The serialized body must carry the device fields at the top level.
	raw, _ := json.Marshal(req)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"deviceId", "deviceModel", "pluginVersion", "deviceTime", "items"} {
		if _, ok := m[key]; !ok {
			t.Errorf("bulk request missing top-level field %q", key)
		}
	}
}

func TestBulkProgressResponseUnmatched(t *testing.T) {
	body := `{"unmatched":["h1","h2"]}`
	var resp BulkProgressResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Unmatched) != 2 || resp.Unmatched[0] != "h1" {
		t.Errorf("Unmatched = %+v, want [h1 h2]", resp.Unmatched)
	}
}

// TestSetReadStatusRequestWireBody pins the Channel B wire contract: the body
// is exactly {"status": "..."} with no device-wrapper keys (the server DTO is
// single-field and forbidNonWhitelisted rejects extras with a 400). See
// docs/phase-9-status-sync-design.md §3.2.
func TestSetReadStatusRequestWireBody(t *testing.T) {
	req := SetReadStatusRequest{Status: "read"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"status":"read"}` {
		t.Errorf("SetReadStatusRequest body = %s, want exactly {\"status\":\"read\"}", raw)
	}
	for _, notWant := range []string{"deviceId", "deviceModel", "pluginVersion", "deviceTime"} {
		if strings.Contains(string(raw), notWant) {
			t.Errorf("body = %s, must not carry device-wrapper key %q", raw, notWant)
		}
	}
}

// TestSetReadStatusResponseDecodes pins that {"readStatus": "<token>"} decodes.
func TestSetReadStatusResponseDecodes(t *testing.T) {
	var resp SetReadStatusResponse
	if err := json.Unmarshal([]byte(`{"readStatus":"read"}`), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ReadStatus != "read" {
		t.Errorf("ReadStatus = %q, want read", resp.ReadStatus)
	}
}

func TestWithUpdateProgress(t *testing.T) {
	d := DeviceInfo{DeviceID: "dev-1", DeviceModel: "readest-bridge"}
	req := d.WithUpdateProgress("h1", 0.5, "", 1700000000)

	if req.Document != "h1" || req.Percentage != 0.5 || req.Timestamp != 1700000000 {
		t.Errorf("WithUpdateProgress fields = %+v", req)
	}
	if req.Device != "readest-bridge" || req.DeviceID != "dev-1" {
		t.Errorf("WithUpdateProgress device identity = %+v", req)
	}

	// The wire shape uses kosync's own keys, not the camelCase device wrapper.
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"document", "percentage", "progress", "device", "device_id", "timestamp"} {
		if _, ok := m[key]; !ok {
			t.Errorf("update-progress request missing field %q", key)
		}
	}
	for _, key := range []string{"deviceId", "pluginVersion", "deviceTime"} {
		if _, ok := m[key]; ok {
			t.Errorf("update-progress request should not carry device-wrapper key %q", key)
		}
	}
}
