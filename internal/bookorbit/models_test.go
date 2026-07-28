package bookorbit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMatchCheckRequestShape(t *testing.T) {
	// The BookOrbit backend requires `books` as an array (not a keyed map).
	req := MatchCheckRequest{
		Hashes: []string{"h1"},
		Books:  []MatchCandidate{{Hash: "h1", Title: "T", Authors: "A", LastOpen: 100, Source: "readest"}},
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

func TestClientStubReturnsNotImplemented(t *testing.T) {
	c := NewClient("http://nas:8080/api/v1", "user", "key", DeviceInfo{}, nil, nil, 900*1024)
	if err := c.Auth(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Auth err = %v, want ErrNotImplemented", err)
	}
	if _, err := c.MatchCheck(context.Background(), MatchCheckRequest{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("MatchCheck err = %v, want ErrNotImplemented", err)
	}
	if err := c.BulkProgress(context.Background(), BulkProgressRequest{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("BulkProgress err = %v, want ErrNotImplemented", err)
	}
}
