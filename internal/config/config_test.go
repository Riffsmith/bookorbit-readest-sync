package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSimpleYAML(t *testing.T) {
	doc := `
# a comment
readest:
  email: "me@example.com"   # inline comment
  password: 'secret'

bridge:
  poll_interval: "5m"
  log_level: debug
`
	got, err := parseSimpleYAML([]byte(doc))
	if err != nil {
		t.Fatalf("parseSimpleYAML error: %v", err)
	}
	want := map[string]string{
		"readest.email":        "me@example.com",
		"readest.password":     "secret",
		"bridge.poll_interval": "5m",
		"bridge.log_level":     "debug",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseSimpleYAMLErrors(t *testing.T) {
	cases := []struct{ name, doc string }{
		{"tab indent", "bridge:\n\tlog_level: debug\n"},
		{"indented without section", "  log_level: debug\n"},
		{"nested map", "bridge:\n  nested:\n    deeper: 1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseSimpleYAML([]byte(c.doc)); err == nil {
				t.Fatalf("expected error for %q", c.doc)
			}
		})
	}
}

func TestDurationParsing(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"15m", 15 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"900", 900 * time.Second},
	}
	for _, c := range cases {
		got, err := time_ParseDuration(c.in)
		if err != nil || got != c.want {
			t.Errorf("time_ParseDuration(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	if _, err := time_ParseDuration("nonsense"); err == nil {
		t.Error("expected error for unparseable duration")
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("Load missing file err = %v, want ErrNoConfigFile", err)
	}
	if cfg.Bridge.PollInterval != 15*time.Minute {
		t.Errorf("default poll interval = %v, want 15m", cfg.Bridge.PollInterval)
	}
	if cfg.Bridge.MatchBatchSize != 500 || cfg.Bridge.ProgressBatchSize != 100 {
		t.Errorf("default batch sizes = %d/%d, want 500/100", cfg.Bridge.MatchBatchSize, cfg.Bridge.ProgressBatchSize)
	}
	if cfg.Readest.SupabaseAnonKey == "" {
		t.Error("default anon key should be decoded from the bundled base64")
	}
}

func TestLoadFromFileAndNormalize(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	doc := `
readest:
  email: "file@example.com"
  password: "filepass"
bookorbit:
  server_url: "https://nas:8080/"
  username: "reader"
  password: "bpass"
  device_name: "file-device"
bridge:
  poll_interval: "10m"
  match_batch_size: 250
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.BookOrbit.DeviceName != "file-device" {
		t.Errorf("device_name = %q, want file value", cfg.BookOrbit.DeviceName)
	}
	if cfg.BookOrbit.ServerURL != "https://nas:8080/api/v1" {
		t.Errorf("server_url = %q, want normalized /api/v1", cfg.BookOrbit.ServerURL)
	}
	if cfg.Bridge.PollInterval != 10*time.Minute {
		t.Errorf("poll_interval = %v, want 10m", cfg.Bridge.PollInterval)
	}
	if cfg.Bridge.MatchBatchSize != 250 {
		t.Errorf("match_batch_size = %d, want 250", cfg.Bridge.MatchBatchSize)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("bridge:\n  bogus_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv(EnvReadestEmail, "env@example.com")
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("readest:\n  email: \"file@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Readest.Email != "env@example.com" {
		t.Errorf("email = %q, want env override", cfg.Readest.Email)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	cfg := Default()
	cfg.BookOrbit.ServerURL = ""
	err := cfg.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate err = %v, want *ValidationError", err)
	}
	if len(ve.Problems) < 3 {
		t.Errorf("expected multiple problems, got %v", ve.Problems)
	}
}

func TestAuthKeyDerivation(t *testing.T) {
	cfg := Config{}
	cfg.BookOrbit.Password = "password"
	if got := cfg.AuthKey(); got != "5f4dcc3b5aa765d61d8327deb882cf99" {
		t.Errorf("AuthKey from password = %q, want MD5 digest", got)
	}
	cfg.BookOrbit.Userkey = "  5F4DCC3B5AA765D61D8327DEB882CF99 "
	if got := cfg.AuthKey(); got != "5f4dcc3b5aa765d61d8327deb882cf99" {
		t.Errorf("AuthKey from userkey = %q, want normalized digest", got)
	}
}

func TestEnvSupabaseAnonKeyBase64JWTShapedDecodes(t *testing.T) {
	// applyEnv's conditional: a value that base64-decodes AND whose decoded
	// form starts with "eyJ" (a JWT-shaped prefix) is treated as base64 and
	// decoded before being stored.
	setRequiredEnv(t)
	plain := "eyJhbGciOiJIUzI1NiJ9-test-payload"
	encoded := base64.StdEncoding.EncodeToString([]byte(plain))
	t.Setenv(EnvSupabaseAnonKey, encoded)

	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("Load err = %v, want ErrNoConfigFile", err)
	}
	if cfg.Readest.SupabaseAnonKey != plain {
		t.Errorf("SupabaseAnonKey = %q, want decoded %q", cfg.Readest.SupabaseAnonKey, plain)
	}
}

func TestEnvSupabaseAnonKeyRawPassthroughWhenNotBase64(t *testing.T) {
	// A value that is not valid base64 is used verbatim, exercising
	// applyEnv's else branch.
	setRequiredEnv(t)
	raw := "not-a-valid-base64-string!!!"
	t.Setenv(EnvSupabaseAnonKey, raw)

	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("Load err = %v, want ErrNoConfigFile", err)
	}
	if cfg.Readest.SupabaseAnonKey != raw {
		t.Errorf("SupabaseAnonKey = %q, want raw passthrough %q", cfg.Readest.SupabaseAnonKey, raw)
	}
}

// TestSyncStatusDefaultsFalse pins the opt-in default: with neither file nor
// env set, SyncStatus is false (Decision F). A status write edits what the
// BookOrbit catalog displays, so off-by-default is the safe posture.
func TestSyncStatusDefaultsFalse(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("Load err = %v, want ErrNoConfigFile", err)
	}
	if cfg.Bridge.SyncStatus {
		t.Error("SyncStatus should default to false (opt-in)")
	}
}

// TestSyncStatusFromFile exercises the YAML `bridge.sync_status` key.
func TestSyncStatusFromFile(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	doc := `
readest:
  email: "file@example.com"
  password: "filepass"
bookorbit:
  server_url: "https://nas:8080"
  username: "reader"
  password: "bpass"
bridge:
  sync_status: true
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.Bridge.SyncStatus {
		t.Error("SyncStatus = false, want true from file")
	}
}

// TestSyncStatusEnvOverridesFile exercises the BRIDGE_SYNC_STATUS override and
// the new setBool helper: a non-empty env value wins over the file.
func TestSyncStatusEnvOverridesFile(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv(EnvSyncStatus, "false")
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("bridge:\n  sync_status: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if cfg.Bridge.SyncStatus {
		t.Error("SyncStatus = true, want env override to false")
	}
}

// TestSyncStatusEnvInvalidValueIgnored: an unparseable env value is ignored
// (the config file / default wins), matching the lenient duration-override
// convention — an invalid value never blocks startup.
func TestSyncStatusEnvInvalidValueIgnored(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv(EnvSyncStatus, "not-a-bool")
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("Load err = %v, want ErrNoConfigFile", err)
	}
	if cfg.Bridge.SyncStatus {
		t.Error("SyncStatus = true after invalid env value, want false (ignored)")
	}
}

