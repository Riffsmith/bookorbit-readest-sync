package main

import (
	"bytes"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/config"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/readest"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != version {
		t.Errorf("stdout = %q, want %q", got, version)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunHelp(t *testing.T) {
	for _, flagName := range []string{"-h", "--help"} {
		t.Run(flagName, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run([]string{flagName}, &stdout, &stderr)
			if err != nil {
				t.Fatalf("run(%s) = %v, want nil (help is a successful invocation)", flagName, err)
			}
			// The flag package prints its auto-generated usage to the
			// FlagSet's configured output (stderr here); this is idiomatic
			// and matches --help's usual Unix behavior.
			if !strings.Contains(stderr.String(), "Usage") {
				t.Errorf("stderr = %q, want usage text", stderr.String())
			}
		})
	}
}

func TestRunUnknownFlagIsNotClassifiedAsHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"--this-flag-does-not-exist"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() = nil, want a parse error for an unknown flag")
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Error("an unknown flag must not be classified as flag.ErrHelp")
	}
}

func TestModeName(t *testing.T) {
	cases := []struct {
		once, daemon bool
		want         string
	}{
		{once: true, daemon: false, want: "once"},
		{once: true, daemon: true, want: "once"},
		{once: false, daemon: false, want: "daemon"},
		{once: false, daemon: true, want: "daemon"}, // --daemon is a documentation-only flag
	}
	for _, c := range cases {
		if got := modeName(c.once, c.daemon); got != c.want {
			t.Errorf("modeName(%v, %v) = %q, want %q", c.once, c.daemon, got, c.want)
		}
	}
}

func TestRunMissingConfigInsufficientEnvFailsValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	err := run([]string{"--config", configPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() = nil, want a validation error")
	}
	var verr *config.ValidationError
	if !errors.As(err, &verr) {
		t.Errorf("err = %v, want *config.ValidationError", err)
	}
}

// TestRunOnceFullWiringSurfacesClassifiedError exercises the entire
// construction chain (config -> state -> device id -> Readest/BookOrbit
// clients -> engine -> RunOnce -> error propagation) against a local test
// server, with zero mocking — matching the project's existing testing
// convention (stubbed transports, no new integration-test tier).
//
// The sync endpoint deliberately returns 400 (a non-retryable classification
// per internal/sync/engine.go's classifyReadestErr) rather than refusing the
// connection, so the failure is immediate and deterministic instead of
// waiting out the engine's real exponential backoff against an unreachable
// host.
func TestRunOnceFullWiringSurfacesClassifiedError(t *testing.T) {
	mux := http.NewServeMux()
	// Supabase password grant: let auth succeed so PullBooks actually
	// reaches the sync call below.
	mux.HandleFunc("/auth/v1/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok","refresh_token":"ref","expires_at":9999999999,"expires_in":3600}`))
	})
	// Readest sync pull: fail fast and non-retryably.
	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"since must be an integer"}`))
	})
	// BookOrbit startup connectivity probe: just succeed, it's non-fatal
	// either way and isn't what this test is asserting on.
	mux.HandleFunc("/api/v1/koreader/users/auth", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv(config.EnvReadestEmail, "user@example.com")
	t.Setenv(config.EnvReadestPassword, "pw")
	t.Setenv(config.EnvSupabaseURL, srv.URL)
	t.Setenv(config.EnvReadestSyncURL, srv.URL)
	t.Setenv(config.EnvBookOrbitServerURL, srv.URL)
	t.Setenv(config.EnvBookOrbitUsername, "reader")
	t.Setenv(config.EnvBookOrbitPassword, "bpass")
	t.Setenv(config.EnvStateFile, filepath.Join(t.TempDir(), "state.json"))

	var stdout, stderr bytes.Buffer
	// The config file itself is missing, which exercises config.Load's
	// non-fatal ErrNoConfigFile path all the way through engine construction.
	configPath := filepath.Join(t.TempDir(), "missing.yaml")

	err := run([]string{"--once", "--config", configPath}, &stdout, &stderr)
	if err == nil {
		t.Fatal("run() = nil, want the classified pull error to propagate")
	}
	if !errors.Is(err, readest.ErrBadRequest) {
		t.Errorf("err = %v, want wrapped readest.ErrBadRequest", err)
	}
	if !strings.Contains(stderr.String(), "warning") {
		t.Errorf("stderr = %q, want the missing-config-file warning", stderr.String())
	}
}
