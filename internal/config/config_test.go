package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
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
  server_url: "http://nas:8080/"
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
	if cfg.BookOrbit.ServerURL != "http://nas:8080/api/v1" {
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

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvReadestEmail, "test@example.com")
	t.Setenv(EnvReadestPassword, "testpass")
	t.Setenv(EnvBookOrbitServerURL, "http://nas:8080")
	t.Setenv(EnvBookOrbitUsername, "reader")
	t.Setenv(EnvBookOrbitPassword, "bpass")
}