// TestSyncStatusFileInvalidValueRejected: an unparseable *file* value is a
// hard error, consistent with every other typed key's error wrapping.
func TestSyncStatusFileInvalidValueRejected(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("bridge:\n  sync_status: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid bridge.sync_status value")
	}
}

// --- Security-hardening tests (URL scheme / loopback / cleartext opt-in) ---
//
// These cover the URL transport guard introduced by the
// security-hardening-bookorbit-url-validation design. Per the design's §7
// verification rule: when a pre-existing test fails because of a new Validate
// clause, fix the test's URL to a loopback or https value — never weaken the
// check. The setRequiredEnv helper above already moved to https:// for that
// reason; the tests below exercise both the rejection and the opt-in arms.

// validConfig returns a default-populated Config that passes Validate (every
// required field present; BookOrbit URL https; Readest URLs already https in
// Default; SupabaseAnonKey supplied since validConfig skips finalize()).
// Callers mutate one field to trigger a specific rejection.
func validConfig() Config {
	cfg := Default()
	cfg.Readest.Email = "test@example.com"
	cfg.Readest.Password = "testpass"
	cfg.Readest.SupabaseAnonKey = "decoded-anon-key-for-testing"
	cfg.BookOrbit.ServerURL = "https://nas:8080/api/v1"
	cfg.BookOrbit.Username = "reader"
	cfg.BookOrbit.Password = "bpass"
	return cfg
}

