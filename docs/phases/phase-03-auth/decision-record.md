Phase 3 is complete and verified. Here is the completion report.

  1. Package walkthrough

  Work stayed inside the existing flat internal/readest package, plus one new package you approved.

  - internal/readest/auth.go — the full authentication layer. Holds the sentinel errors, the tokenResponse wire type, the Authenticator interface, and the concrete Auth with SignIn / Refresh
  / AccessToken, plus small focused helpers: grant (shared HTTP+decode for both endpoints), classifyStatus (status→sentinel mapping), readErrorSnippet (bounded error bodies), persist (atomic
  store write), and reauth (the isolated deviation helper). Token is now a type alias (type Token = token.Token) so the public API still reads readest.Token.
  - internal/token/token.go (new, Option 2) — minimal, provider-agnostic. Owns only Token and its three pure methods ShouldRefresh, ExpiresWithin, IsZero. No auth, HTTP, persistence, or
  config logic. Both readest and state depend on it, which breaks the import cycle cleanly.

  No changes to client.go, models.go, doc.go, or any BookOrbit/sync code — those remain Phase 4/6 stubs.

  2. Public API walkthrough

  func NewAuth(supabaseURL, anonKey, email, password string, store state.Store, hc httpclient.Doer, log *slog.Logger) *Auth
  func (a *Auth) SignIn(ctx context.Context) (Token, error)
  func (a *Auth) Refresh(ctx context.Context) (Token, error)
  func (a *Auth) AccessToken(ctx context.Context) (string, error)
  Sentinel errors (all errors.Is-matchable, wrapped with status + truncated body): ErrInvalidCredentials, ErrInvalidRefreshToken, ErrNetwork, ErrMalformedResponse, ErrRateLimited. Token,
  Authenticator, ShouldRefresh, ExpiresWithin are unchanged in behavior (freshness logic now lives on token.Token; IsZero was exported so AccessToken can detect first-run).

  3. Constructor changes and rationale

  NewAuth grew from 4 to 7 params, adding store state.Store, hc httpclient.Doer, log *slog.Logger — the dependencies §9 of the design flagged. Callers updated cleanly:
  - cmd/bridge/main.go — passes the already-built st, a shared httpDoer, and log. (I hoisted httpDoer one line so it's defined before use.)
  - internal/readest/models_test.go — the obsolete stub test was removed (real auth is now covered in auth_test.go); the client stub test's NewAuth call was updated.

  The clock is a field (a.now func() int64), defaulting to time.Now().Unix() and settable in tests — matching the injectable-clock decision.

  4. Concurrency design

  A sync.Mutex wraps the entire read-check-refresh-write sequence in AccessToken. Concurrent callers serialize: the first refreshes (or signs in), the rest then observe the now-fresh token
  and return it without a second network call. This matters because Supabase refresh tokens are single-use — a duplicate refresh would be rejected. Verified by two -race tests: 16 concurrent
  callers with a stale token fire exactly 1 refresh; 16 concurrent first-run callers fire exactly 1 sign-in.

  5. Token lifecycle

  First run (zero token) → SignIn (password grant). Otherwise, AccessToken refreshes when ShouldRefresh (50% TTL) or ExpiresWithin(now, 60) (pre-request guard) is true, else returns the
  stored token. Both SignIn and Refresh persist the full set atomically via store.SetToken + store.Save() — never a partial field update, so the rotated refresh token is always saved with its
  access token. A missing expires_at is derived as now + expires_in so a zero value can't force perpetual refresh.

  6. Testing summary

  internal/readest/auth_test.go uses a scriptable stub Doer (records requests, queues responses/errors) and a controllable fixedClock — fully deterministic, no real network. Covers the
  complete §13 matrix: sign-in success / invalid credentials (400, 401) / network / malformed JSON / missing access_token / derived expires_at; refresh success + rotation + atomic persist /
  sends stored token / invalid token (400, 401, 403) / network-no-fallback / no-stored-token; AccessToken first-run, fresh-no-HTTP, half-TTL, zero-TTL, 60s guard, refresh→sign-in fallback,
  both-fail-leaves-store-untouched, two concurrency tests, injected-clock flip, required headers (apikey, Content-Type, Accept, Authorization absent, correct path/query), rate-limit, and
  sign-in body contents.

  7. Deviations from the approved design

  1. Import cycle (structural, you approved Option 2). §256 claimed readest → state was "anticipated, not a violation," but missed that state already imports readest for Token. Resolved per
  your direction: new internal/token package owns the shared Token type; state and readest both depend on it. This is the only structural change.
  2. Refresh→SignIn fallback (behavioral, you approved). Implemented exactly as you specified: documented as an intentional deviation in the helper's doc comment (citing §8/§14.2), isolated
  in the tiny reauth helper, and logged loudly (Warn on trigger, Info on success). The public Refresh() does not fall back — it returns ErrInvalidRefreshToken; the fallback lives only in
  AccessToken, which owns the re-auth policy.

  Neither required editing the design docs (no factual error in the reverse-engineering report). If you'd like, I can append a short note to phase-3-auth-design.md recording the
  internal/token decision so the docs match the code.

  8. Remaining open questions

  - Whether 429 actually occurs on this Supabase project (handled defensively as ErrRateLimited; not in the documented status list) — a live-validation item, unchanged from the design.
  - Whether expires_at is ever omitted in practice (handled by deriving from expires_in) — also a pre-existing live-validation item.

  Verification, all green: gofmt clean, go vet ./... OK, go build ./... OK, go test ./... pass, go test -race ./... pass (43 passing tests in internal/readest), make build produces
  bin/bridge.

  Stopping here as instructed — awaiting your approval before starting Phase 4.
