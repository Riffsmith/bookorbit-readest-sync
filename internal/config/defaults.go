package config

import "time"

// Built-in defaults. The Readest service endpoints mirror the hardcoded values
// in the reference plugin and are correct for the official hosted service; they
// are exposed in config only for future-proofing (e.g., self-hosted Readest).
const (
	// DefaultSupabaseURL is the official Readest Supabase project URL.
	DefaultSupabaseURL = "https://readest.supabase.co"
	// DefaultSyncBaseURL is the official Readest sync API base.
	DefaultSyncBaseURL = "https://web.readest.com/api"

	// defaultSupabaseAnonKeyBase64 is the official Readest Supabase anon key,
	// base64-encoded exactly as it appears in the reference plugin. This key is
	// public — it merely identifies the Supabase project, it is not a secret —
	// but we keep it base64 so the source of truth matches the plugin verbatim.
	// The decoded form is what actually gets sent as the `apikey` header.
	defaultSupabaseAnonKeyBase64 = "ZXlKaGJHY2lPaUpJVXpJMU5pSXNJblI1Y0NJNklrcFhWQ0o5LmV5SnBjM01pT2lKemRYQmhZbUZ6WlNJc0luSmxaaUk2SW5aaWMzbDRablZ6YW1weFpIaHJhbkZzZVhOaklpd2ljbTlzWlNJNkltRnViMjRpTENKcFlYUWlPakUzTXpReE1qTTJOekVzSW1WNGNDSTZNakEwT1RZNU9UWTNNWDAuM1U1VXFhb3VfMVNnclZlMWVvOXJBcGMwdUtqcWhwUWRVWGh2d1VIbVVmZw=="
)

// Default returns a Config populated with sensible defaults. Credentials are
// intentionally left empty so validation can require them; every behavioral
// knob and service URL gets a working value.
func Default() Config {
	return Config{
		Readest: ReadestConfig{
			SupabaseURL: DefaultSupabaseURL,
			SyncBaseURL: DefaultSyncBaseURL,
			// SupabaseAnonKey is decoded from the base64 constant at Load time.
		},
		BookOrbit: BookOrbitConfig{
			DeviceName: "readest-bridge",
		},
		Bridge: BridgeConfig{
			PollInterval:        15 * time.Minute,
			LogLevel:            "info",
			LogFormat:           "json",
			StateFile:           "bridge-state.json",
			MatchBatchSize:      500,
			ProgressBatchSize:   100,
			MaxBodyBytes:        900 * 1024,
			UnmatchedCooldown:   24 * time.Hour,
			HTTPTimeout:         30 * time.Second,
			RetryMaxAttempts:    5,
			RetryInitialBackoff: 500 * time.Millisecond,
			RetryMaxBackoff:     30 * time.Second,
		},
	}
}
