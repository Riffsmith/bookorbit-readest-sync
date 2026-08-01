package config

import (
	"encoding/base64"
	"os"
	"strconv"
	"strings"
)

// Environment variable names. File values are overridden by these, which lets
// operators keep secrets out of the config file entirely. This is the
// documented mechanism for supplying credentials.
const (
	EnvReadestEmail    = "BRIDGE_READEST_EMAIL"
	EnvReadestPassword = "BRIDGE_READEST_PASSWORD"
	EnvSupabaseURL     = "BRIDGE_SUPABASE_URL"
	EnvSupabaseAnonKey = "BRIDGE_SUPABASE_ANON_KEY" // decoded value
	EnvReadestSyncURL  = "BRIDGE_READEST_SYNC_BASE_URL"

	EnvBookOrbitServerURL  = "BRIDGE_BOOKORBIT_SERVER_URL"
	EnvBookOrbitUsername   = "BRIDGE_BOOKORBIT_USERNAME"
	EnvBookOrbitPassword   = "BRIDGE_BOOKORBIT_PASSWORD"
	EnvBookOrbitUserkey    = "BRIDGE_BOOKORBIT_USERKEY"
	EnvBookOrbitDeviceName = "BRIDGE_BOOKORBIT_DEVICE_NAME"
	EnvBookOrbitDeviceID   = "BRIDGE_BOOKORBIT_DEVICE_ID"

	EnvPollInterval = "BRIDGE_POLL_INTERVAL"
	EnvLogLevel     = "BRIDGE_LOG_LEVEL"
	EnvLogFormat    = "BRIDGE_LOG_FORMAT"
	EnvStateFile    = "BRIDGE_STATE_FILE"
	EnvSyncStatus   = "BRIDGE_SYNC_STATUS"
)

// applyEnv overlays any set environment variables onto the config. Only
// non-empty variables override, so an unset variable never clobbers a file
// value or a default.
func (c *Config) applyEnv() {
	setStr(&c.Readest.Email, EnvReadestEmail)
	setStr(&c.Readest.Password, EnvReadestPassword)
	setStr(&c.Readest.SupabaseURL, EnvSupabaseURL)
	setStr(&c.Readest.SyncBaseURL, EnvReadestSyncURL)
	if v, ok := lookupNonEmpty(EnvSupabaseAnonKey); ok {
		// Accept either the decoded key or its base64 form.
		if decoded, err := base64.StdEncoding.DecodeString(v); err == nil && strings.HasPrefix(string(decoded), "eyJ") {
			c.Readest.SupabaseAnonKey = string(decoded)
		} else {
			c.Readest.SupabaseAnonKey = v
		}
	}

	setStr(&c.BookOrbit.ServerURL, EnvBookOrbitServerURL)
	setStr(&c.BookOrbit.Username, EnvBookOrbitUsername)
	setStr(&c.BookOrbit.Password, EnvBookOrbitPassword)
	setStr(&c.BookOrbit.Userkey, EnvBookOrbitUserkey)
	setStr(&c.BookOrbit.DeviceName, EnvBookOrbitDeviceName)
	setStr(&c.BookOrbit.DeviceID, EnvBookOrbitDeviceID)

	setStr(&c.Bridge.LogLevel, EnvLogLevel)
	setStr(&c.Bridge.LogFormat, EnvLogFormat)
	setStr(&c.Bridge.StateFile, EnvStateFile)
	if v, ok := lookupNonEmpty(EnvPollInterval); ok {
		if d, err := time_ParseDuration(v); err == nil {
			c.Bridge.PollInterval = d
		}
	}
	setBool(&c.Bridge.SyncStatus, EnvSyncStatus)
}

// setStr assigns the value of the named environment variable to *dst when set
// and non-empty.
func setStr(dst *string, name string) {
	if v, ok := lookupNonEmpty(name); ok {
		*dst = v
	}
}

// setBool parses the named environment variable as a bool and assigns it to
// *dst when set and non-empty. An unparseable value is ignored, matching the
// lenient handling the duration override uses (an invalid value never blocks
// startup; validation of the merged config is Validate's job).
func setBool(dst *bool, name string) {
	if v, ok := lookupNonEmpty(name); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

// lookupNonEmpty returns the trimmed value of the named environment variable
// and whether it was set to a non-empty string.
func lookupNonEmpty(name string) (string, bool) {
	v, present := os.LookupEnv(name)
	if !present {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}
