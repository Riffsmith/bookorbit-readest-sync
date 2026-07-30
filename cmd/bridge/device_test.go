package main

import (
	"regexp"
	"testing"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/config"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
)

// uuidV4Pattern matches a syntactically valid RFC 4122 version-4 UUID: the
// version nibble is "4" and the variant nibble is one of 8/9/a/b.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDIsValidV4(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := newUUID()
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("newUUID() = %q, not a valid v4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("newUUID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func TestResolveDeviceIDExplicitConfigWins(t *testing.T) {
	cfg := config.Default()
	cfg.BookOrbit.DeviceID = "explicit-id"

	st := state.NewMemStore()
	st.DeviceID(func() string { return "persisted-id" }) // pre-seed a persisted id

	if got := resolveDeviceID(cfg, st); got != "explicit-id" {
		t.Errorf("resolveDeviceID = %q, want the explicit config value", got)
	}
}

func TestResolveDeviceIDReusesPersisted(t *testing.T) {
	cfg := config.Default()
	st := state.NewMemStore()
	st.DeviceID(func() string { return "persisted-id" })

	if got := resolveDeviceID(cfg, st); got != "persisted-id" {
		t.Errorf("resolveDeviceID = %q, want the persisted value", got)
	}
}

func TestResolveDeviceIDGeneratesAndPersistsFreshUUID(t *testing.T) {
	cfg := config.Default()
	st := state.NewMemStore()

	got := resolveDeviceID(cfg, st)
	if !uuidV4Pattern.MatchString(got) {
		t.Fatalf("resolveDeviceID = %q, not a valid v4 UUID", got)
	}
	// A second lookup within the same process must observe the same,
	// now-persisted id rather than generating a new one every time.
	if got2 := resolveDeviceID(cfg, st); got2 != got {
		t.Errorf("resolveDeviceID not stable across calls: %q then %q", got, got2)
	}
}
