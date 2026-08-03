# Design: `internal/readest` Sync Client (Phase 4)

**Status:** proposal, awaiting approval. No code is written in this phase.
**Scope:** the Readest sync client only — `PullBooks` against `GET /sync?type=books`, its row/timestamp models, and its integration with the Phase 3 `Auth`. Everything BookOrbit-side, the sync engine, and the CLI are explicitly out of scope except where a boundary must be named.

Authoritative behavior is taken from the reference plugin (`reference/readest.koplugin/`), read directly for this document: `readest-sync-api.json` (endpoint + status codes), `readest_syncclient.lua` (transport + middleware), `library/syncbooks.lua` (`pullBooks` semantics, dual watermark), `library/librarystore.lua` (`parseSyncRow`, `iso_to_ms`, dummy filter), and `readest_syncconfig.lua` (downstream 401/403 detection). Where those disagree with the planning docs, the plugin wins and the disagreement is flagged in §11.

---

## 1. Overall responsibility — ownership boundaries

Phase 4 draws three clean lines. The rule throughout: **each layer owns exactly one external system or concern, and nothing crosses a line except through the interface that line exists for.**

### 1.1 The Readest client (`internal/readest`, this phase)

Owns exactly one thing: **turning a millisecond cursor into a decoded page of book rows over HTTP.**

- Build the `GET {base}/sync?type=books&since=<ms>` request.
- Attach a valid Bearer credential, obtained from `Auth`.
- Classify the response into success / retryable / non-retryable (§2, §6).
- Decode the envelope into `[]BookRow` (models already exist; the client feeds bytes to them).
- On a downstream 401/403, drive **one** token refresh and **one** retry (§5).
- Return data or a wrapped sentinel error to the caller.

