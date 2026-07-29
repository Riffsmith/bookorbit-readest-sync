package readest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/token"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/util/httpclient"
)

// Sentinel errors for the authentication lifecycle. Each is returned wrapped
// with diagnostic detail (HTTP status, truncated body) via %w so callers can
// match with errors.Is while logs still carry context. The sync engine (Phase
// 6) uses these to distinguish "retry later" (ErrNetwork) from "bad
// credentials, stop" (ErrInvalidCredentials / ErrInvalidRefreshToken).
var (
	// ErrNotImplemented marks stub methods whose real implementation lands in a
	// later roadmap phase. Retained for the still-stubbed Phase 4 sync client.
	ErrNotImplemented = errors.New("readest: not implemented in foundation phase")

	// ErrInvalidCredentials is a 400/401 response to the password grant.
	ErrInvalidCredentials = errors.New("readest: auth: invalid email or password")
	// ErrInvalidRefreshToken is a 400/401/403 response to the refresh grant.
	ErrInvalidRefreshToken = errors.New("readest: auth: refresh token invalid or expired")
	// ErrNetwork is a transport-level failure (DNS, connection, timeout).
	ErrNetwork = errors.New("readest: auth: network error")
	// ErrMalformedResponse is a 2xx response whose body cannot be parsed into a
	// usable token (bad JSON, or a missing required field).
	ErrMalformedResponse = errors.New("readest: auth: malformed response")
	// ErrRateLimited is a 429 response. Not in the documented status list, but
	// Supabase can return it under repeated polling, so it is handled.
	ErrRateLimited = errors.New("readest: auth: rate limited")
)

// preRequestGuardSeconds is the absolute pre-request safety window: a token
// expiring within this many seconds is treated as already expired, independent
// of the proactive 50% TTL math. It mirrors needsLogin in the reference plugin.
const preRequestGuardSeconds = 60

// maxErrorBodyBytes bounds how much of an error response body is read into an
// error message, so a hostile or buggy server cannot flood logs/memory.
const maxErrorBodyBytes = 512

// Token is the credential set this authenticator obtains and persists. It is an
// alias for the shared token.Token type so the authenticator's API reads
// naturally (readest.Token) while the underlying type stays provider-agnostic
// and shared with the state layer.
type Token = token.Token

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
	// ForceRefresh returns a fresh access token, refreshing unconditionally.
	// The sync client uses it after the sync API rejects the current token
	// (a downstream 401/403), where the stored freshness signal can no longer
	// be trusted. Like AccessToken, it falls back to re-authentication from
	// stored credentials when the refresh token is rejected.
	ForceRefresh(ctx context.Context) (string, error)
}

// tokenResponse is the wire shape of both Supabase /token endpoints. Only the
// fields the bridge consumes are modeled; the nested user object is irrelevant
// to a headless bridge and intentionally omitted.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Auth is the concrete Authenticator. It owns the Supabase token lifecycle:
// password sign-in, proactive refresh at the 50% TTL threshold, the 60-second
// pre-request guard, refresh-token rotation, and atomic persistence of every
// new token set. Dependencies (transport, logger, state store, clock) are
// injected so the type is fully testable with a fake transport and clock.
type Auth struct {
	supabaseURL string
	anonKey     string
	email       string
	password    string

	store state.Store
	http  httpclient.Doer
	log   *slog.Logger
	// now returns the current Unix epoch second; injected for deterministic
	// tests. Defaults to time.Now().Unix().
	now func() int64

	// mu serializes the read-check-refresh-write sequence in AccessToken so
	// concurrent callers trigger exactly one refresh (refresh tokens are
	// single-use; a duplicate refresh would be rejected by Supabase).
	mu sync.Mutex
}

// NewAuth constructs an authenticator. supabaseURL is the Supabase project base
// (e.g. https://readest.supabase.co); anonKey is the project's public anon key
// sent as the `apikey` header on every request. store persists tokens
// atomically; hc is the HTTP transport; log receives lifecycle events. A nil
// log falls back to slog.Default().
func NewAuth(supabaseURL, anonKey, email, password string, store state.Store, hc httpclient.Doer, log *slog.Logger) *Auth {
	if log == nil {
		log = slog.Default()
	}
	return &Auth{
		supabaseURL: supabaseURL,
		anonKey:     anonKey,
		email:       email,
		password:    password,
		store:       store,
		http:        hc,
		log:         log,
		now:         func() int64 { return time.Now().Unix() },
	}
}

