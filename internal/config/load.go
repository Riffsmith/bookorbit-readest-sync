package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// ErrNoConfigFile is returned by Load when the given path does not exist. The
// CLI treats this as a normal "run on defaults + env" condition rather than a
// fatal error, so callers can distinguish it from a malformed-file error.
var ErrNoConfigFile = errors.New("config: file not found")

// Load reads configuration from the given YAML path, applies defaults for any
// unset value, then overlays environment-variable overrides. The returned
// config is validated before being handed back.
//
// A missing file is not an error: Load returns defaults + env with
// ErrNoConfigFile wrapped in the error, and the config is still usable. A file
// that exists but cannot be parsed returns a hard error.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Proceed with defaults + env, but signal the miss. Validation
				// still applies: running with no credentials is a hard error
				// even when the file is absent.
				cfg.applyEnv()
				cfg.finalize()
				if verr := cfg.Validate(); verr != nil {
					return Config{}, verr
				}
				return cfg, fmt.Errorf("%w: %s", ErrNoConfigFile, path)
			}
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
		values, err := parseSimpleYAML(data)
		if err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
		if err := cfg.applyValues(values); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
	}

	cfg.applyEnv()
	cfg.finalize()

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// finalize performs post-merge normalization that must run after both file and
// env values are in place.
func (c *Config) finalize() {
	// Decode the default anon key only if the operator has not supplied one.
	if c.Readest.SupabaseAnonKey == "" {
		if decoded, err := base64.StdEncoding.DecodeString(defaultSupabaseAnonKeyBase64); err == nil {
			c.Readest.SupabaseAnonKey = string(decoded)
		}
	}
	// Normalize the BookOrbit server URL so "/api/v1" appears exactly once.
	c.BookOrbit.ServerURL = normalizeServerURL(c.BookOrbit.ServerURL)
}

// applyValues assigns parsed YAML scalars onto the config. Unknown keys are
// rejected so a typo in the config file surfaces immediately rather than being
// silently ignored.
func (c *Config) applyValues(v map[string]string) error {
	for key, val := range v {
		switch key {
		// Readest.
		case "readest.email":
			c.Readest.Email = val
		case "readest.password":
			c.Readest.Password = val
		case "readest.supabase_url":
			c.Readest.SupabaseURL = val
		case "readest.supabase_anon_key":
			c.Readest.SupabaseAnonKey = val
		case "readest.sync_base_url":
			c.Readest.SyncBaseURL = val

		// BookOrbit.
		case "bookorbit.server_url":
			c.BookOrbit.ServerURL = val
		case "bookorbit.username":
			c.BookOrbit.Username = val
		case "bookorbit.password":
			c.BookOrbit.Password = val
		case "bookorbit.userkey":
			c.BookOrbit.Userkey = val
		case "bookorbit.device_name":
			c.BookOrbit.DeviceName = val
		case "bookorbit.device_id":
			c.BookOrbit.DeviceID = val
		case "bookorbit.allow_insecure_transport":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("bookorbit.allow_insecure_transport: %w", err)
			}
			c.BookOrbit.AllowInsecureTransport = b

		// Bridge.
		case "bridge.poll_interval":
			d, err := time_ParseDuration(val)
			if err != nil {
				return fmt.Errorf("bridge.poll_interval: %w", err)
			}
			c.Bridge.PollInterval = d
		case "bridge.log_level":
			c.Bridge.LogLevel = val
		case "bridge.log_format":
			c.Bridge.LogFormat = val
		case "bridge.state_file":
			c.Bridge.StateFile = val
		case "bridge.match_batch_size":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("bridge.match_batch_size: %w", err)
			}
			c.Bridge.MatchBatchSize = n
		case "bridge.progress_batch_size":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("bridge.progress_batch_size: %w", err)
			}
			c.Bridge.ProgressBatchSize = n
		case "bridge.max_body_bytes":
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return fmt.Errorf("bridge.max_body_bytes: %w", err)
			}
			c.Bridge.MaxBodyBytes = n
		case "bridge.unmatched_cooldown":
			d, err := time_ParseDuration(val)
			if err != nil {
				return fmt.Errorf("bridge.unmatched_cooldown: %w", err)
			}
			c.Bridge.UnmatchedCooldown = d
		case "bridge.sync_status":
			b, err := strconv.ParseBool(val)
			if err != nil {
				return fmt.Errorf("bridge.sync_status: %w", err)
			}
			c.Bridge.SyncStatus = b
		case "bridge.http_timeout":
			d, err := time_ParseDuration(val)
			if err != nil {
				return fmt.Errorf("bridge.http_timeout: %w", err)
			}
			c.Bridge.HTTPTimeout = d
		case "bridge.retry_max_attempts":
			n, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("bridge.retry_max_attempts: %w", err)
			}
			c.Bridge.RetryMaxAttempts = n
		case "bridge.retry_initial_backoff":
			d, err := time_ParseDuration(val)
			if err != nil {
				return fmt.Errorf("bridge.retry_initial_backoff: %w", err)
			}
			c.Bridge.RetryInitialBackoff = d
		case "bridge.retry_max_backoff":
			d, err := time_ParseDuration(val)
			if err != nil {
				return fmt.Errorf("bridge.retry_max_backoff: %w", err)
			}
			c.Bridge.RetryMaxBackoff = d

		default:
			return fmt.Errorf("unknown configuration key %q", key)
		}
	}
	return nil
}