The client does **not**: decide what `since` should be (that is the engine/state's watermark), filter dummy/deleted rows for sync decisions (the *model* exposes `IsDummy`/`IsDeleted`; the *engine* applies them in Phase 6), advance any watermark, batch, dedupe percentages, or know that BookOrbit exists. It returns the raw rows for the requested window, faithfully.

### 1.2 Auth (`internal/readest/auth.go`, Phase 3 — already built)

Owns the **Supabase token lifecycle** and nothing about the sync API:

- `AccessToken(ctx)` returns a valid access token, signing in on first run and refreshing at the 50%-TTL / 60-second rules. It serializes refresh with a mutex and persists rotations atomically.
- `Refresh(ctx)` is exposed so the client can force a refresh after a downstream 401 (§5.4).
- It knows the Supabase endpoints, the `apikey` header, and its own sentinel errors (`ErrInvalidCredentials`, `ErrInvalidRefreshToken`, `ErrNetwork`, `ErrMalformedResponse`, `ErrRateLimited`).

Auth does **not** know the sync API exists, never sees a 401 from `/sync`, and never builds a sync request. The client is the only consumer of `Auth` within this phase.

### 1.3 The Sync Engine (`internal/sync`, Phase 6 — not built)

Owns **orchestration and policy**, consuming the two clients and the store:

- Reads the persisted watermark, calls `client.PullBooks(ctx, watermark)`.
- Applies `IsDummy`/`IsDeleted`, computes `Percentage()`, dedupes against `state.MatchRecord.LastPushedPct`.
- Runs match-check and bulk-progress against BookOrbit (Phase 5), advances watermarks, applies retry/backoff policy across batches, decides fail-fast vs. skip.

The engine does **not** build HTTP requests or parse JSON; those stay in the clients. Phase 4 must therefore expose everything the engine needs to make those decisions (`BookRow` predicates, `WatermarkMs`, sentinel errors it can `errors.Is` on) without exposing transport internals.

### 1.4 Boundary summary

| Concern | Owner | Crosses via |
|---|---|---|
| Token lifecycle (Supabase) | `readest.Auth` (Phase 3) | `Authenticator` interface |
| `/sync` transport + decode + one 401 retry | `readest.Client` (**this phase**) | `SyncClient` interface |
| Wire row shape + row semantics | `readest` models (already present) | `BookRow` value type |
| Watermark value & persistence | `state.Store` | `Watermark()`/`SetWatermark()` |
| Sync orchestration/policy | `sync.Engine` (Phase 6) | the two client interfaces |

The one boundary that does **not** exist in code yet is the seam between the client's "raw rows" and the engine's "actionable rows." Phase 4 keeps the client faithful (returns everything in the window) and leaves selection to the engine — this is deliberate, and matches the plugin, where `client:pullBooks` returns `body.books` untouched and `syncbooks.lua`'s loop does the filtering/watermark work.

---

## 2. HTTP API surface

Only **one** endpoint is in scope for Phase 4. Everything else in `readest-sync-api.json` is intentionally out of scope (§2.3).

Base URL comes from `Config.Readest.SyncBaseURL` (default `https://web.readest.com/api`, `internal/config/defaults.go`). The plugin hardcodes it; the bridge exposes it for future-proofing only — there is no evidence the hosted service is reachable anywhere else (see §11, discrepancy 6).

### 2.1 `GET /sync?type=books` — bulk library pull (IN SCOPE)

| Aspect | Value | Source |
|---|---|---|
| URL | `{SyncBaseURL}/sync` with query `type=books&since=<ms>` | `readest-sync-api.json` `pullBooks` |
| Method | `GET` | spec |
| Query params | `type=books` (constant), `since` = integer **epoch milliseconds**, `0` = full library | spec `required_params: ["since"]`; `syncbooks.lua:404` (`since = store:getLastPulledAt() or 0`) |
| Required headers | `Authorization: Bearer <access_token>`, `Content-Type: application/json`, `Accept: application/json` | `readest_syncclient.lua:28-37` (ReadestHeaders + ReadestAuth middleware) |
| Auth | Supabase JWT from `Auth.AccessToken(ctx)`; the request is refused before dispatch if no token is available | `ReadestAuth` middleware returns false when token unset |
| Request body | none (GET) | — |
| Success status | `200` | spec `expected_status`; `syncbooks.lua` treats `status == 200` as success |
| Success body | `{ "books": [ <row>, ... ] }`; `books` may be empty | `readest_syncclient.lua:117`; `BooksResponse` model |

**Row shape on the wire** (`parseSyncRow`, `librarystore.lua:791-858`): snake_case DB columns. The bridge models only what it needs (already in `models.go`): `book_hash` (or `hash`), `meta_hash`, `title`, `author`, `format`, `progress` (JSON tuple `[cur,total]`, may be a stringified tuple or `null`), `updated_at`, `deleted_at`, `synced_at`, `created_at` — all timestamps ISO-8601 strings. The client does not synthesize fields the model omits (`group_id`, `metadata`, `reading_status`, `uploaded_at`, `source_title`); those are Phase 6+ concerns if ever needed.

### 2.2 Status codes and retry classification

From `expected_status: [200, 400, 301, 401, 403]` on `pullBooks`, plus the realities of an HTTP service:

| Status | Meaning for the bridge | Retryable? |
|---|---|---|
| `200` | Success; decode body | — |
| `301` | Redirect. `net/http` follows same-scheme redirects automatically; a cross-scheme/host downgrade that strips the `Authorization` header surfaces as a subsequent 401. Treat a redirect that lands anywhere unexpected as non-retryable and flag for live validation (§12) — the hosted service should not be redirecting `/api/sync`. | No (surfaced) |
| `400` | Malformed request (bad `since`). A bug on our side, not transient. | **No** — `ErrBadRequest` |
| `401` / `403` | Token rejected/expired (`readest_syncconfig.lua:242` also matches body `{error:"Not authenticated"}`). Drives **one** refresh + **one** retry (§5). After the retry, still-401 is non-retryable for this call and surfaces as `ErrUnauthorized`. | Once, internally |
| `429` | Not in `expected_status` but plausible. | **Yes** — `ErrRateLimited` (engine backs off) |
| `5xx` | Server fault. | **Yes** — `ErrServer` |
| transport error / timeout / `ctx` cancel | Network-level | **Yes** — `ErrNetwork` |

"Retryable" above means *the engine may choose to retry later*; the client itself performs no backoff loop (that policy lives in Phase 6). The client only ever repeats a request **once**, and only for the auth-refresh case (§5.5).

### 2.3 Endpoints intentionally OUT of scope

All other methods in `readest-sync-api.json`:

- `POST /sync` (`pushChanges`) — one-way bridge never pushes to Readest.
- `GET /sync?type=configs` (`pullChanges`) — exact xpointer/page resume; the planning docs and §8 of the reverse-engineering report confirm the bulk `books.progress` tuple is sufficient for percentage-level sync. Explicitly deferred (see problem-statement answer 2: "not currently").
- `GET /storage/download`, `GET /storage/list`, `DELETE /storage/delete`, `POST /storage/upload` — book-file/cover transfer; the bridge never sees a file (the partial-MD5 parity is the whole design).

None of these get Go types or methods in Phase 4.

---

## 3. Client API (proposed Go surface)

The package already declares the contract; Phase 4 fills in the concrete type. **No new exported types are required** beyond what exists — this is intentional and is itself an architectural decision (§13).

### 3.1 Interface (unchanged, already in `client.go`)

```go
type SyncClient interface {
    PullBooks(ctx context.Context, since int64) ([]BookRow, error)
}
```

Kept exactly as-is. The engine depends only on this; the real client and Phase 6 test fakes both satisfy it. `since` is epoch milliseconds; `0` requests the full library.

### 3.2 Constructor (unchanged signature)

```go
func NewClient(baseURL string, auth Authenticator, hc httpclient.Doer, log *slog.Logger, timeout time.Duration) *Client
```

Already the shape `cmd/bridge/main.go:85` calls. Phase 4 keeps it; see §9 for the dependency justification, including why `timeout` is retained despite being redundant with the `Doer`.

### 3.3 Exported method

```go
func (c *Client) PullBooks(ctx context.Context, since int64) ([]BookRow, error)
```

The only exported behavior. Why it exists: it is the single read the bridge needs from Readest. Returns the decoded rows for the window; the caller owns watermark math and filtering. `ErrNotImplemented` is removed from this path.

### 3.4 Internal (unexported) helpers

These are proposed to keep `PullBooks` readable and each concern testable. None is exported.

- `buildRequest(ctx, since int64, token string) (*http.Request, error)` — constructs the GET, sets `type=books`/`since` query and the three headers. Exists so request shape (URL, query encoding, headers) is unit-testable without a round trip.
- `do(ctx, req) (*http.Response, error)` — thin wrapper over `c.http.Do` that maps a transport error to `ErrNetwork` and closes/limits bodies. Exists to centralize the transport-error taxonomy in one place.
- `decodeBooks(resp) ([]BookRow, error)` — reads the bounded body, unmarshals `BooksResponse`, returns the slice. Exists so malformed-body handling (§6.4) is isolated and testable.
- `isAuthFailure(status int, body []byte) bool` — mirrors `readest_syncconfig.lua:242`: true on `401`/`403` **or** a decoded `{error:"Not authenticated"}`. Exists because the body-sniff is a real rule the plugin relies on, not just the status code.
- `classifyStatus(resp) error` — status→sentinel mapping (§2.2), paralleling `auth.go`'s helper. Exists to keep the error taxonomy in one switch.

`maxBodyBytes` for the pull response is a package constant (proposed 4 MiB — a full library is large but bounded; generous headroom over any realistic row count, and it guards memory against a hostile/buggy server). This is **not** the BookOrbit `MaxBodyBytes` request cap; it is a response-read cap and lives in this package.

---

## 4. Data models

### 4.1 Request models

**None.** `PullBooks` is a `GET` with only query parameters; there is no request body to model. This is a deliberate contrast with Phase 5 (BookOrbit), which has rich request bodies.

### 4.2 Response models (already exist — reuse, do not duplicate)

All in `internal/readest/models.go`, already implemented and unit-tested in `models_test.go`:

| Type | Role | Reuse? |
|---|---|---|
| `BooksResponse` | Envelope `{books: [...]}` | **Reuse as-is** — the client unmarshals into it |
| `BookRow` | One `/sync?type=books` row | **Reuse as-is** — fields already match `parseSyncRow` |
| `DummyHash` const | Sentinel filter value | **Reuse** |

### 4.3 Row behavior methods (already exist — the engine will consume them)

`BookRow.IsDummy()`, `IsDeleted()`, `ProgressTuple()`, `Percentage()`, `WatermarkMs()`, `UpdatedMs()` — all present and tested. Phase 4 does not modify them. They are listed here to record that the client's return value is already "engine-ready" without further modeling work.

### 4.4 Missing structs

**None for Phase 4.** The existing model set is complete for a percentage-level pull. Fields the wire carries but the bridge does not yet need (`group_id`, `metadata`, `reading_status`, `uploaded_at`, `source_title`) are intentionally omitted; adding them is a future, additive change if the engine ever wants them, and does not belong in this phase.

### 4.5 Model ownership

- `BookRow` / `BooksResponse` / `DummyHash` — owned by `internal/readest`. Only the readest package and (read-only, as values) the engine touch them.
- The client owns *producing* them; the engine owns *interpreting* them. BookOrbit never sees a `BookRow` — the engine translates to `bookorbit.ProgressItem` (Phase 5) so the two packages stay independent (reverse-engineering report recommendation 10).

---

## 5. Authentication interaction

This is the heart of the phase and mirrors `syncbooks.lua:pullBooks` + `readest_syncconfig.lua` precisely.

### 5.1 When is `AccessToken()` called?

**Once per `PullBooks` call, before the request is built** — exactly as `pullBooks` wraps the dispatch in `SyncAuth:withFreshToken(...)`. `AccessToken` already encapsulates the 50%-TTL and 60-second rules and the sign-in-on-first-run path, so by the time it returns, the token is valid for at least the next 60 seconds. The client does not re-implement any freshness logic.

### 5.2 How is the Bearer header attached?

`req.Header.Set("Authorization", "Bearer "+token)` in `buildRequest`, mirroring `readest_syncclient.lua:36` (`req.headers["authorization"] = "Bearer " .. self.access_token`). `Content-Type` and `Accept` are set alongside it (`readest_syncclient.lua:28-29`). The token is **never** logged; log lines reference only `since`, row counts, and status codes.

### 5.3 How are expired tokens handled?

Proactively, inside `Auth.AccessToken` (Phase 3, already correct): refresh at half-TTL or within 60s of expiry. The client benefits from this without doing anything — the token it gets is fresh.

### 5.4 How are downstream 401/403 responses handled?

This is the client's job, **not** Auth's (Phase 3 design §8 explicitly assigns downstream-401 handling to the client). The flow:

1. First request returns 401/403 (or body `{error:"Not authenticated"}` — `isAuthFailure`, §3.4).
2. Client calls `c.auth.Refresh(ctx)` **once** to force a fresh token. (It calls `Refresh`, not `AccessToken`, because the stored token may still *look* fresh to the proactive rules while being server-revoked; the plugin's `getReadestSyncClient`/`withFreshToken` pair has the same "server said no, get a new one" intent. `Refresh` in turn already falls back to `SignIn` internally? — **No**: `Refresh` does *not* fall back; that fallback lives in `AccessToken`. So the client calls `c.auth.AccessToken(ctx)` **after invalidating freshness is not possible** — see the open decision below.)
3. Rebuild the request with the new token and retry **once**.
4. If the retry also returns 401/403, give up for this call and return `ErrUnauthorized`.

### 5.5 Does the client retry automatically? Where does responsibility end?

- **Auth-triggered retry: exactly one**, only after a 401/403, only after a successful re-auth. Bounded and total — never a loop.
- **Transient retry (network, 5xx, 429): the client performs none.** It returns the classified retryable error (`ErrNetwork`/`ErrServer`/`ErrRateLimited`) and stops. Backoff, jitter, and attempt counts are the **engine's** job (Phase 6), driven by `Config.Bridge.Retry*` settings the client deliberately does not read. This keeps "how many times to knock" (policy) separate from "how to knock" (transport).

**Responsibility ends** at: returning either a decoded `[]BookRow` or a wrapped, `errors.Is`-matchable sentinel. The client never advances the watermark, never decides a row is worth pushing, and never loops.

> **Open decision for approval (§13, item B):** the exact re-auth call after a downstream 401. Two viable shapes:
> 1. Client calls `c.auth.Refresh(ctx)`. Simple; but `Refresh` alone returns `ErrInvalidRefreshToken` without the `AccessToken` sign-in fallback, so a rotated-away refresh token would hard-fail instead of re-authing.
> 2. Client calls a small `Auth` addition, e.g. `ForceRefresh(ctx) (string, error)`, that does "refresh, else sign-in, return token" — i.e. `AccessToken`'s recovery semantics, forced. This reuses the already-approved re-auth policy and keeps the client dumb.
>
> **I recommend option 2** (a tiny additive method on `Auth`), because it keeps the headless re-authentication policy in exactly one place (Phase 3's `reauth`) rather than duplicating its decision logic in the client. It is a one-method addition to `auth.go`, not a behavior change.

---

## 6. Error handling

### 6.1 Sentinel taxonomy (proposed additions to `client.go`)

Following the established package pattern (`errors.New` + `%w` wrapping, matching `auth.go`):

```go
var (
    ErrUnauthorized      // 401/403 after the single re-auth+retry; terminal for this call
    ErrBadRequest        // 400; a client-side bug, non-retryable
    ErrRateLimited       // 429; retryable by the engine
    ErrServer            // 5xx; retryable by the engine
    ErrNetwork           // transport/timeout/ctx; retryable by the engine
    ErrMalformedResponse // 200 but undecodable or wrong-shaped body; non-retryable
)
```

`ErrNetwork` and `ErrMalformedResponse` already exist in `auth.go` for the *auth* package's own use. **Decision:** the sync client should have its **own** sentinels, not reuse auth's, because the engine must distinguish "auth network failed" from "sync network failed" in logs even when both retry the same way. Two clean options:

- **(a) Shared transport sentinels** in a tiny `internal/readest`-level var block reused by both files. Risk: auth and sync errors become indistinguishable.
- **(b) Per-concern sentinels** — `ErrNetwork` stays auth's; the client gets `ErrSyncNetwork`, etc. More verbose but unambiguous.

**I recommend (b)-style distinct sync sentinels** (`ErrUnauthorized`, `ErrBadRequest`, `ErrSyncServer`, `ErrSyncNetwork`, `ErrSyncMalformed`, plus reusing a shared `ErrRateLimited` concept) so `errors.Is` in the engine is unambiguous. Naming is finalized at implementation; the principle — *auth failures and sync failures are different sentinels* — is the decision being asked for (§13, item C).

### 6.2 Wrapping strategy

Every error wraps its sentinel via `%w` and appends status + a bounded body snippet, exactly as `auth.go:classifyStatus` does (`readErrorSnippet`, 512-byte cap). Logs get diagnostics; the engine gets `errors.Is`. Example shape: `fmt.Errorf("%w: pull books since %d: status %d: %s", ErrSyncServer, since, code, snippet)`.

### 6.3 Transport errors

`c.http.Do` returning an `error` (DNS, connect, TLS, timeout) → `ErrSyncNetwork`, wrapping the underlying `%v`. `ctx.Done()` mid-flight surfaces through `Do` as a wrapped `context.Canceled`/`DeadlineExceeded`; the client lets those propagate so the engine's graceful shutdown sees `context.Canceled` cleanly (Phase 6 requirement, `main.go:130`).

### 6.4 Malformed responses

A `200` whose body is not valid JSON, or not the `{"books":[...]}` shape (e.g. `books` is not an array), → `ErrSyncMalformed`. Individual **rows** are never rejected by the client: `BookRow` uses `json.RawMessage` for `progress` precisely so a weird tuple doesn't fail the whole page (that normalization lives in `decodeProgressTuple`, already tested). A single bad row's timestamps yield `0` via `WatermarkMs`'s tolerant parse (`models.go:102` documents "a single bad row cannot stall the watermark"). The client returns rows faithfully; per-row skip decisions are the engine's.

### 6.5 API errors

A non-2xx with a JSON `{error: "..."}` body has that message folded into the wrapped error's detail string (bounded), so a `400` saying `"since must be an integer"` is diagnosable. The `{error:"Not authenticated"}` body is special-cased into `isAuthFailure` (§5.4) regardless of status.

### 6.6 Retry semantics (summary)

| Failure | Client action | Engine action (Phase 6) |
|---|---|---|
| 401/403 | one re-auth + one retry, then `ErrUnauthorized` | fail the run; surface |
| 400 | `ErrBadRequest` | fail loud (bug) |
| 429 | `ErrRateLimited` | backoff, honor `Retry-After` if present |
| 5xx | `ErrSyncServer` | exponential backoff |
| network/timeout | `ErrSyncNetwork` | exponential backoff |
| 200 bad body | `ErrSyncMalformed` | fail the run (do not advance watermark) |

---

## 7. Pagination / synchronization behavior

Determined directly from `syncbooks.lua` and `librarystore.lua`:

- **Pagination: none.** There is no page/limit/offset concept in `pullBooks`. One request returns the whole delta (or whole library on `since=0`).
- **Cursors / change tokens: the `since` parameter IS the cursor.** It is an epoch-**millisecond** watermark, not an opaque token.
- **Incremental sync: supported and is the steady state.** `since=<watermark>` returns only rows changed since then.
- **Full sync: `since=0`.** Used on first run; emits the dummy hash row which must be filtered.
- **Timestamps: dual.** The pull cursor is `max(synced_at, updated_at, deleted_at)` with **`synced_at` winning when present** (`row_pull_cursor`, `librarystore.lua:203-210`; server-authoritative per issue #4678). This is already encoded in `BookRow.WatermarkMs()`.

**What Phase 4 implements:** exactly the single-cursor incremental pull. The client sends whatever `since` the engine provides and returns rows. **Watermark computation and advancement are NOT in the client** — `syncbooks.lua` keeps `pull_ts` in the store and computes it in the loop; the bridge's engine will do the same in Phase 6 using `BookRow.WatermarkMs()`. The client is the "fetch this window" primitive only. Orchestration (looping, batching to BookOrbit, advancing the persisted cursor, empty-page handling) is explicitly deferred to Phase 6 per the task instructions.

---

## 8. Concurrency expectations

**The client should be goroutine-safe, but shares no mutable state of its own.**

- `Client` holds only immutable-after-construction fields (`baseURL`, `auth`, `http`, `log`, `timeout`). It has **no mutex and needs none** — there is no per-request mutable field.
- Token freshness is the only shared mutable concern, and it is **already serialized inside `Auth.AccessToken` by its mutex** (Phase 3, race-tested). Concurrent `PullBooks` calls would each call `AccessToken`; the mutex collapses redundant refreshes into one. So even under concurrency the client is safe *by delegation*.
- In practice the bridge is single-threaded per sync pass (`implementation-brief.md`: polling daemon; `Engine.RunOnce` is serial). Goroutine-safety is therefore a free property of the design, not a requirement driving extra synchronization.

**Conclusion: add no synchronization to `Client`.** Justify: its fields are read-only after `NewClient`, and the one shared mutable resource (the token) is guarded downstream. Adding a mutex would guard nothing.

---

## 9. Constructor dependencies

`NewClient(baseURL, auth, hc, log, timeout)` — five dependencies:

| Dep | Justification | Verdict |
|---|---|---|
| `baseURL string` | Where `/sync` lives; from `Config.Readest.SyncBaseURL`. | **Keep.** |
| `auth Authenticator` | Source of Bearer tokens and the re-auth path (§5). The core dependency. | **Keep.** |
| `hc httpclient.Doer` | Injected transport → testable without a network (`stubDoer` pattern from `auth_test.go`). | **Keep.** |
| `log *slog.Logger` | Structured lifecycle logging; nil→`slog.Default()` (same convention as `NewAuth`). | **Keep.** |
| `timeout time.Duration` | **Redundant as constructed.** `main.go:83` already builds `httpDoer = httpclient.New(cfg.Bridge.HTTPTimeout)`, so the timeout is baked into the `Doer`; passing it again to `NewClient` is dead weight *unless* the client wraps calls in its own `context.WithTimeout`. | **See below.** |

**On `timeout`:** two honest options.
- **(i) Drop it** and rely on the `Doer`'s timeout. Cleanest; removes a parameter that currently does nothing.
- **(ii) Keep and honor it** as a per-`PullBooks` `context.WithTimeout(ctx, timeout)`, giving the *sync read* its own deadline independent of the transport default — useful because a full `since=0` pull can be slower than a typical request.

**I recommend (ii):** keep the parameter and actually use it as a per-call context deadline (defaulting to the configured `HTTPTimeout`, which is what `main.go` already passes). This makes the currently-decorative field meaningful, matches how `pullBooks` sets sync-specific timeouts (`SYNC_TIMEOUTS = {5,10}` in `readest_syncclient.lua:6` — the plugin *does* give sync its own timeout), and costs nothing. If you'd rather the constructor shrink, option (i) is a one-line removal. Flagged for your call (§13, item D).

**No dependency should be removed or added beyond this.** Notably the client does **not** take `state.Store` (unlike `Auth`) — it has nothing to persist; watermark ownership stays with the engine/store. This is a deliberate boundary.

---

## 10. Testing plan (unit-test matrix)

Mirrors the `auth_test.go` harness: a scriptable `stubDoer`, a fake `Authenticator`, no real network. The existing `models_test.go` already covers row predicates, tuple decoding, percentage, watermark, and envelope decoding — **those are not repeated here.** This matrix is for `Client.PullBooks` only.

| # | Scenario | Assert |
|---|---|---|
| 1 | Success 200, several rows | Rows decoded; `since` sent correctly in query |
| 2 | Success 200, empty `{books:[]}` | Empty slice, no error |
| 3 | `since=0` full pull | Query has `since=0` |
| 4 | Request URL/path | `GET {base}/sync`, `type=books` |
| 5 | Headers present | `Authorization: Bearer <tok>`, `Content-Type`, `Accept` |
| 6 | `AccessToken` called once per pull | Fake auth call count == 1 on success |
| 7 | `AccessToken` error | Propagates; no HTTP request fires |
| 8 | 401 → re-auth → retry succeeds | Exactly 2 HTTP calls; 2nd uses new token; returns rows |
| 9 | 403 → re-auth → retry succeeds | Same as #8 |
| 10 | Body `{error:"Not authenticated"}` (any status) → re-auth → retry | `isAuthFailure` body-sniff triggers retry |
| 11 | 401, re-auth fails | `ErrUnauthorized` (or wrapped re-auth err); exactly 1 HTTP call + 1 auth call |
| 12 | 401 → retry still 401 | `ErrUnauthorized`; exactly 2 HTTP calls (no 3rd) |
| 13 | 400 | `ErrBadRequest`; no retry (1 HTTP call) |
| 14 | 429 | `ErrRateLimited`; no client retry |
| 15 | 500 / 502 / 503 | `ErrSyncServer`; no client retry |
| 16 | Transport error from `Do` | `ErrSyncNetwork`; wraps underlying msg |
| 17 | `ctx` cancelled | `context.Canceled` propagates cleanly |
| 18 | 200 malformed JSON | `ErrSyncMalformed` |
| 19 | 200, `books` not an array (e.g. object) | `ErrSyncMalformed` |
| 20 | 200, body over `maxBodyBytes` | `ErrSyncMalformed`/read-cap error; no OOM |
| 21 | Weird `progress` in one row (stringified/null) | Row still returned; no whole-page failure |
| 22 | Dummy-hash row present | Returned **to caller unfiltered** (client is faithful; filtering is engine's) — pins the boundary |
| 23 | Timeout honored (if §9 option ii) | Slow doer → deadline error |
| 24 | Concurrent `PullBooks` with fresh token (`-race`) | No data race; token fetched via shared `Auth` |
| 25 | Error body snippet bounded | Long error bodies truncated in message |

Tests 8–12 are the load-bearing ones (the auth-retry dance); 22 pins the most important architectural boundary (client faithful, engine selective).

---

## 11. Documentation discrepancies (flagged, not invented)

1. **Re-auth method after downstream 401.** Phase 3 design §8 says the client should "detect and call `Auth.Refresh`/invalidate token," but `Refresh` alone lacks the sign-in fallback that the approved `AccessToken`/`reauth` path has. The docs don't specify the exact call. This is §5.4's open decision (option 2 recommended) — needs your ruling, not invention.
2. **Watermark source.** `planning.md` §4 says watermark = `max(updated_at)`; the reverse-engineering report (discrepancy #8) and `librarystore.lua:203` correct this to `max(synced_at, updated_at, deleted_at)` with `synced_at` winning. **The code already implements the correct rule** (`BookRow.WatermarkMs`); only the stale planning text disagrees. No action beyond noting it.
3. **`configs` bulk pull.** `planning.md` §8 issue 1 wonders whether `/sync?type=configs` supports bulk pull. Moot — Phase 4 uses `type=books` only; the question is documented as irrelevant to percentage sync (reverse-engineering report, discrepancy #5).
4. **Row fields.** `reverse-engineering-report.md` §4.1 lists more fields than `BookRow` models (`group_id`, `metadata`, `reading_status`, `uploaded_at`, `source_title`). This is intentional minimal modeling (§4.4), not an omission bug — but it's a doc/code mismatch worth recording.
5. **Progress tuple units.** `planning.md` §8 issue 2 calls units "unconfirmed"; the plugin treats the tuple as a plain `cur/total` ratio for its own progress bar (reverse-engineering report, discrepancy #5). The bridge already adopts the ratio (`util.Percent`). Residual risk: the values may be opaque internal locations, so the percentage is best-effort — a **live-validation** item (§12), not a design gap.
6. **Sync base URL configurability.** `planning.md` §5.2 says "presumably configurable — verify." The plugin hardcodes `web.readest.com/api` with no override (`readest-sync-api.json`). The bridge exposes it in config as future-proofing (defaults.go comment), which matches report discrepancy #6 — but there is no evidence any other host works. Noted, not changed.
7. **Timeout ownership.** The plugin gives sync its own timeout (`SYNC_TIMEOUTS={5,10}`); the bridge currently threads one global `HTTPTimeout`. §9 option (ii) reconciles this by letting the sync read carry its own deadline. Flagged because the docs don't address it.

---

## 12. Live validation items (cannot be determined from source)

1. **`progress` tuple units for EPUBs** — pages, locations, or opaque ordinals. Affects whether the ratio is a "true" percentage. (Report risk #1.) Test against a real account.
2. **Does `/sync?type=books` ever paginate or truncate very large libraries?** Source shows no pagination, but a huge first pull's behavior (size cap? implicit limit?) is unverified.
3. **`301` on `/api/sync`** — does the hosted service ever redirect, and does the `Authorization` header survive? (§2.2.)
4. **Does the sync API return `429`?** Not in `expected_status`; handled defensively as retryable.
5. **Exact 401-vs-403 semantics** — whether an expired-but-well-formed JWT yields 401 vs 403, and whether the `{error:"Not authenticated"}` body accompanies both (the body-sniff in `isAuthFailure` assumes the plugin's comment is accurate).
6. **Response size of a full `since=0` pull** — informs whether the proposed 4 MiB read cap is right.
7. **Whether a revoked (rotated-away) access token yields 401/403 vs a 200 with an auth-error body** — determines how reliably `isAuthFailure` catches it.
8. **`synced_at` presence on all rows** — the watermark rule prefers it; if some rows omit it, the `max(updated_at, deleted_at)` fallback (already in `WatermarkMs`) must hold. Verify it's always present in practice.

---

## 13. Proposed implementation plan

### 13.1 Architectural decisions (each explained)

1. **Stay in the flat `internal/readest` package.** The client, models, and auth already live together; `SyncClient`/`Client`/`BookRow` are only consumed by `sync.Engine`, which imports `readest`. No subpackage is warranted (same reasoning as Phase 3's ADR for auth).
2. **Implement `PullBooks` in `client.go`; remove `ErrNotImplemented` from that path.** The models and interface need no changes — the design deliberately produces zero churn in already-tested code.
3. **Add distinct sync sentinels** (§6.1) rather than reusing auth's, so the engine can tell auth-transport failures from sync-transport failures. **(Decision C — confirm.)**
4. **Add one method to `Auth` for forced re-auth** (`ForceRefresh(ctx) (string, error)`) so the client's downstream-401 path reuses Phase 3's approved re-auth policy instead of duplicating its logic. **(Decision B — confirm; alternative: client calls existing `Refresh` and forgoes the sign-in fallback.)**
5. **Honor `timeout` as a per-call context deadline** rather than dropping it. **(Decision D — confirm; alternative: remove the parameter.)**
6. **Client returns rows faithfully; filtering stays with the engine.** The client does not pre-filter dummy/deleted rows (test #22 pins this). This keeps one selection policy in one place (Phase 6) and matches the plugin's `client:pullBooks` → `syncbooks.lua` split.

### 13.2 Files to modify

- **`internal/readest/client.go`** — implement `PullBooks`; add unexported helpers (`buildRequest`, `do`, `decodeBooks`, `isAuthFailure`, `classifyStatus`); add the sync sentinel block; add `maxBodyBytes` const; (optionally) per-call `context.WithTimeout`.
- **`internal/readest/auth.go`** — add the single `ForceRefresh` method (Decision B) reusing the existing refresh/`reauth` internals. No change to existing methods.
- **`cmd/bridge/main.go`** — no change expected (constructor signature unchanged), unless Decision D chooses to drop `timeout`, in which case the call site drops one argument.

### 13.3 Files to create

- **`internal/readest/client_test.go`** — the §10 matrix, with a `stubDoer` (reusable pattern from `auth_test.go`) and a fake `Authenticator`. (`models_test.go` stays untouched; its `TestClientStubReturnsNotImplemented` is removed or rewritten since the stub is gone.)

### 13.4 Constructor changes

- `readest.NewClient` — **unchanged** under the recommended options (timeout kept and honored).
- `readest.NewAuth` — **unchanged.** `ForceRefresh` is a method addition, not a constructor change.

### 13.5 Dependency changes

- **None.** `readest` already imports `context`, `encoding/json`, `net/http`, `net/url`, `log/slog`, `time`, `fmt`, `errors`, `internal/util/httpclient`. It gains no new module dependency. The `readest → state` edge (via `Auth`) already exists from Phase 3 and is unchanged; the client itself adds no `state` dependency.

### 13.6 Package layout (unchanged)

```
internal/readest/
  doc.go           (package doc; minor wording update to drop "stub" language)
  auth.go          (+ ForceRefresh)
  auth_test.go     (unchanged)
  client.go        (PullBooks implemented)
  client_test.go   (new)
  models.go        (unchanged)
  models_test.go   (remove obsolete stub test)
```

### 13.7 Public API (net change)

```go
// existing, now functional
func NewClient(baseURL string, auth Authenticator, hc httpclient.Doer, log *slog.Logger, timeout time.Duration) *Client
func (c *Client) PullBooks(ctx context.Context, since int64) ([]BookRow, error)

// additive (Decision B)
func (a *Auth) ForceRefresh(ctx context.Context) (string, error)

// additive sentinels (Decision C, names finalized at implementation)
var ErrUnauthorized, ErrBadRequest, ErrRateLimited, ErrSyncServer, ErrSyncNetwork, ErrSyncMalformed error
```

No type is removed; `SyncClient`, `Authenticator`, `BookRow`, `BooksResponse`, `DummyHash` are all unchanged.

---

## Decisions requested before implementation

- **B.** Downstream-401 re-auth shape: add `Auth.ForceRefresh` (recommended) vs. client calls existing `Refresh` only.
- **C.** Error taxonomy: distinct sync sentinels (recommended) vs. sharing auth's transport sentinels.
- **D.** `timeout` param: keep and honor as a per-call deadline (recommended) vs. remove from `NewClient`.
- **E.** Response read cap: proposed 4 MiB constant — acceptable default pending live item #6?

Stopping here as instructed — awaiting approval before any Phase 4 implementation.
