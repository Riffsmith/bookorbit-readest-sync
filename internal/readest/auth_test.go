package readest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/token"
)

// stubDoer is a scriptable httpclient.Doer. Each queued step is invoked in
// order; a step either returns a response or an error. It records every
// request it sees so tests can assert on headers and bodies.
type stubDoer struct {
	mu       sync.Mutex
	steps    []stubStep
	calls    int
	requests []*http.Request
}

type stubStep struct {
	status int
	body   string
	err    error
	// delay, when > 0, makes Do wait (respecting the request context) before
	// responding, so tests can exercise per-call deadlines.
	delay time.Duration
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls++
	d.requests = append(d.requests, req)
	d.mu.Unlock()

	if d.calls > len(d.steps) {
		return nil, errors.New("stubDoer: unexpected extra request")
	}
	step := d.steps[d.calls-1]
	if step.err != nil {
		return nil, step.err
	}
	if step.delay > 0 {
		timer := time.NewTimer(step.delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	return &http.Response{
		StatusCode: step.status,
		Body:       io.NopCloser(strings.NewReader(step.body)),
		Header:     make(http.Header),
	}, nil
}

// requestCount reports how many requests the doer has served so far. It is
// safe to call concurrently with in-flight AccessToken calls.
func (d *stubDoer) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// fixedClock returns a controllable clock for deterministic time-based tests.
type fixedClock struct {
	now int64
}

func (c *fixedClock) set(v int64) { atomic.StoreInt64(&c.now, v) }
func (c *fixedClock) get() int64  { return atomic.LoadInt64(&c.now) }

// newTestAuth builds an Auth wired to the stub doer, a MemStore, a discard
// logger, and the fixed clock, and returns all the pieces a test needs.
func newTestAuth(t *testing.T, doer *stubDoer, store state.Store, clock *fixedClock) *Auth {
	t.Helper()
	if clock == nil {
		clock = &fixedClock{now: 1_000_000}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := NewAuth("https://supabase.test", "anon-key", "user@example.com", "pw", store, doer, log)
	a.now = clock.get
	return a
}

// tokenBody renders a Supabase token response JSON body.
func tokenBody(access, refresh string, expiresAt, expiresIn int64) string {
	return `{"access_token":"` + access + `","refresh_token":"` + refresh +
		`","expires_at":` + itoa(expiresAt) + `,"expires_in":` + itoa(expiresIn) + `,"user":{"id":"u1"}}`
}

// itoa is a tiny int64→string helper to avoid importing strconv for two uses.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestSignInSuccess(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-1", "refresh-1", 2_000_000, 3600)},
	}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	tok, err := a.SignIn(context.Background())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if tok.AccessToken != "access-1" || tok.RefreshToken != "refresh-1" {
		t.Errorf("token = %+v", tok)
	}
	if tok.ExpiresAt != 2_000_000 || tok.ExpiresIn != 3600 {
		t.Errorf("expiry fields = %+v", tok)
	}
	// Persisted via the store.
	if got := store.Token(); got != tok {
		t.Errorf("persisted token = %+v, want %+v", got, tok)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1", doer.requestCount())
	}
}

func TestSignInInvalidCredentials(t *testing.T) {
	for _, status := range []int{400, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			doer := &stubDoer{steps: []stubStep{{status: status, body: `{"error":"invalid_grant"}`}}}
			store := state.NewMemStore()
			a := newTestAuth(t, doer, store, nil)

			_, err := a.SignIn(context.Background())
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Errorf("err = %v, want ErrInvalidCredentials", err)
			}
			if got := store.Token(); !got.IsZero() {
				t.Errorf("nothing should be persisted, got %+v", got)
			}
		})
	}
}

