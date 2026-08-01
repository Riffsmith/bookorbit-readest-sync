package config

import "time"

// Config is the root of the bridge configuration. It maps directly onto the
// sections of configs/bridge.example.yaml and is populated from a YAML file,
// environment-variable overrides, and built-in defaults.
type Config struct {
	Readest   ReadestConfig   `yaml:"readest"`
	BookOrbit BookOrbitConfig `yaml:"bookorbit"`
	Bridge    BridgeConfig    `yaml:"bridge"`
}

// ReadestConfig holds everything needed to authenticate to Readest's Supabase
// backend and pull the library via the sync API.
type ReadestConfig struct {
	// Email and Password are the account credentials for the Supabase
	// password grant. They may be supplied via environment variables instead
	// of the file to keep secrets out of version control.
	Email    string `yaml:"email"`
	Password string `yaml:"password"`

	// SupabaseURL is the base URL of the Supabase project (auth endpoint).
	SupabaseURL string `yaml:"supabase_url"`
	// SupabaseAnonKey is the decoded Supabase anon (public) key sent as the
	// `apikey` header. The reference plugin ships this base64-encoded; the
	// config accepts the decoded value, or the base64 form via the env var.
	SupabaseAnonKey string `yaml:"supabase_anon_key"`
	// SyncBaseURL is the base URL of the Readest sync API.
	SyncBaseURL string `yaml:"sync_base_url"`
}

// BookOrbitConfig holds everything needed to reach a self-hosted BookOrbit
// server and authenticate with its static x-auth headers.
type BookOrbitConfig struct {
	// ServerURL is the base URL of the BookOrbit server; it is normalized so
	// the "/api/v1" prefix appears exactly once.
	ServerURL string `yaml:"server_url"`
	// Username is the BookOrbit account name sent as `x-auth-user`.
	Username string `yaml:"username"`
	// Password is the account password; it is MD5-hashed to form `x-auth-key`.
	// Mutually exclusive with Userkey. Prefer env-var injection over the file.
	Password string `yaml:"password"`
	// Userkey is a pre-hashed lowercase-hex MD5 of the password. When set, it
	// is used verbatim and Password is ignored.
	Userkey string `yaml:"userkey"`
	// DeviceName is the human-readable device string shown in BookOrbit.
	DeviceName string `yaml:"device_name"`
	// DeviceID is the stable identifier for this bridge instance. If empty,
	// one is generated on first run and persisted in the state file.
	DeviceID string `yaml:"device_id"`
}

// BridgeConfig holds daemon behavior and tuning knobs.
type BridgeConfig struct {
	// PollInterval is how often the sync engine runs in daemon mode.
	PollInterval time.Duration `yaml:"poll_interval"`
	// LogLevel is the minimum log level: debug, info, warn, or error.
	LogLevel string `yaml:"log_level"`
	// LogFormat selects the log encoding: "json" or "text".
	LogFormat string `yaml:"log_format"`
	// StateFile is the path to the JSON state file (tokens, match cache,
	// watermarks, unmatched cooldown). Written with 0600 permissions.
	StateFile string `yaml:"state_file"`

	// MatchBatchSize caps hashes per BookOrbit match-check request.
	MatchBatchSize int `yaml:"match_batch_size"`
	// ProgressBatchSize caps items per BookOrbit bulk-progress request.
	ProgressBatchSize int `yaml:"progress_batch_size"`
	// MaxBodyBytes caps the encoded request body sent to BookOrbit.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`

	// UnmatchedCooldown is how long an unmatched hash is skipped before a
	// fresh match-check is attempted.
	UnmatchedCooldown time.Duration `yaml:"unmatched_cooldown"`

	// SyncStatus gates the optional Readest → BookOrbit reading-status push
	// (Phase 9). Off by default: unlike progress (purely informational), a
	// status write changes what the operator's BookOrbit catalog displays as
	// the book's state, so it is opt-in. See docs/phase-9-status-sync-design.md
	// Decision F.
	SyncStatus bool `yaml:"sync_status"`

	// HTTPTimeout is the per-request timeout for API calls.
	HTTPTimeout time.Duration `yaml:"http_timeout"`
	// RetryMaxAttempts bounds transient-failure retries per request.
	RetryMaxAttempts int `yaml:"retry_max_attempts"`
	// RetryInitialBackoff is the starting backoff between retries.
	RetryInitialBackoff time.Duration `yaml:"retry_initial_backoff"`
	// RetryMaxBackoff caps the exponential backoff.
	RetryMaxBackoff time.Duration `yaml:"retry_max_backoff"`
}