func TestValidateRejectsBadScheme(t *testing.T) {
	cfg := validConfig()
	cfg.BookOrbit.ServerURL = "gopher://nas/api/v1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate returned nil for gopher scheme")
	} else {
		ve := err.(*ValidationError)
		if !containsProblem(ve.Problems, "must use http or https") {
			t.Errorf("expected 'must use http or https' problem, got %v", ve.Problems)
		}
	}
}

func TestValidateRejectsCleartextNonLoopback(t *testing.T) {
	cfg := validConfig()
	cfg.BookOrbit.ServerURL = "http://192.168.1.10:3000/api/v1"
	cfg.BookOrbit.AllowInsecureTransport = false
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate returned nil for cleartext non-loopback")
	}
	ve := err.(*ValidationError)
	if !containsProblem(ve.Problems, "cleartext http to a non-loopback host") {
		t.Errorf("expected cleartext warning problem, got %v", ve.Problems)
	}

	// With the opt-in boolean set, the cleartext clause no longer rejects.
	cfg.BookOrbit.AllowInsecureTransport = true
	if err := cfg.Validate(); err != nil {
		ve, ok := err.(*ValidationError)
		if !ok {
			t.Fatalf("unexpected error type %T: %v", err, err)
		}
		if containsProblem(ve.Problems, "cleartext http to a non-loopback host") {
			t.Errorf("opt-in boolean did not suppress cleartext rejection: %v", ve.Problems)
		}
		// Other unrelated problems are fine; we asserted only about the cleartext clause. If the caller
		// wanted a fully-valid config here it would use https:// instead, which is the recommended posture.
	}
}

func TestValidateAcceptsLoopbackCleartext(t *testing.T) {
	for _, url := range []string{
		"http://127.0.0.1:3000/api/v1",
		"http://localhost:3000/api/v1",
		"http://[::1]:8443/api/v1",
		"http://169.254.1.1:3000/api/v1",
	} {
		t.Run(url, func(t *testing.T) {
			cfg := validConfig()
			cfg.BookOrbit.ServerURL = url
			cfg.BookOrbit.AllowInsecureTransport = false // explicit: loopback doesn't need the opt-in
			// We assert only that the cleartext clause does NOT fire. Isolating
			// the cleartext clause (rather than asserting Validate returns nil)
			// keeps the test focused on the security property even if a future
			// unrelated clause exists; the loopback URLs are syntactically
			// valid so no unrelated problem should appear here.
			var problems []string
			if err := cfg.Validate(); err != nil {
				ve, ok := err.(*ValidationError)
				if !ok {
					t.Fatalf("unexpected error type %T: %v", err, err)
				}
				problems = ve.Problems
			}
			if containsProblem(problems, "cleartext") {
				t.Errorf("loopback URL %q rejected by cleartext clause: %v", url, problems)
			}
		})
	}
}

