# Design Review: `internal/readest/auth` (Phase 3)

## 1. Authentication flow, startup through steady-state

**Startup (no persisted token / first run):**
1. `Engine.RunOnce` (or CLI init) calls `Authenticator.AccessToken(ctx)`.
2. `Auth` sees no valid token in `state.Store` → calls `SignIn(ctx)`.
3. `SignIn` does `POST {supabase_url}/auth/v1/token?grant_type=password` with `apikey: <anon_key>` header and body `{email, password}`.
4. On success (200), response is parsed into `Token{AccessToken, RefreshToken, ExpiresAt, ExpiresIn}` and persisted via `state.Store.SetToken` + `Save()`.
5. `AccessToken` returns the fresh `access_token` string.

**Steady state (token present):**
1. `AccessToken(ctx)` loads the current `Token` from the store (or an in-memory cache backed by the store).
2. Apply `ShouldRefresh(now)` (50% TTL rule). If true → `Refresh(ctx)`.
3. Additionally/independently, apply the 60-second guard (`ExpiresWithin(now, 60)`) as a **hard pre-request check** — even if the proactive 50% check was somehow skipped, no request goes out with a token expiring in under 60s.
4. `Refresh` does `POST {supabase_url}/auth/v1/token?grant_type=refresh_token` with `apikey` header and body `{refresh_token}`.
5. On success, new `access_token` **and rotated `refresh_token`** are persisted atomically (both fields must be overwritten together — the old refresh token is invalidated server-side by Supabase's rotation).
6. Return the (possibly refreshed) access token to the caller for use as `Authorization: Bearer <token>` on the Readest sync client.

This exactly mirrors `readest_syncauth.lua:withFreshToken`, which blocks the caller until refresh completes rather than racing a request against an in-flight refresh (fixing the `ensureClient` race the plugin's own comments call out).

## 2. HTTP endpoints involved

| Purpose | Method | Path | Source |
|---|---|---|---|
| Password sign-in | POST | `{supabase_url}/auth/v1/token?grant_type=password` | `supabase-auth-api.json` |
| Refresh token | POST | `{supabase_url}/auth/v1/token?grant_type=refresh_token` | `supabase-auth-api.json` |
| Sign out | POST | `{supabase_url}/auth/v1/logout` | not needed for bridge (no logout flow) |
| Get user | GET | `{supabase_url}/auth/v1/user` | not needed for bridge |

Only the first two are in scope for Phase 3/4. Default `supabase_url` = `https://readest.supabase.co` (already in `config.Default()`).

## 3. Required headers

Every request to the Supabase auth API needs:
- `apikey: <anon_key>` (always — this is the Supabase project's public anon key, decoded from base64 in config)
- `Content-Type: application/json`
- `Accept: application/json` (plugin sets this; harmless to include)

Refresh and sign-in do **not** need `Authorization: Bearer`. That header only appears on `sign_out` and `get_user`, which are out of scope.

## 4. Exact request/response JSON shapes

**Sign-in request** (`grant_type=password`):
```json
{ "email": "user@example.com", "password": "secret" }
```

**Refresh request** (`grant_type=refresh_token`):
```json
{ "refresh_token": "<opaque string>" }
```

**Response shape (both endpoints, same structure)** — inferred from `main.lua`/`readest_syncauth.lua` field usage:
```json
{
  "access_token": "...",
  "refresh_token": "...",
  "expires_at": 1234567890,
  "expires_in": 3600,
  "user": {
    "id": "...",
    "user_metadata": { "user_name": "..." }
  }
}
```
Only `access_token`, `refresh_token`, `expires_at`, `expires_in` are consumed by the bridge (`user` is irrelevant — the plugin uses it only for the display name in `main.lua`'s login flow, which the bridge has no UI for).

**Expected status codes** (`supabase-auth-api.json`): sign-in → `[200, 400, 401]`; refresh → `[200, 400, 401, 403]`.

## 5. Token fields and storage

`internal/readest/auth.go` already defines:
```go
type Token struct {
    AccessToken  string `json:"access_token"`
    RefreshToken string `json:"refresh_token"`
    ExpiresAt    int64  `json:"expires_at"`
    ExpiresIn    int64  `json:"expires_in"`
}
```
This matches the wire shape field-for-field and is already the type persisted by `state.Store.SetToken`/`Token()` (see `internal/sync/state/state.go`, `file.go`, `mem.go`). No new storage type is needed — Phase 3 fills in `Auth.SignIn`/`Refresh`/`AccessToken` to read/write through the already-injected `state.Store`.

Note: `Auth` as currently constructed (`NewAuth(supabaseURL, anonKey, email, password)`) does **not** receive a `state.Store`. This is a gap I need to flag (see §9).

## 6. Refresh-token rotation behavior

Supabase issues a **new refresh token on every refresh call** — the old one is invalidated. The plugin (`readest_syncauth.lua:tryRefreshToken`/`withFreshToken`) always overwrites all four fields (`access_token`, `refresh_token`, `expires_at`, `expires_in`) together via one `G_reader_settings:saveSetting`. The bridge must do the same: a single `state.Store.SetToken(newToken)` + `Save()`, never partial field updates. If the process crashes between receiving a refreshed token and persisting it, the next run will retry with the *old* (now-invalid) refresh token and get a 400/401 from Supabase — this is an accepted failure mode requiring re-`SignIn` (see §8).

## 7. 50% TTL rule and 60-second guard — precise semantics

Already implemented correctly in `internal/readest/auth.go`:

```go
func (t Token) ShouldRefresh(now int64) bool {
    if t.ExpiresIn <= 0 {
        return true
    }
    return t.ExpiresAt < now+t.ExpiresIn/2
}

func (t Token) ExpiresWithin(now, seconds int64) bool {
    return t.ExpiresAt < now+seconds
}
```

This matches `readest_syncauth.lua` exactly:
```lua
-- tryRefreshToken / withFreshToken
settings.expires_at < os.time() + settings.expires_in / 2
-- needsLogin
not settings.access_token or not settings.expires_at or settings.expires_at < os.time() + 60
```

**Precise rule for `AccessToken(ctx)`:**
1. If no token persisted at all → `SignIn`.
2. Else if `ShouldRefresh(now)` → `Refresh`.
3. Else if `ExpiresWithin(now, 60)` → `Refresh` anyway (belt-and-suspenders; in practice `ShouldRefresh` will almost always fire first for any `expires_in` ≥ 120s, but the 60s guard is the plugin's *pre-request* absolute-safety check independent of the proactive TTL math, and should be applied even if `ExpiresIn` is 0/unknown).
4. Else return the existing access token unchanged.

Both checks use the **same current-time source** (`time.Now().Unix()`), injected for testability.

## 8. Behavior on each condition

| Condition | Behavior |
|---|---|
| **Startup, no tokens** | `state.Store.Token()` returns zero-value `Token{}`. `AccessToken` detects `AccessToken == ""` (or `ExpiresAt == 0`) → calls `SignIn`. If `Email`/`Password` are empty, this is a config-validation error already caught earlier by `Config.Validate()`, so `Auth` can assume both are non-empty. |
| **Access token expired but refresh token valid** | `ShouldRefresh`/`ExpiresWithin` trip → `Refresh` succeeds → new token persisted, returned. |
| **Refresh token itself expired/invalid** | `Refresh` gets `400` or `401` from Supabase. Per plugin behavior there is no automatic fallback to `SignIn` inside `tryRefreshToken` — but the bridge, being headless with credentials on hand, **should** fall back: on a refresh failure specifically due to `400`/`401` (invalid_grant), attempt one `SignIn` with the stored email/password before giving up. This is new behavior beyond the plugin (the plugin's UI just shows a login prompt to a human); the bridge has no human, so re-authenticating from stored credentials is the correct headless analogue. **This should be called out explicitly as a deliberate deviation, not an assumption I'm hiding.** |
| **401 from the sync API (not auth API) when calling Readest `/sync`** | Not `Auth`'s job — that's `readest.Client`'s job to detect and call `Auth.Refresh`/invalidate token, mirroring `readest_syncconfig.lua`'s 401/403 handling. `Auth` only owns the Supabase token lifecycle, not reaction to downstream 401s from the sync API. Worth noting for the Phase 4 boundary. |
| **403 from Supabase auth endpoints** | Only listed for refresh (`expected_status: [200, 400, 401, 403]`). Treat identically to 401 — invalid/expired refresh token → fall back to `SignIn`. |
| **Network failure (transport error, timeout)** | Return a wrapped error (`fmt.Errorf("readest: auth: ... %w", err)`) distinguishable via `errors.Is`/a sentinel like `ErrNetwork` so the engine's retry/backoff layer (Phase 6) can distinguish "retry later" from "bad credentials, stop." Do **not** fall back to `SignIn` on network errors — that would mask a transient outage as a credential problem and burn a sign-in attempt needlessly. |
| **Malformed response (200 but bad JSON, or missing required fields)** | Return a distinct error (`ErrMalformedResponse` or similar) rather than silently persisting a zero-value token. Never persist a `Token` with an empty `AccessToken`. |

## 9. Methods on `Authenticator`

The interface already defined in `internal/readest/auth.go` is correct and sufficient:
```go
type Authenticator interface {
    SignIn(ctx context.Context) (Token, error)
    Refresh(ctx context.Context) (Token, error)
    AccessToken(ctx context.Context) (string, error)
}
```
`SignIn` and `Refresh` are exposed on the interface (not just internal helpers) because:
- Tests need to exercise each path independently.
- `AccessToken` is the only method the sync client (`readest.Client`) actually calls — `SignIn`/`Refresh` are effectively internal to `Auth` but kept on the interface for testability and because the roadmap phase document explicitly calls out `SignIn`/`Refresh` as named methods (`docs/implementation-roadmap.md` Phase 3).

**Gap to flag:** `Auth` (the concrete stub in `auth.go`) currently has no `state.Store` field, and its constructor is `NewAuth(supabaseURL, anonKey, email, password string)`. For Phase 3, `Auth` must also hold a `state.Store` (to load/persist tokens) and an `httpclient.Doer` + `*slog.Logger` (matching the pattern already used by `readest.Client` and `bookorbit.Client`). This means `NewAuth`'s signature needs to grow. I'm flagging this now rather than silently changing it — the constructor shape needs your sign-off since it's a breaking change to an already-committed public API.

## 10. Internal state transitions

```
        ┌─────────────┐
        │  NoToken     │ (zero-value Token in store)
        └──────┬──────┘
               │ SignIn() success
               ▼
        ┌─────────────┐   ShouldRefresh(now) == false
        │   Fresh      │◄────────────────────────────┐
        └──────┬──────┘                              │
               │ ShouldRefresh(now) == true           │
               ▼                                      │
        ┌─────────────┐   Refresh() success           │
        │  Refreshing  │───────────────────────────────┘
        └──────┬──────┘
               │ Refresh() fails with 400/401/403
               ▼
        ┌─────────────┐
        │ ReAuthing    │──► SignIn() ──► Fresh (or hard failure if SignIn also fails)
        └─────────────┘
```
There's no need for a richer state machine than this — `Token` itself plus `time.Now()` fully determines which branch to take; no separate "state" enum is needed as a type, the branching is computed fresh on every `AccessToken` call.

## 11. Concurrency expectations — is a refresh mutex needed?

**Yes.** The bridge is a single poll-loop process today (per `docs/implementation-brief.md`: "Polling daemon"), so in the *current* architecture there is exactly one caller of `AccessToken` per sync pass and no concurrent goroutines calling it simultaneously — `Engine.RunOnce` runs serially. However:

- I'd still add a `sync.Mutex` inside `Auth` around the read-check-refresh-write sequence, for two reasons: (1) it costs nothing and guards against future concurrency (e.g., if the CLI ever adds a health-check HTTP endpoint that also needs a fresh token, or if `--once` and a signal-triggered manual sync ever overlap), and (2) it prevents a subtle bug where `AccessToken` is called twice in quick succession (e.g., once for `PullBooks`, once for a hypothetical future call in the same pass) and both see a stale "needs refresh" token, firing two redundant refresh calls — the second of which would be rejected by Supabase since refresh tokens are single-use (rotation invalidates the old one immediately). A mutex serializes this into "refresh once, second caller reads the now-fresh result."

This mirrors the *intent* of `withFreshToken` in the plugin, which exists specifically to close a race (codex finding: `ensureClient` fired a request with a stale token while a refresh was in flight).

## 12. Error taxonomy — recommended Go error values

```go
var (
    ErrNotImplemented      = errors.New("readest: not implemented in foundation phase") // existing
    ErrInvalidCredentials  = errors.New("readest: auth: invalid email or password")      // 400/401 on SignIn
    ErrInvalidRefreshToken = errors.New("readest: auth: refresh token invalid or expired") // 400/401/403 on Refresh
    ErrNetwork             = errors.New("readest: auth: network error")                    // transport-level failure
    ErrMalformedResponse   = errors.New("readest: auth: malformed response")               // 200 but bad body
    ErrRateLimited         = errors.New("readest: auth: rate limited")                     // 429 (not in documented status codes but Supabase can return it)
)
```
Each should wrap the underlying HTTP status/body via `%w` so `errors.Is` works while `%v`/`Error()` still carries diagnostic detail (status code, truncated body). This follows the pattern already used for `ErrNotImplemented` in `auth.go`, `client.go` (bookorbit), and `engine.go`.

## 13. Unit-test matrix

| # | Scenario | Assert |
|---|---|---|
| 1 | `SignIn` success (200) | Token fields parsed correctly, persisted via store |
| 2 | `SignIn` 400 (bad credentials) | Returns `ErrInvalidCredentials`, nothing persisted |
| 3 | `SignIn` 401 | Same as #2 |
| 4 | `SignIn` network error | Returns `ErrNetwork`, nothing persisted |
| 5 | `SignIn` malformed JSON body | Returns `ErrMalformedResponse` |
| 6 | `SignIn` response missing `access_token` | Returns `ErrMalformedResponse`, not persisted |
| 7 | `Refresh` success (200) | New token (incl. rotated refresh_token) persisted atomically |
| 8 | `Refresh` 400 | Returns `ErrInvalidRefreshToken`; triggers fallback `SignIn` per §8 |
| 9 | `Refresh` 401 | Same as #8 |
| 10 | `Refresh` 403 | Same as #8 |
| 11 | `Refresh` network error | Returns `ErrNetwork`; **no** fallback SignIn |
| 12 | `AccessToken` with no persisted token | Calls `SignIn` once |
| 13 | `AccessToken` with fresh token (`ShouldRefresh`=false, `ExpiresWithin(60)`=false) | Returns existing token, no HTTP call |
| 14 | `AccessToken` with `ShouldRefresh`=true | Calls `Refresh`, returns new token |
| 15 | `AccessToken` with `ExpiresIn`=0 (never set) | `ShouldRefresh` returns true unconditionally → refresh path |
| 16 | `AccessToken` with token expiring in 30s but `ShouldRefresh`=false (e.g. huge `ExpiresIn` edge case) | 60s guard still fires refresh |
| 17 | Concurrent `AccessToken` calls (goroutine test with mutex) | Only one HTTP refresh call fires; second caller gets the refreshed token |
| 18 | `ShouldRefresh` table test | Exact boundary at `expires_at == now + expires_in/2` |
| 19 | `ExpiresWithin` table test | Exact boundary at `expires_at == now + seconds` |
| 20 | Apikey header present on every request | Assert via test `Doer` capturing `req.Header` |
| 21 | Authorization header absent on SignIn/Refresh | Assert not set (these two endpoints don't need it) |
| 22 | Refresh failure then fallback SignIn also fails | Returns the SignIn error (or a wrapped combination), nothing persisted, original invalid token remains untouched in store (don't corrupt existing state on total failure) |

## 14. Discrepancies / ambiguities in documentation

1. **`reverse-engineering-report.md` item #9** flags that the bridge must handle a missing `expires_at` defensively — the plugin trusts it unconditionally. I'd treat a response with `expires_at == 0` as malformed (or derive it as `time.Now().Unix() + expires_in` if `expires_in` is present and `expires_at` is absent) rather than silently accepting a zero value that would make `ShouldRefresh` always true forever (harmless but wasteful — refreshes every single call).
2. **Refresh-then-fallback-to-SignIn is new behavior I'm proposing**, not something present in the Lua plugin (which just tells a human to log in again via UI). This needs your explicit sign-off since it's a deviation from "mirror the plugin exactly."
3. **`Auth`'s constructor lacking `state.Store`** (§9) is a real gap between the roadmap's stub and what Phase 3 needs — not a doc discrepancy, but a code-contract gap I want confirmed before touching the constructor signature.
4. Planning doc mentions Supabase "get_user"/"sign_out" as available but the bridge has no use for either — I'm treating both as permanently out of scope, confirming that's still your intent.

## 15. Unknowns requiring live-server validation

1. Whether Supabase ever omits `expires_at` in practice (spec says it's always present for GoTrue `/token` responses, but worth a live check).
2. Whether a `429` (rate limit) is ever returned by this Supabase project — not in the documented `expected_status` list but plausible under repeated polling; should be handled defensively regardless.
3. Whether `Refresh` with an already-invalidated (rotated-away) refresh token returns `400` or `401` specifically — affects whether the fallback-to-SignIn logic needs to key off status code or just "any Refresh error."

---

## Proposed package layout / public API for `internal/readest/auth`

I'd keep this **inside the existing `internal/readest` package** rather than splitting into a new `internal/readest/auth` subpackage — the existing files (`auth.go`, `client.go`, `models.go`, `doc.go`) already live flat in `internal/readest`, and `Token`/`Authenticator`/`Auth` are already declared there. Introducing a subpackage now would require import-path churn across `client.go`, `engine.go`, and the tests, for no architectural benefit (Token/Authenticator are only consumed by `readest.Client` and `sync.Engine`, both of which already import `readest`). I'd only split it out if `internal/readest` grew unwieldy later.

**Files to modify (not create new):**
- `internal/readest/auth.go` — implement `SignIn`, `Refresh`, `AccessToken`; add `store state.Store`, `http httpclient.Doer`, `log *slog.Logger`, `mu sync.Mutex`, `now func() int64` (injectable clock) fields to `Auth`; update `NewAuth` signature.

**Public API (revised):**
```go
func NewAuth(supabaseURL, anonKey, email, password string, store state.Store, hc httpclient.Doer, log *slog.Logger) *Auth

func (a *Auth) SignIn(ctx context.Context) (Token, error)
func (a *Auth) Refresh(ctx context.Context) (Token, error)
func (a *Auth) AccessToken(ctx context.Context) (string, error)
```
`Token`, `Authenticator` interface, `ShouldRefresh`, `ExpiresWithin` remain unchanged (already correct). New sentinel errors added to `auth.go` per §12.

Note: `internal/readest` currently has no import of `internal/sync/state` — this would be a new dependency edge (`readest` → `state`). Checking the roadmap's dependency graph, `STATE --> RAUTH` is already drawn as an arrow in `docs/implementation-roadmap.md`'s mermaid diagram, so this is anticipated, not a violation.

---

**Waiting for your approval before writing any implementation.** Specifically I need a decision on:
1. The `NewAuth` constructor signature change (adding `state.Store`, `Doer`, `*slog.Logger`).
2. Whether the refresh-failure → fallback-SignIn behavior (§8, §14.2) is acceptable, or whether you'd rather `Refresh` failure just surface as a hard error for the engine to handle at a higher level.
3. Whether an injectable clock (`now func() int64`) is wanted for deterministic tests, or whether `time.Now().Unix()` inline is fine (I lean toward injectable — it's already the pattern used in `state.NewMemStore` tests via explicit timestamps and in `SyncCoordinator.new{ now = ... }` in the Lua reference).

