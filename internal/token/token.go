// Package token owns the credential set the bridge persists across runs. It is
// deliberately minimal and provider-agnostic: it holds only the token data and
// its pure freshness rules, with no authentication, HTTP, persistence, or
// configuration logic. Both internal/readest (which obtains tokens) and
// internal/sync/state (which persists them) depend on this package, which keeps
// the dependency graph acyclic.
package token

// Token is the credential set persisted across runs. The refresh token rotates
// on every refresh, so the whole set must be saved atomically. Field names and
// JSON tags match the provider's wire shape.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is the Unix epoch second at which the access token expires.
	ExpiresAt int64 `json:"expires_at"`
	// ExpiresIn is the token's lifetime in seconds, used for the 50% TTL rule.
	ExpiresIn int64 `json:"expires_in"`
}

// ExpiresWithin reports whether the access token will expire within the given
// number of seconds from `now`. A token expiring within 60 seconds is treated
// as already expired by the bridge's pre-request guard.
func (t Token) ExpiresWithin(now, seconds int64) bool {
	return t.ExpiresAt < now+seconds
}

// ShouldRefresh reports whether the token has passed the proactive-refresh
// threshold: refresh when fewer than half the TTL remains. This is the exact
// rule the reference plugin's withFreshToken applies.
func (t Token) ShouldRefresh(now int64) bool {
	if t.ExpiresIn <= 0 {
		return true
	}
	return t.ExpiresAt < now+t.ExpiresIn/2
}

// IsZero reports whether the token carries no usable credential. A zero token
// is the first-run condition (nothing persisted yet) and triggers sign-in.
func (t Token) IsZero() bool {
	return t.AccessToken == "" && t.RefreshToken == ""
}
