package readest

import (
	"context"
	"errors"
)

// ErrNotImplemented marks stub methods whose real implementation lands in a
// later roadmap phase. It lets the foundation compile and wire interfaces
// without any premature network behavior.
var ErrNotImplemented = errors.New("readest: not implemented in foundation phase")

// Token is the Supabase credential set persisted across runs. The refresh
// token rotates on every refresh, so the whole set must be saved atomically.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt is the Unix epoch second at which the access token expires.
	ExpiresAt int64 `json:"expires_at"`
	// ExpiresIn is the token's lifetime in seconds, used for the 50% TTL rule.
	ExpiresIn int64 `json:"expires_in"`
}

// ExpiresWithin reports whether the access token will expire within the given
// number of seconds from `now`. The reference plugin treats a token expiring
// within 60 seconds as already expired.
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

// Authenticator obtains and refreshes the Supabase access token used to call
// the Readest sync API. The sync engine depends on this interface only.
type Authenticator interface {
	// SignIn performs the Supabase password grant and returns a fresh token.
	SignIn(ctx context.Context) (Token, error)
	// Refresh exchanges the stored refresh token for a new token set.
	Refresh(ctx context.Context) (Token, error)
	// AccessToken returns a valid access token, refreshing proactively at the
	// 50% TTL threshold or when the token expires within 60 seconds.
	AccessToken(ctx context.Context) (string, error)
}

// Auth is the foundation-phase stub Authenticator. Its construction validates
// the inputs a real implementation needs, but every method returns
// ErrNotImplemented so no premature network behavior exists.
type Auth struct {
	supabaseURL string
	anonKey     string
	email       string
	password    string
}

// NewAuth constructs the stub authenticator. The parameters are captured so
// the Phase 3 implementation can use them without changing the constructor.
func NewAuth(supabaseURL, anonKey, email, password string) *Auth {
	return &Auth{
		supabaseURL: supabaseURL,
		anonKey:     anonKey,
		email:       email,
		password:    password,
	}
}

// SignIn is a stub; the Supabase password grant is implemented in Phase 3.
func (a *Auth) SignIn(ctx context.Context) (Token, error) {
	return Token{}, ErrNotImplemented
}

// Refresh is a stub; the Supabase refresh grant is implemented in Phase 3.
func (a *Auth) Refresh(ctx context.Context) (Token, error) {
	return Token{}, ErrNotImplemented
}

// AccessToken is a stub; token freshness logic is implemented in Phase 3.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	return "", ErrNotImplemented
}