// SignIn performs the Supabase password grant with the configured email and
// password, persists the resulting token set atomically, and returns it.
func (a *Auth) SignIn(ctx context.Context) (Token, error) {
	tok, err := a.grant(ctx, "password", map[string]string{
		"email":    a.email,
		"password": a.password,
	})
	if err != nil {
		return Token{}, err
	}
	if err := a.persist(tok); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// Refresh exchanges the persisted refresh token for a new token set (with a
// rotated refresh token) and persists it atomically. It returns
// ErrInvalidRefreshToken when the stored refresh token is rejected; it performs
// no sign-in fallback itself — that recovery lives in AccessToken, which owns
// the policy of when re-authentication from stored credentials is appropriate.
func (a *Auth) Refresh(ctx context.Context) (Token, error) {
	current := a.store.Token()
	if current.RefreshToken == "" {
		return Token{}, fmt.Errorf("%w: no refresh token persisted", ErrInvalidRefreshToken)
	}
	tok, err := a.grant(ctx, "refresh_token", map[string]string{
		"refresh_token": current.RefreshToken,
	})
	if err != nil {
		return Token{}, err
	}
	if err := a.persist(tok); err != nil {
		return Token{}, err
	}
	return tok, nil
}

// AccessToken returns a valid access token for use as a Bearer credential,
// signing in on first run and refreshing proactively so the returned token is
// never within 60 seconds of expiry. The read-check-refresh-write sequence is
// serialized by a mutex: concurrent callers block until the first completes
// and then observe the now-fresh token, so only one network refresh ever fires.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	current := a.store.Token()
	now := a.now()

	switch {
	case current.IsZero():
		// First run: nothing persisted, authenticate from credentials.
		tok, err := a.SignIn(ctx)
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil

	case current.ShouldRefresh(now) || current.ExpiresWithin(now, preRequestGuardSeconds):
		// Proactive 50% TTL refresh, or the belt-and-suspenders 60s guard.
		tok, err := a.Refresh(ctx)
		if err == nil {
			return tok.AccessToken, nil
		}
		// An invalid/expired refresh token triggers re-authentication; any
		// other failure (network, malformed, rate limit) is surfaced as-is so
		// the engine can apply its own retry policy.
		if errors.Is(err, ErrInvalidRefreshToken) {
			return a.reauth(ctx)
		}
		return "", err

	default:
		return current.AccessToken, nil
	}
}

// ForceRefresh returns a fresh access token, refreshing regardless of the
// proactive 50%-TTL / 60-second freshness rules. It exists for the sync
// client's downstream-401 path: the sync API has already rejected the current
// token, so the stored freshness signal is untrustworthy and a refresh must be
// forced rather than skipped as "still fresh." A rejected refresh token falls
// back to re-authentication from stored credentials via the same reauth helper
// AccessToken uses, so the headless re-auth policy lives in exactly one place
// rather than being duplicated in the client. The whole sequence is serialized
// by the same mutex so a forced refresh cannot race a concurrent AccessToken
// into a duplicate (single-use) refresh call.
func (a *Auth) ForceRefresh(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	tok, err := a.Refresh(ctx)
	if err == nil {
		return tok.AccessToken, nil
	}
	// Mirror AccessToken's recovery: only an invalid/expired refresh token
	// triggers re-authentication; any other failure is surfaced as-is.
	if errors.Is(err, ErrInvalidRefreshToken) {
		return a.reauth(ctx)
	}
	return "", err
}