func TestSignInNetworkError(t *testing.T) {
	netErr := errors.New("connection refused")
	doer := &stubDoer{steps: []stubStep{{err: netErr}}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	_, err := a.SignIn(context.Background())
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("err = %v, want ErrNetwork", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err should carry underlying detail: %v", err)
	}
	if got := store.Token(); !got.IsZero() {
		t.Errorf("nothing should be persisted, got %+v", got)
	}
}

func TestSignInMalformedJSON(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"access_token":`}}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	_, err := a.SignIn(context.Background())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("err = %v, want ErrMalformedResponse", err)
	}
	if got := store.Token(); !got.IsZero() {
		t.Errorf("nothing should be persisted, got %+v", got)
	}
}

func TestSignInMissingAccessToken(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: `{"refresh_token":"r","expires_at":2000,"expires_in":3600}`},
	}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	_, err := a.SignIn(context.Background())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("err = %v, want ErrMalformedResponse", err)
	}
	if got := store.Token(); !got.IsZero() {
		t.Errorf("nothing should be persisted, got %+v", got)
	}
}

func TestSignInDerivesExpiresAt(t *testing.T) {
	// expires_at absent but expires_in present → derive now+expires_in.
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: `{"access_token":"a","refresh_token":"r","expires_in":3600}`},
	}}
	store := state.NewMemStore()
	clock := &fixedClock{now: 5_000}
	a := newTestAuth(t, doer, store, clock)

	tok, err := a.SignIn(context.Background())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	if tok.ExpiresAt != 5_000+3600 {
		t.Errorf("ExpiresAt = %d, want derived %d", tok.ExpiresAt, 5_000+3600)
	}
}

func TestRefreshSuccessRotatesToken(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-2", "refresh-2", 3_000_000, 3600)},
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: 1, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, nil)

	tok, err := a.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.RefreshToken != "refresh-2" {
		t.Errorf("refresh token should rotate, got %q", tok.RefreshToken)
	}
	// The whole new set (access + rotated refresh) persisted atomically.
	if got := store.Token(); got.AccessToken != "access-2" || got.RefreshToken != "refresh-2" {
		t.Errorf("persisted = %+v, want rotated set", got)
	}
}

func TestRefreshSendsStoredRefreshToken(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("a", "r2", 3_000_000, 3600)},
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "x", RefreshToken: "stored-refresh", ExpiresAt: 1, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, nil)

	if _, err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	req := doer.requests[0]
	body, _ := io.ReadAll(req.Body)
	if !strings.Contains(string(body), `"refresh_token":"stored-refresh"`) {
		t.Errorf("request body = %s, want stored refresh token", body)
	}
	if !strings.Contains(req.URL.RawQuery, "grant_type=refresh_token") {
		t.Errorf("query = %s, want grant_type=refresh_token", req.URL.RawQuery)
	}
}

func TestRefreshInvalidToken(t *testing.T) {
	for _, status := range []int{400, 401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			doer := &stubDoer{steps: []stubStep{{status: status, body: `{"error":"invalid_grant"}`}}}
			store := state.NewMemStore()
			store.SetToken(token.Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: 1, ExpiresIn: 3600})
			a := newTestAuth(t, doer, store, nil)

			_, err := a.Refresh(context.Background())
			if !errors.Is(err, ErrInvalidRefreshToken) {
				t.Errorf("err = %v, want ErrInvalidRefreshToken", err)
			}
		})
	}
}

func TestRefreshNetworkErrorNoFallback(t *testing.T) {
	// Network error on refresh must NOT trigger a sign-in (would mask a
	// transient outage as a credential problem).
	doer := &stubDoer{steps: []stubStep{{err: errors.New("timeout")}}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "a", RefreshToken: "r", ExpiresAt: 1, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, nil)

	_, err := a.Refresh(context.Background())
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("err = %v, want ErrNetwork", err)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want exactly 1 (no fallback)", doer.requestCount())
	}
}

func TestAccessTokenFirstRunSignsIn(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-1", "refresh-1", 2_000_000, 3600)},
	}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	got, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "access-1" {
		t.Errorf("AccessToken = %q, want access-1", got)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 sign-in", doer.requestCount())
	}
	// Sign-in request used the password grant.
	if !strings.Contains(doer.requests[0].URL.RawQuery, "grant_type=password") {
		t.Errorf("query = %s, want grant_type=password", doer.requests[0].URL.RawQuery)
	}
}

func TestAccessTokenFreshNoHTTP(t *testing.T) {
	clock := &fixedClock{now: 1_000}
	doer := &stubDoer{} // no steps: any request errors out
	store := state.NewMemStore()
	// Fresh: expires at 1_000+3600, TTL 3600 → halfway is now+1800; far off.
	store.SetToken(token.Token{AccessToken: "fresh", RefreshToken: "r", ExpiresAt: 1_000 + 3600, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, clock)

	got, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "fresh" {
		t.Errorf("AccessToken = %q, want existing token", got)
	}
	if doer.requestCount() != 0 {
		t.Errorf("requests = %d, want 0 for a fresh token", doer.requestCount())
	}
}

func TestAccessTokenRefreshesAtHalfTTL(t *testing.T) {
	clock := &fixedClock{now: 1_000}
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-new", "refresh-new", 1_000+3600, 3600)},
	}}
	store := state.NewMemStore()
	// ExpiresAt 1_500, TTL 3600: halfway threshold is now+1800=2_800; 1_500 < 2_800 → refresh.
	store.SetToken(token.Token{AccessToken: "old", RefreshToken: "r", ExpiresAt: 1_500, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, clock)

	got, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "access-new" {
		t.Errorf("AccessToken = %q, want refreshed", got)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 refresh", doer.requestCount())
	}
	if !strings.Contains(doer.requests[0].URL.RawQuery, "grant_type=refresh_token") {
		t.Errorf("query = %s, want grant_type=refresh_token", doer.requests[0].URL.RawQuery)
	}
}

func TestAccessTokenZeroTTLRefreshes(t *testing.T) {
	// ExpiresIn == 0 → ShouldRefresh is unconditionally true.
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-new", "refresh-new", 9_000_000, 3600)},
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "old", RefreshToken: "r", ExpiresAt: 9_000_000, ExpiresIn: 0})
	a := newTestAuth(t, doer, store, nil)

	got, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "access-new" {
		t.Errorf("AccessToken = %q, want refreshed", got)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 refresh", doer.requestCount())
	}
}

func TestAccessTokenSixtySecondGuard(t *testing.T) {
	// ShouldRefresh false (huge ExpiresIn) but token expires in 30s → guard fires.
	clock := &fixedClock{now: 1_000}
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-new", "refresh-new", 1_000+3600, 3600)},
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "old", RefreshToken: "r", ExpiresAt: 1_030, ExpiresIn: 1_000_000})
	a := newTestAuth(t, doer, store, clock)

	got, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "access-new" {
		t.Errorf("AccessToken = %q, want refreshed via 60s guard", got)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 refresh", doer.requestCount())
	}
}

func TestAccessTokenRefreshFailureFallsBackToSignIn(t *testing.T) {
	// Refresh rejected (invalid refresh token) → isolated reauth helper signs in.
	doer := &stubDoer{steps: []stubStep{
		{status: 400, body: `{"error":"invalid_grant"}`},                                   // refresh fails
		{status: 200, body: tokenBody("access-reauth", "refresh-reauth", 9_000_000, 3600)}, // sign-in succeeds
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "old", RefreshToken: "bad-refresh", ExpiresAt: 1, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, nil)

	got, err := a.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got != "access-reauth" {
		t.Errorf("AccessToken = %q, want re-authenticated token", got)
	}
	if doer.requestCount() != 2 {
		t.Fatalf("requests = %d, want refresh + sign-in", doer.requestCount())
	}
	// First call was a refresh; second was a password sign-in.
	if !strings.Contains(doer.requests[0].URL.RawQuery, "grant_type=refresh_token") {
		t.Errorf("first request should be refresh, query = %s", doer.requests[0].URL.RawQuery)
	}
	if !strings.Contains(doer.requests[1].URL.RawQuery, "grant_type=password") {
		t.Errorf("second request should be sign-in, query = %s", doer.requests[1].URL.RawQuery)
	}
	// The re-authenticated token is persisted.
	if tok := store.Token(); tok.AccessToken != "access-reauth" {
		t.Errorf("persisted = %+v, want re-authenticated token", tok)
	}
}

func TestAccessTokenRefreshAndSignInBothFail(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 400, body: `{"error":"invalid_grant"}`}, // refresh fails
		{status: 401, body: `{"error":"invalid_grant"}`}, // sign-in also fails
	}}
	store := state.NewMemStore()
	original := token.Token{AccessToken: "old", RefreshToken: "bad-refresh", ExpiresAt: 1, ExpiresIn: 3600}
	store.SetToken(original)
	a := newTestAuth(t, doer, store, nil)

	_, err := a.AccessToken(context.Background())
	if err == nil {
		t.Fatal("AccessToken should fail when refresh and sign-in both fail")
	}
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("err = %v, want wrapped ErrInvalidCredentials from sign-in", err)
	}
	// Original token must be left untouched (no corruption of existing state).
	if got := store.Token(); got != original {
		t.Errorf("store should be unchanged, got %+v want %+v", got, original)
	}
}

func TestAccessTokenConcurrentSingleRefresh(t *testing.T) {
	// Many concurrent callers with a stale token must trigger exactly one
	// refresh; all observe the refreshed token.
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-new", "refresh-new", 9_000_000, 3600)},
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "old", RefreshToken: "r", ExpiresAt: 1, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, nil)

	const workers = 16
	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := a.AccessToken(context.Background())
			results[i], errs[i] = tok, err
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d error: %v", i, errs[i])
		}
		if results[i] != "access-new" {
			t.Errorf("worker %d token = %q, want access-new", i, results[i])
		}
	}
	if got := doer.requestCount(); got != 1 {
		t.Errorf("refresh requests = %d, want exactly 1", got)
	}
}

func TestAccessTokenConcurrentFirstRunSingleSignIn(t *testing.T) {
	// Concurrent first-run callers (no token) must trigger exactly one sign-in.
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-1", "refresh-1", 9_000_000, 3600)},
	}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = a.AccessToken(context.Background())
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d error: %v", i, errs[i])
		}
	}
	if got := doer.requestCount(); got != 1 {
		t.Errorf("sign-in requests = %d, want exactly 1", got)
	}
}

func TestInjectedClockControlsRefreshDecision(t *testing.T) {
	// The refresh decision must flip purely on the injected clock. With a token
	// expiring at 10_000 (TTL 3600), the halfway threshold is now+1800: fresh
	// while now+1800 <= 10_000, i.e. now <= 8_200; stale once now advances past it.
	store2 := state.NewMemStore()
	store2.SetToken(token.Token{AccessToken: "stay", RefreshToken: "r", ExpiresAt: 10_000, ExpiresIn: 3600})
	doerFresh := &stubDoer{}
	aFresh := newTestAuth(t, doerFresh, store2, &fixedClock{now: 1_000})
	if got, err := aFresh.AccessToken(context.Background()); err != nil || got != "stay" {
		t.Fatalf("fresh clock: got %q err %v", got, err)
	}
	if doerFresh.requestCount() != 0 {
		t.Errorf("fresh clock should make no request, got %d", doerFresh.requestCount())
	}

	// Advancing the same clock past the threshold flips the decision.
	doerStale := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("refreshed", "r2", 20_000, 3600)},
	}}
	clock := &fixedClock{now: 1_000}
	aStale := newTestAuth(t, doerStale, store2, clock)
	clock.set(9_000) // threshold 9000+1800=10800; 10000 < 10800 → refresh
	got, err := aStale.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("stale clock: %v", err)
	}
	if got != "refreshed" {
		t.Errorf("stale clock token = %q, want refreshed", got)
	}
	if doerStale.requestCount() != 1 {
		t.Errorf("stale clock requests = %d, want 1", doerStale.requestCount())
	}
}

func TestRequiredHeaders(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("a", "r", 9_000_000, 3600)},
	}}
	store := state.NewMemStore()
	a := NewAuth("https://supabase.test", "the-anon-key", "user@example.com", "pw", store, doer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := a.SignIn(context.Background()); err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	req := doer.requests[0]
	if got := req.Header.Get("apikey"); got != "the-anon-key" {
		t.Errorf("apikey header = %q, want the-anon-key", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	// Sign-in/refresh must NOT carry a Bearer token.
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization header should be absent on sign-in, got %q", got)
	}
	// Endpoint path.
	if req.URL.Path != "/auth/v1/token" {
		t.Errorf("path = %q, want /auth/v1/token", req.URL.Path)
	}
}

func TestSignInSendsEmailAndPassword(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("a", "r", 9_000_000, 3600)},
	}}
	store := state.NewMemStore()
	a := NewAuth("https://supabase.test", "k", "user@example.com", "s3cret", store, doer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := a.SignIn(context.Background()); err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	body, _ := io.ReadAll(doer.requests[0].Body)
	s := string(body)
	if !strings.Contains(s, `"email":"user@example.com"`) || !strings.Contains(s, `"password":"s3cret"`) {
		t.Errorf("sign-in body = %s, want email+password", s)
	}
}

func TestRefreshNoStoredToken(t *testing.T) {
	// Refresh with nothing persisted cannot proceed.
	doer := &stubDoer{}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	_, err := a.Refresh(context.Background())
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("err = %v, want ErrInvalidRefreshToken", err)
	}
	if doer.requestCount() != 0 {
		t.Errorf("no HTTP call expected, got %d", doer.requestCount())
	}
}

func TestRateLimited(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 429, body: `{"error":"rate limit"}`}}}
	store := state.NewMemStore()
	a := newTestAuth(t, doer, store, nil)

	_, err := a.SignIn(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestTokenPersistsAtomicallyOnRotation(t *testing.T) {
	// After a refresh, the store must hold the new access AND refresh tokens
	// together — never a mix of old/new.
	doer := &stubDoer{steps: []stubStep{
		{status: 200, body: tokenBody("access-2", "refresh-2", 5_000_000, 3600)},
	}}
	store := state.NewMemStore()
	store.SetToken(token.Token{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresAt: 1, ExpiresIn: 3600})
	a := newTestAuth(t, doer, store, nil)

	if _, err := a.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got := store.Token()
	if got.AccessToken != "access-2" || got.RefreshToken != "refresh-2" || got.ExpiresAt != 5_000_000 {
		t.Errorf("persisted set = %+v, want fully rotated", got)
	}
}
