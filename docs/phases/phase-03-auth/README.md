# Phase 3 — Readest Supabase Auth

**Status:** Complete and verified.
**Design:** [design.md](./design.md)
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`internal/readest/auth.go`](../../../internal/readest/auth.go), [`internal/token/token.go`](../../../internal/token/token.go), [`internal/readest/auth_test.go`](../../../internal/readest/auth_test.go)
**Depends on:** [Phase 0 skeleton](../#phase-folders) (project layout), Phase 1 config + Phase 2 state (no design/ADR docs were written for those — see [`../implementation-roadmap.md`](../implementation-roadmap.md) Phase 1–2 descriptions).
**Unblocked by this phase:** [Phase 4 — Readest Sync Client](../phase-04-readest-client/README.md)

## What this phase shipped

A pure Go port of `readest_syncauth.lua`'s Supabase token lifecycle: `SignIn` (password grant), `Refresh` (refresh-token grant), and `AccessToken` (which applies the 50%-TTL proactive refresh rule and the 60-second pre-request guard, serialized under a `sync.Mutex` to prevent the duplicate-refresh race `withFreshToken` was written to close in the Lua plugin). The bridge automatically falls back to `SignIn` if `Refresh` returns 400/401/403 (a headless-only behavior the Lua plugin handles via its UI login prompt; flagged in §8/§14.2 as a deliberate deviation).

## What shipped DIFFERENTLY from the design

- **Package split — [`internal/token`](../../../internal/token/token.go) created** (design said "stay flat in `internal/readest`"). Solves an import cycle the design missed: `state` already imported `readest.Token`, so adding `readest → state` would have been circular. Resolved per the operator's "Option 2" direction. See [decision-record.md §7](./decision-record.md).
- **Refresh → SignIn fallback** (design §8/§14.2, approved). Lives only in `AccessToken`'s `reauth` helper; `Refresh` itself returns `ErrInvalidRefreshToken` and does not fall back. Documented in the helper's doc comment.

## Live-validation items this phase opened

- Whether Supabase ever returns 429 from this project (handled defensively as `ErrRateLimited`).
- Whether `expires_at` is ever omitted (handled by deriving `now + expires_in`).

Both tracked in [`../../live-validation-status.md`](../../live-validation-status.md).

## Tests

[`internal/readest/auth_test.go`](../../../internal/readest/auth_test.go) covers the full §13 matrix (22 cases incl. two `-race` concurrency tests), driven by a scriptable stub `Doer` and an injectable clock. Plus `internal/token/token_test.go` added in Phase 8 for the `Token` freshness rules.