func TestValidateRejectsReadestHttp(t *testing.T) {
	cfg := validConfig()
	cfg.Readest.SupabaseURL = "http://sb.example.co"
	cfg.Readest.SyncBaseURL = "http://sync.example.com/api"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate returned nil for cleartext Readest URLs")
	}
	ve := err.(*ValidationError)
	if !containsProblem(ve.Problems, "readest.supabase_url must use https") {
		t.Errorf("expected readest.supabase_url https problem, got %v", ve.Problems)
	}
	if !containsProblem(ve.Problems, "readest.sync_base_url must use https") {
		t.Errorf("expected readest.sync_base_url https problem, got %v", ve.Problems)
	}

	// Loopback http:// is still rejected for Readest — there is no opt-out,
	// the credentials travel to a third-party endpoint even when self-hosted.
	cfg2 := validConfig()
	cfg2.Readest.SupabaseURL = "http://127.0.0.1:8999"
	if err := cfg2.Validate(); err == nil {
		t.Error("Validate accepted http://127.0.0.1 for Readest SupabaseURL — no opt-out is permitted")
	} else {
		ve := err.(*ValidationError)
		if !containsProblem(ve.Problems, "readest.supabase_url must use https") {
			t.Errorf("expected readest.supabase_url https requirement even on loopback, got %v", ve.Problems)
		}
	}
}

func TestLoadEnvAllowInsecureTransport(t *testing.T) {
	// A cleartext non-loopback URL would normally fail Validate; the env
	// opt-in boolean must let it through. Mirrors the existing TestEnvOverridesFile
	// shape: env wins over the file's unset default.
	//
	// strconv.ParseBool is the parser for setBool (see env.go); it accepts
	// true/false/1/0/t/f/T/F/TRUE/FALSE (Go stdlib). The yaml comment in
	// configs/bridge.example.yaml suggests yes/no/on/off for sync_status,
	// but those do NOT parse with strconv.ParseBool — that's a pre-existing
	// doc inaccuracy beyond this design's scope to fix. Use "true" here.
	setRequiredEnv(t)
	t.Setenv(EnvBookOrbitServerURL, "http://192.168.1.10:3000")
	t.Setenv(EnvBookOrbitAllowInsecureTransport, "true")
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil && !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.BookOrbit.AllowInsecureTransport {
		t.Error("AllowInsecureTransport = false, want true from env")
	}
}

func TestLoadYamlAllowInsecureTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	doc := `
readest:
  email: "file@example.com"
  password: "filepass"
bookorbit:
  server_url: "https://nas:8080"
  username: "reader"
  password: "bpass"
  allow_insecure_transport: true
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if !cfg.BookOrbit.AllowInsecureTransport {
		t.Error("AllowInsecureTransport = false, want true from yaml file")
	}
}

func TestLoadYamlAllowInsecureTransportInvalidValueRejected(t *testing.T) {
	// An unparseable value should be rejected by applyValues (the safety rail
	// the unknown-key rejection depends on). Mirrors TestSyncStatusFileInvalidValueRejected.
	setRequiredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("bookorbit:\n  allow_insecure_transport: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid bookorbit.allow_insecure_transport value")
	}
}

// containsProblem reports whether any string in problems contains substr. It
// matches on substring (not equality) because problem messages are appended
// with contextual detail (host address in the cleartext message, etc.).
func containsProblem(problems []string, substr string) bool {
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvReadestEmail, "test@example.com")
	t.Setenv(EnvReadestPassword, "testpass")
	// https is required by Validate's cleartext-transport guard unless the
	// host is loopback/link-local. The value here is a non-resolving fake;
	// Validate performs no DNS, so a syntactically valid https URL passes
	// the scheme/host policy. Tests that specifically exercise the cleartext
	// rejection override this env var with their own http:// URL.
	t.Setenv(EnvBookOrbitServerURL, "https://nas:8080")
	t.Setenv(EnvBookOrbitUsername, "reader")
	t.Setenv(EnvBookOrbitPassword, "bpass")
}