// reauth is the single, isolated place where the bridge re-authenticates from
// stored credentials after the refresh token is rejected.
//
// INTENTIONAL DEVIATION FROM THE REFERENCE PLUGIN: the Lua plugin has no such
// fallback — on a failed refresh it surfaces a login prompt and waits for a
// human to re-enter credentials (see docs/phase-3-auth-design.md §8, §14.2).
// The bridge is headless, so an expired refresh token would otherwise stall
// sync until manual intervention. Re-authenticating from the email/password in
// config is the headless analogue of that human re-login. It is deliberately
// confined to this helper and logged loudly so the recovery is never silent.
//
// On total failure the original token is left untouched in the store: the
// method never corrupts existing state.
func (a *Auth) reauth(ctx context.Context) (string, error) {
	a.log.Warn("readest: refresh token rejected; re-authenticating from stored credentials (intentional deviation from plugin behavior)")
	tok, err := a.SignIn(ctx)
	if err != nil {
		return "", fmt.Errorf("readest: auth: refresh failed and re-authentication failed: %w", err)
	}
	a.log.Info("readest: re-authentication succeeded; token set rotated")
	return tok.AccessToken, nil
}

// grant performs one POST to the Supabase token endpoint for the given grant
// type and form fields, and decodes the response into a Token. It classifies
// failures into the package's sentinel errors based on the HTTP status.
func (a *Auth) grant(ctx context.Context, grantType string, fields map[string]string) (Token, error) {
	endpoint, err := url.Parse(a.supabaseURL + "/auth/v1/token")
	if err != nil {
		return Token{}, fmt.Errorf("readest: auth: invalid supabase URL %q: %w", a.supabaseURL, err)
	}
	q := endpoint.Query()
	q.Set("grant_type", grantType)
	endpoint.RawQuery = q.Encode()

	body, err := json.Marshal(fields)
	if err != nil {
		return Token{}, fmt.Errorf("readest: auth: encode %s grant: %w", grantType, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Token{}, fmt.Errorf("readest: auth: build request: %w", err)
	}
	// Every Supabase request needs the anon key; sign-in and refresh do NOT use
	// a Bearer token (that header is only for sign_out / get_user, out of scope).
	req.Header.Set("apikey", a.anonKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("%w: %s grant: %v", ErrNetwork, grantType, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Token{}, classifyStatus(grantType, resp)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Token{}, fmt.Errorf("%w: %s grant: decode: %v", ErrMalformedResponse, grantType, err)
	}
	return tr.token(grantType, a.now())
}

// token converts a decoded tokenResponse into a Token, validating required
// fields and deriving a missing expires_at from expires_in so a zero value
// cannot force ShouldRefresh true forever.
func (tr tokenResponse) token(grantType string, now int64) (token.Token, error) {
	if tr.AccessToken == "" {
		return token.Token{}, fmt.Errorf("%w: %s grant: empty access_token", ErrMalformedResponse, grantType)
	}
	if tr.RefreshToken == "" {
		return token.Token{}, fmt.Errorf("%w: %s grant: empty refresh_token", ErrMalformedResponse, grantType)
	}
	expiresAt := tr.ExpiresAt
	if expiresAt == 0 && tr.ExpiresIn > 0 {
		expiresAt = now + tr.ExpiresIn
	}
	return token.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    expiresAt,
		ExpiresIn:    tr.ExpiresIn,
	}, nil
}

// classifyStatus maps a non-200 auth response to the appropriate sentinel
// error, attaching the status code and a truncated body for diagnostics.
func classifyStatus(grantType string, resp *http.Response) error {
	snippet := readErrorSnippet(resp.Body)

	var sentinel error
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		sentinel = ErrRateLimited
	case grantType == "password" && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized):
		sentinel = ErrInvalidCredentials
	case grantType == "refresh_token" && (resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden):
		sentinel = ErrInvalidRefreshToken
	default:
		// An unclassified status is treated as transient so the engine retries.
		sentinel = ErrNetwork
	}

	return fmt.Errorf("%w: %s grant: status %d: %s", sentinel, grantType, resp.StatusCode, snippet)
}

// readErrorSnippet reads up to maxErrorBodyBytes of an error body for logging.
func readErrorSnippet(body io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	if err != nil {
		return "<unreadable body>"
	}
	return string(bytes.TrimSpace(b))
}

// persist saves a freshly obtained token set atomically via the store.
func (a *Auth) persist(tok Token) error {
	a.store.SetToken(tok)
	if err := a.store.Save(); err != nil {
		return fmt.Errorf("readest: auth: persist token: %w", err)
	}
	return nil
}
