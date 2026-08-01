package config

import (
	"github.com/Riffsmith/bookorbit-readest-sync/internal/util"
)

// normalizeServerURL delegates to the shared util implementation so the
// normalization rule lives in exactly one place.
func normalizeServerURL(raw string) string {
	return util.NormalizeBookOrbitURL(raw)
}

// Validate checks the merged config for the credentials and values the bridge
// cannot operate without, and reports every problem at once rather than
// failing on the first. It is run by Load before the config is returned.
func (c *Config) Validate() error {
	var problems []string

	// Readest credentials are required to obtain a Supabase token.
	if c.Readest.Email == "" {
		problems = append(problems, "readest.email is required (or set "+EnvReadestEmail+")")
	}
	if c.Readest.Password == "" {
		problems = append(problems, "readest.password is required (or set "+EnvReadestPassword+")")
	}
	if c.Readest.SupabaseURL == "" {
		problems = append(problems, "readest.supabase_url must not be empty")
	}
	if c.Readest.SupabaseAnonKey == "" {
		problems = append(problems, "readest.supabase_anon_key must not be empty")
	}
	if c.Readest.SyncBaseURL == "" {
		problems = append(problems, "readest.sync_base_url must not be empty")
	}

	// BookOrbit needs a target server, a username, and either a password or a
	// pre-hashed userkey from which to derive x-auth-key.
	if c.BookOrbit.ServerURL == "" {
		problems = append(problems, "bookorbit.server_url is required (or set "+EnvBookOrbitServerURL+")")
	}
	if c.BookOrbit.Username == "" {
		problems = append(problems, "bookorbit.username is required (or set "+EnvBookOrbitUsername+")")
	}
	if c.BookOrbit.Password == "" && c.BookOrbit.Userkey == "" {
		problems = append(problems, "bookorbit.password or bookorbit.userkey is required (or set "+EnvBookOrbitPassword+"/"+EnvBookOrbitUserkey+")")
	}
	if c.BookOrbit.Userkey != "" && !util.IsMD5Hex(c.BookOrbit.Userkey) {
		problems = append(problems, "bookorbit.userkey must be a 32-character hex MD5 digest")
	}

	// Behavioral values must be sane.
	if c.Bridge.PollInterval <= 0 {
		problems = append(problems, "bridge.poll_interval must be positive")
	}
	if c.Bridge.MatchBatchSize <= 0 {
		problems = append(problems, "bridge.match_batch_size must be positive")
	}
	if c.Bridge.ProgressBatchSize <= 0 {
		problems = append(problems, "bridge.progress_batch_size must be positive")
	}
	if c.Bridge.MaxBodyBytes <= 0 {
		problems = append(problems, "bridge.max_body_bytes must be positive")
	}
	if c.Bridge.HTTPTimeout <= 0 {
		problems = append(problems, "bridge.http_timeout must be positive")
	}
	if c.Bridge.RetryMaxAttempts < 0 {
		problems = append(problems, "bridge.retry_max_attempts must not be negative")
	}
	// bridge.sync_status (a bool, Phase 9) carries no cross-field invariant: any
	// parsed value is valid, so there is intentionally nothing to check here.

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// ValidationError aggregates all configuration problems found during Validate.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	out := "config: invalid configuration:"
	for _, p := range e.Problems {
		out += "\n  - " + p
	}
	return out
}

// AuthKey returns the value to send as BookOrbit's `x-auth-key` header. It is
// the configured userkey verbatim when present, otherwise the lowercase hex
// MD5 of the password. Deriving it here keeps the hashing rule in one place.
func (c *Config) AuthKey() string {
	if c.BookOrbit.Userkey != "" {
		return util.LowerNormal(c.BookOrbit.Userkey)
	}
	return util.MD5Hex(c.BookOrbit.Password)
}
