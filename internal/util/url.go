package util

import (
	"net/url"
	"strings"
)

// NormalizeBookOrbitURL canonicalizes a user-supplied BookOrbit server base
// URL so that the API prefix "/api/v1" appears exactly once.
//
// It mirrors the reference plugin's normalizeServerUrl:
//   - trims whitespace and strips trailing slashes
//   - collapses a trailing "/api/v1/koreader" to "/api/v1"
//   - appends "/api/v1" when the path does not already end with it
//
// As of the security-hardening pass, the URL is parsed with net/url.Parse
// before any string manipulation so that unparseable inputs, URLs with no
// scheme, URLs carrying userinfo (user:pass@host), and URLs with a fragment
// are all rejected with an empty return instead of being silently concatenated
// into a request URL. The empty return is the sentinel config.Validate already
// reacts to for "missing URL": the BookOrbit client can never build a request
// from a malformed base, so config load is the right place to surface the
// problem.
//
// The scheme is preserved as given; the policy check ("is http allowed here?")
// lives in internal/config, not here.
func NormalizeBookOrbitURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		// Unparseable or scheme-less: reject. Schemes besides http/https are
		// also caught here only incidentally (they parse cleanly); the
		// explicit scheme allowlist is enforced in config.Validate via
		// util.SchemeOf, which is the cleaner place to surface "must use http
		// or https" messages.
		return ""
	}
	// Reject credentials baked into the URL. The BookOrbit protocol authenticates
	// via x-auth-user / x-auth-key headers, never via URL userinfo; a URL like
	// http://user:pass@nas/api/v1 would today have its userinfo silently
	// discarded by the HTTP client, masking the operator's mistaken belief
	// that they configured auth.
	if u.User != nil {
		return ""
	}
	// Reject a fragment. A fragment on an API base URL is meaningless and
	// indicates a paste error.
	if u.Fragment != "" {
		return ""
	}

	// Operate on the reassembled URL so userinfo/fragment are definitely gone.
	// (Steps above already returned in those cases, so the String() call is
	// defensive.)
	s := u.String()

	// Strip trailing slashes.
	s = strings.TrimRight(s, "/")

	// Collapse a redundant koreader suffix that some users copy in.
	for _, suffix := range []string{"/api/v1/koreader"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
		}
	}

	// Ensure the API prefix is present exactly once.
	if !strings.HasSuffix(s, "/api/v1") {
		// If the path already contains "/api/v1" mid-string (unusual), leave it.
		if !strings.Contains(s, "/api/v1") {
			s += "/api/v1"
		}
	}
	return s
}
