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
	} else {
		problems = appendReadestURLProblem(problems, "readest.supabase_url", c.Readest.SupabaseURL)
	}
	if c.Readest.SupabaseAnonKey == "" {
		problems = append(problems, "readest.supabase_anon_key must not be empty")
	}
	if c.Readest.SyncBaseURL == "" {
		problems = append(problems, "readest.sync_base_url must not be empty")
	} else {
		problems = appendReadestURLProblem(problems, "readest.sync_base_url", c.Readest.SyncBaseURL)
	}

	// BookOrbit needs a target server, a username, and either a password or a
	// pre-hashed userkey from which to derive x-auth-key.
	if c.BookOrbit.ServerURL == "" {
		problems = append(problems, "bookorbit.server_url is required (or set "+EnvBookOrbitServerURL+")")
	} else {
		// Security-hardening pass: the BookOrbit wire protocol uses an
		// unsalted MD5 of the password as the x-auth-key header — a
		// password-equivalent credential. Confine where that credential
		// is allowed to travel at config load, where the operator can
		// still correct a misconfiguration, instead of silently sending
		// it over cleartext or to an unexpected scheme/host. See
		// docs/security-hardening-bookorbit-url-validation-design.md §3
		// for the settled-scheme (Decisions B/C/D/F).
		problems = appendBookOrbitURLProblems(problems, c)
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

// appendBookOrbitURLProblems adds validation problems for the (already
// non-empty, already-normalized) BookOrbit server_url. The URL is normalized
// before Validate runs (load.go finalize -> util.NormalizeBookOrbitURL), so a
// malformed input has already collapsed to "" and the caller's empty-URL
// check above has produced the "is required" problem. This helper still
// re-derives the scheme from the post-normalization form so it can surface:
//   - an unusable scheme (parse error / missing scheme / non-http(s)), and
//   - a cleartext http:// URL pointing at a non-loopback, non-link-local host
//     unless the operator has explicitly opted in via
//     bookorbit.allow_insecure_transport.
//
// The shape check runs unconditionally; the cleartext check runs only when the
// shape check passed (so an unparseable URL produces one diagnostic, not two).
// See docs/security-hardening-bookorbit-url-validation-design.md §3 Decisions
// B/C/D and §6 Step 7.
func appendBookOrbitURLProblems(problems []string, c *Config) []string {
	scheme, err := util.SchemeOf(c.BookOrbit.ServerURL)
	if err != nil {
		return append(problems, "bookorbit.server_url is not a valid URL: "+err.Error())
	}
	if scheme != "http" && scheme != "https" {
		return append(problems, "bookorbit.server_url must use http or https; got "+scheme)
	}
	if scheme == "http" {
		host := util.HostOf(c.BookOrbit.ServerURL)
		if !util.IsLoopbackOrLinkLocal(host) && !c.BookOrbit.AllowInsecureTransport {
			return append(problems,
				"bookorbit.server_url uses cleartext http to a non-loopback host ("+host+
					"); the x-auth-key header is the MD5 of your password (a password-equivalent credential). "+
					"Use https://, or set bookorbit.allow_insecure_transport: true to acknowledge the cleartext risk.")
		}
	}
	return problems
}

// appendReadestURLProblem adds a validation problem if the (already non-empty)
// Readest endpoint URL is not parseable or does not use https. Unlike
// BookOrbit there is no cleartext opt-out: both endpoints carry Supabase
// password-grant and Bearer-token credentials to a third-party-operated
// service, so https is required outright even for loopback (an operator fully
// in control of the loopback transport can still set it up with a self-signed
// cert). See docs/security-hardening-bookorbit-url-validation-design.md §3
// Decision E.
//
// The defaults in internal/config/defaults.go (https://readest.supabase.co,
// https://web.readest.com/api) already satisfy this; the check is a pure
// misconfiguration guard that fires only on an explicit override.
func appendReadestURLProblem(problems []string, label, rawURL string) []string {
	scheme, err := util.SchemeOf(rawURL)
	if err != nil {
		return append(problems, label+" is not a valid URL: "+err.Error())
	}
	if scheme != "https" {
		return append(problems, label+" must use https (Supabase password grant / Bearer token travel to this endpoint)")
	}
	return problems
}
