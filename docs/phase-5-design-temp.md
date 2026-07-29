# Design: `internal/bookorbit` Client (Phase 5)

**Status:** proposal, awaiting approval. No code is written in this phase.
**Scope:** the BookOrbit-side REST client only — `Auth`, `MatchCheck`, `BulkProgress`, and the optional `UpdateProgress` fallback; their models; error taxonomy; and their integration points with the Phase 3 config (`Config.AuthKey()`, `util.NormalizeBookOrbitURL`) and Phase 2 foundation (`internal/util/batch.go`, `internal/sync/state`). Everything Readest-side and the sync engine itself are out of scope except where a boundary must be named.

Authoritative behavior is taken from `reference/koreader-plugin/bookorbit.koplugin/bookorbit_api.lua` (the entire REST surface), cross-checked against `bookorbit_sweep.lua` (real call sites and real response-field usage), `bookorbit_book_sync.lua` (single-book match-check call site), `bookorbit_progress_sync.lua` (the singular-PUT fallback endpoint), and `bookorbit_state.lua` (the match-cache shape `internal/sync/state` already mirrors). Where the planning docs disagree with the plugin source, the plugin wins, and the disagreement is flagged in §15.

---

## 1. Overall architecture and package responsibilities

`internal/bookorbit` owns exactly one thing: **turning domain-level sync facts (hashes + candidate metadata, progress items) into BookOrbit HTTP calls, and BookOrbit's JSON responses back into typed Go values or classified errors.**

This mirrors the boundary already drawn and accepted in Phase 4 (`docs/phase-4-design.md` §1): each layer owns one external system, nothing crosses a line except through the interface that line exists for.

| Concern | Owner | Crosses via |
|---|---|---|
| Static credentials (`x-auth-user`/`x-auth-key`) | `config.Config` (`AuthKey()`, already implemented) | Constructor params |
| Server URL normalization | `internal/util.NormalizeBookOrbitURL` (already implemented, already applied in `config.finalize()`) | `cfg.BookOrbit.ServerURL` is pre-normalized by the time it reaches this package |
| BookOrbit transport + decode + classification | `bookorbit.Client` (**this phase**) | `API` interface |
| Wire request/response shapes | `bookorbit` models (`models.go`, partially present, extended this phase) | value types |
| Match-cache, unmatched cooldown, per-book last-pushed percentage | `internal/sync/state.Store` (already implemented) | consumed by the engine, **not** by this package |
| Batching (500 hashes/match-check, 100 items/bulk-progress) | `internal/util.Batch`/`BatchFunc` (already implemented) + the sync engine (Phase 6) | the client sends exactly what it is given, once |
| Retry/backoff policy | `sync.Engine` (Phase 6), driven by `config.Bridge.Retry*` | the client returns classified sentinel errors only |

**What this package does NOT do**, matching the "client is faithful, engine is selective" principle already established for `internal/readest` (Phase 4 design §7):

- It does not decide which hashes need matching, or split a call into a series of batched calls — it sends the hashes/candidates/items slice it is given, in one HTTP round trip.
- It does not read or write `state.Store`. The engine reads `Match`/`UnmatchedAt` before calling, and writes `SetMatch`/`SetUnmatched`/`ClearUnmatched` after, using the response this client returns.
- It does not retry on transient failures. It classifies them into sentinel errors and returns immediately; the engine's backoff loop (Phase 6, not yet built) decides whether/when to call again.
- It does not decide whether the single-book `UpdateProgress` fallback should be used instead of `BulkProgress`. It only implements the endpoint faithfully; the *policy* of "bulk endpoint looks unsupported, switch to the fallback" is a Phase 6 concern (see §9).

---

## 2. Public API for `internal/bookorbit`

### 2.1 Interface — one required change, one addition

The interface already declared in the Phase 2 foundation (`client.go`) is:

```go
type API interface {
    Auth(ctx context.Context) error
    MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error)
    BulkProgress(ctx context.Context, req BulkProgressRequest) error
}
```

**Required fix (not optional — see §15.1):** `BulkProgress` must return the decoded response, not just an error. The only field the reference plugin reads off a bulk-progress response is `body.unmatched` (`bookorbit_sweep.lua:stepProgressNext`, verified), and the engine cannot mark a book unmatched (`state.Store.SetUnmatched`) without it. The current stub signature silently discards that data.

```go
type API interface {
    Auth(ctx context.Context) error
    MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error)
    BulkProgress(ctx context.Context, req BulkProgressRequest) (BulkProgressResponse, error)
    UpdateProgress(ctx context.Context, req UpdateProgressRequest) error // new, fallback-only (§9)
}
```

This is flagged explicitly under §20 as a decision requiring sign-off, since it changes an already-accepted public interface — but it is a correctness fix backed directly by verified plugin behavior, not a style preference.

### 2.2 Concrete type

```go
type Client struct {
    baseURL string
    username string
    authKey  string
    device   DeviceInfo   // template: DeviceID, DeviceModel, PluginVersion (static)
    http     httpclient.Doer
    log      *slog.Logger
    maxBody  int64
    timeout  time.Duration        // proposed addition, §3
    now      func() time.Time     // proposed addition, §3 — injectable clock for DeviceTime freshness
}

func NewClient(
    baseURL, username, authKey string,
    device DeviceInfo,
    hc httpclient.Doer,
    log *slog.Logger,
    maxBody int64,
    timeout time.Duration, // proposed addition
) *Client
```

`now` is not a constructor parameter — like Phase 3's `Auth.now`, it defaults to `time.Now` in `NewClient` and is only overridden directly on the struct in tests (same pattern as `auth_test.go`'s `fixedClock`).

---

## 3. Constructor dependencies and rationale

| Dep | Justification | Verdict |
|---|---|---|
| `baseURL string` | Already-normalized BookOrbit API base (`{server}/api/v1`), from `config.BookOrbit.ServerURL` via `config.finalize()` → `util.NormalizeBookOrbitURL`. | **Keep** (already in stub). |
| `username string` | Sent verbatim as `x-auth-user`. From `config.BookOrbit.Username`. | **Keep.** |
| `authKey string` | Sent verbatim as `x-auth-key`. From `config.AuthKey()` (already implemented — MD5 of password, or pre-hashed userkey normalized to lowercase). | **Keep.** No hashing happens inside this package; `config.AuthKey()` already owns that. |
| `device DeviceInfo` | Static per-bridge identity (`DeviceID`, `DeviceModel`, `PluginVersion`) sent on every device-wrapped call. Verified: `bookorbit_api.lua:new()` stores these once at construction (`self.device_id`, `self.device_model`, `self.plugin_version`). | **Keep** (already in stub). |
| `hc httpclient.Doer` | Injected transport, same seam Phase 3/4 use for mockability. | **Keep** (already in stub). |
| `log *slog.Logger` | Structured lifecycle logging; nil → `slog.Default()`, matching `NewAuth`'s convention. | **Keep** (already in stub). |
| `maxBody int64` | Client-side request-body cap. Verified: `bookorbit_api.lua:requestBlocking` checks `#body_json > MAX_BODY_BYTES` (900×1024) **after encoding, before sending**, returning `"body_too_large"` without dispatching the request. From `config.Bridge.MaxBodyBytes` (already defaulted to 900×1024, matching the plugin exactly). | **Keep** (already in stub); this phase must actually *use* it. |
| `timeout time.Duration` | **Proposed addition**, mirroring Phase 4 Decision D (accepted): honor as a per-call `context.WithTimeout`. Justification: the plugin does give BookOrbit calls their own timeout tier (`socketutil.LARGE_BLOCK_TIMEOUT`/`LARGE_TOTAL_TIMEOUT`, distinct from the file-transfer timeout tier) — verified in `bookorbit_api.lua:requestBlocking`, though the exact durations aren't in the provided source (§16.5). A per-call deadline independent of the transport default matters more here than for Readest, since bulk-progress payloads can approach 900 KiB on slow self-hosted hardware. Defaults to `config.Bridge.HTTPTimeout` at the call site, same as `readest.NewClient`. | **Flag for approval** (§20-D). |
| `now func() time.Time` | Not a constructor parameter; a settable field defaulting to `time.Now`, used only to stamp `deviceTime`/`device_time` fields fresh on every request (see §7.4). Mirrors Phase 3's injectable-clock convention (`Auth.now`). | Internal field, no constructor change. |

**Not injected, deliberately:** `state.Store`. Unlike `readest.Auth`, this client has nothing to persist — no token, no rotation, no watermark. Match-cache ownership stays entirely with the engine, matching the boundary in §1.

---

## 4. Required request/response models

### 4.1 Already correct, keep as-is

- `Match { Hash, BookFileID, BookID }` — verified against `bookorbit_sweep.lua:stepMatchNext` (`match.hash`, `match.bookFileId`, `match.bookId`).
- `MatchCheckResponse { Matches, Unmatched, LibraryVersion }` — verified against `stepMatchNext` and `bookorbit_book_sync.lua:stepMatch` (`body.matches`, `body.unmatched`, `body.libraryVersion`).
- `ProgressItem { Hash, Percentage, Progress, Timestamp }` — verified against `stepProgressNext`'s `progress_items` construction (`{hash, percentage, progress, timestamp}`).
- `DeviceInfo { DeviceID, DeviceModel, PluginVersion, DeviceTime }` and `DeviceTimeFormat` — verified against `withDevice`.
- `BulkProgressRequest { DeviceID, DeviceModel, PluginVersion, DeviceTime, Items }` and `DeviceInfo.WithDevice(items)` — verified against `bulkProgress(items)` → `self:withDevice({items = items})`.

### 4.2 Required corrections (gaps found while designing Phase 5)

**(a) `MatchCandidate` is missing `MetadataAmbiguous`.**
Verified: `bookorbit_api.lua:matchCheck` builds each candidate as `{hash, title, authors, lastOpen, source, metadataAmbiguous = cand.metadata_ambiguous}`. The current stub omits `metadataAmbiguous` entirely. Both real call sites populate it — `bookorbit_sweep.lua`'s `ctx.candidates[md5].metadata_ambiguous` and `bookorbit_book_sync.lua`'s `ctx.snap.metadata_ambiguous`.

```go
type MatchCandidate struct {
    Hash              string `json:"hash"`
    Title             string `json:"title"`
    Authors           string `json:"authors"`
    LastOpen          int64  `json:"lastOpen"`
    Source            string `json:"source"`
    MetadataAmbiguous bool   `json:"metadataAmbiguous"`
}
```

**(b) `MatchCheckRequest` is missing the device wrapper.**
Verified: `matchCheck` dispatches via `self:request("POST", "/koreader/plugin/match-check", self:withDevice(payload))` — **every** field-check payload is device-wrapped, not just bulk-progress. The current stub's `MatchCheckRequest` has only `Hashes`/`Books`. Mirroring the already-accepted `BulkProgressRequest` pattern exactly:

```go
type MatchCheckRequest struct {
    Hashes        []string         `json:"hashes"`
    Books         []MatchCandidate `json:"books"`
    DeviceID      string           `json:"deviceId"`
    DeviceModel   string           `json:"deviceModel"`
    PluginVersion string           `json:"pluginVersion"`
    DeviceTime    string           `json:"deviceTime"`
}

func (d DeviceInfo) WithMatchCheck(hashes []string, books []MatchCandidate) MatchCheckRequest {
    return MatchCheckRequest{
        Hashes: hashes, Books: books,
        DeviceID: d.DeviceID, DeviceModel: d.DeviceModel,
        PluginVersion: d.PluginVersion, DeviceTime: d.DeviceTime,
    }
}
```

**(c) `BulkProgressResponse.Updated` is unverified and should be replaced.**
No line in any provided reference source reads an `updated` field from a bulk-progress response. The only field the plugin consumes is `body.unmatched` (`stepProgressNext`, verified twice — sweep and, by the same shape, any future single-call use). Per "never invent APIs," `Updated` should be removed rather than kept as a guess:

```go
type BulkProgressResponse struct {
    Unmatched []string `json:"unmatched"`
}
```

**(d) New: `UpdateProgressRequest`** (fallback endpoint, §9), verified against `bookorbit_api.lua:updateProgress`:

```go
// Note the field naming deliberately does NOT match the plugin's own
// camelCase convention (deviceId, pluginVersion, ...) used everywhere else
// in this package. That's not a bridge inconsistency — /koreader/syncs/progress
// is the kosync-compatible endpoint shared with vanilla KOReader's stock sync
// plugin, not a BookOrbit-native /koreader/plugin/* endpoint, and it uses
// kosync's own snake_case-ish wire shape. Verified: bookorbit_api.lua:updateProgress.
type UpdateProgressRequest struct {
    Document   string  `json:"document"`
    Percentage float64 `json:"percentage"`
    Progress   string  `json:"progress"`
    Device     string  `json:"device"`
    DeviceID   string  `json:"device_id"`
    Timestamp  int64   `json:"timestamp"`
}

func (d DeviceInfo) WithUpdateProgress(document string, percentage float64, progress string, timestamp int64) UpdateProgressRequest {
    return UpdateProgressRequest{
        Document: document, Percentage: percentage, Progress: progress,
        Device: d.DeviceModel, DeviceID: d.DeviceID, Timestamp: timestamp,
    }
}
```

No response model is needed for `UpdateProgress`: the plugin's own call sites (`bookorbit_progress_sync.lua`) treat the PUT as success/failure only and never read a field back from its body.

No response model is needed for `Auth`: the plugin only checks `body ~= nil` (`bookorbit_main_menu.lua:testConnection`). `Auth(ctx) error` stays exactly as declared.

---

## 5. Exact request and response JSON shapes

### `POST /koreader/plugin/match-check`

Request:
```json
{
  "hashes": ["h1", "h2"],
  "books": [
    { "hash": "h1", "title": "T", "authors": "A", "lastOpen": 1700000000, "source": "readest", "metadataAmbiguous": false }
  ],
  "deviceId": "uuid-...",
  "deviceModel": "readest-bridge",
  "pluginVersion": "0.1.0",
  "deviceTime": "2026-01-02 03:04:05"
}
```
`hashes` and `books` must serialize as `[]`, never `null`, even when empty — see §7.5.

Response:
```json
{
  "matches": [ { "hash": "h1", "bookFileId": 42, "bookId": 7 } ],
  "unmatched": ["h2"],
  "libraryVersion": "opaque-token"
}
```
`matches`/`unmatched` may be absent or `null` on the wire; the plugin treats both as `{}` (`body.matches or {}`), so the client must normalize to non-nil empty slices on decode (mirrors `readest.decodeBooks`'s `Books == nil` normalization, already an established pattern in this codebase).

### `POST /koreader/plugin/progress`

Request:
```json
{
  "items": [
    { "hash": "h1", "percentage": 0.5, "progress": "", "timestamp": 1700000000 }
  ],
  "deviceId": "uuid-...",
  "deviceModel": "readest-bridge",
  "pluginVersion": "0.1.0",
  "deviceTime": "2026-01-02 03:04:05"
}
```

Response:
```json
{ "unmatched": ["h1"] }
```

### `PUT /koreader/syncs/progress` (fallback)

Request:
```json
{
  "document": "h1",
  "percentage": 0.5,
  "progress": "",
  "device": "readest-bridge",
  "device_id": "uuid-...",
  "timestamp": 1700000000
}
```
No device wrapper — this is the kosync shape, not the BookOrbit-plugin shape. No response body is consumed.

### `GET /koreader/users/auth`

No request body. Response body shape unconfirmed and unmodeled (§16.2) — success is simply "the request returned a non-error body," matching `testConnection`'s `if body then ...`.

---

## 6. HTTP endpoints, methods, headers, authentication, signing

| Endpoint | Method | Body | In scope |
|---|---|---|---|
| `/koreader/users/auth` | GET | none | Yes (health check) |
| `/koreader/plugin/match-check` | POST | JSON | Yes |
| `/koreader/plugin/progress` | POST | JSON | Yes |
| `/koreader/syncs/progress` | PUT | JSON | Yes, fallback only (§9) |
| `/koreader/syncs/progress/{digest}` | GET | none | **Out of scope** — one-way bridge never pulls BookOrbit progress |
| `/koreader/plugin/page-stats`, `/koreader/plugin/annotations*`, `/koreader/plugin/book-states`, `/koreader/plugin/sweeps` | POST | JSON | **Out of scope** — reading time, highlights, status/rating, sweep-complete signal. Not calling `sweeps` means BookOrbit's own "last sweep" UI won't reflect bridge activity; this is an accepted, deliberate scope exclusion, not an oversight. |
| `/koreader/plugin/catalog/*`, `/koreader/plugin/version`, `/koreader/plugin/package` | GET/PUT | — | **Out of scope** — catalog browsing and self-update; irrelevant to a headless Go binary. |

**Headers, every request:**
- `accept: application/json`
- `x-auth-user: <username>`
- `x-auth-key: <authKey>`

**Headers, only when a body is present** (POST/PUT):
- `Content-Type: application/json`
- `Content-Length` — verified the plugin sets this explicitly (`request.headers["Content-Length"] = #body_json`). In Go, `net/http` computes `Content-Length` automatically when the request body is a `*bytes.Reader`/`*bytes.Buffer` (both implement `Len()`), so no manual header is needed — this is a Go-runtime detail to verify during implementation (§16.6), not a design gap.

**Authentication:** static, no expiry, no refresh, no OAuth, no signing beyond the two fixed headers above. This is a hard architectural contrast with `readest.Auth` and should not be given a token-refresh code path — there is none in the reference plugin (`bookorbit_api.lua` sends `x-auth-user`/`x-auth-key` unconditionally on `NewClient`, with no rotation logic anywhere in the file).

**On a 401/403:** unlike the Readest client, there is **no** re-auth-and-retry dance, because there is no second credential state to refresh. A 401/403 means the configured username/authKey are wrong (or the server rejected them for another reason) and is terminal for that call.

---

## 7. `MatchCheck` behavior, batching, caching, response handling

### 7.1 What the client does

One HTTP round trip: given a `MatchCheckRequest` (hashes + candidates, already device-stamped via `DeviceInfo.WithMatchCheck` or built by hand), POST it, decode the response into `MatchCheckResponse`, normalize `Matches`/`Unmatched` to non-nil, return.

### 7.2 Batching — NOT the client's job

Verified: `MATCH_BATCH = 500` in `bookorbit_sweep.lua`. The chunking loop (`ctx.match_queue`, built with `for i = 1, #to_check, MATCH_BATCH`) lives in the sweep orchestration, not in `bookorbit_api.lua:matchCheck` itself — `matchCheck` always sends exactly the hashes it's handed. This confirms the architectural split already decided in §1: **the engine (Phase 6) chunks using `internal/util.Batch`/`BatchFunc` against `config.Bridge.MatchBatchSize`, calling `Client.MatchCheck` once per chunk.** No batching logic belongs in this package.

### 7.3 Caching — NOT the client's job

The match cache (hash → `bookFileId`/`bookId`) and the unmatched cooldown (`config.Bridge.UnmatchedCooldown`) are entirely `state.Store`'s responsibility, already implemented in Phase 2 (`Match`/`SetMatch`/`DeleteMatch`, `UnmatchedAt`/`SetUnmatched`/`ClearUnmatched`). This client is stateless between calls — it holds no cache and must not.

### 7.4 Response handling and the device-time freshness problem

This is the one nuance worth flagging explicitly. `withDevice` in Lua calls `os.date(...)` fresh, immediately before every dispatch (`bookorbit_api.lua:withDevice`, called from inside `matchCheck`/`bulkProgress` themselves, not by the caller). If the Go engine builds a batch of `MatchCheckRequest` values up front (e.g., while iterating `ctx.match_queue`) and only *later* calls `Client.MatchCheck` on each, a caller-stamped `DeviceTime` could go stale relative to when the request actually goes out — a divergence from the plugin's exact behavior.

**Design decision (flag for approval, §20-E):** `Client.MatchCheck`/`BulkProgress`/`UpdateProgress` overwrite `DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime` on the request they receive, using the client's own static `device` field and `c.now()`, immediately before marshaling — regardless of what the caller populated. This:
- Exactly reproduces the plugin's "stamp at dispatch time" behavior.
- Removes any need for the engine to worry about staleness across a batch loop.
- Makes `DeviceInfo.WithMatchCheck`/`WithDevice`/`WithUpdateProgress` convenience constructors rather than the only correct way to build a request — a caller could pass a zero-value device wrapper and the client would still stamp it correctly.

### 7.5 Empty-array encoding

Verified: the plugin's `rapidjson.encode(body, {empty_table_as_array = true})` exists specifically because "the backend rejects empty `{}` where it expects arrays" (`reference-map.md`, Key Behavioral Constants table). Go's `encoding/json` already encodes a **non-nil, empty** slice as `[]` — the only failure mode is a **nil** slice encoding as `null`. The client must therefore normalize `Hashes`/`Books`/`Items` to non-nil empty slices (`make([]T, 0)`) before marshaling if the caller passed `nil`. This is a pure Go-side implementation detail, fully within our control — no live validation needed (§15.10).

---

## 8. `BulkProgress` behavior, request construction, response handling, limits

### 8.1 What the client does

One HTTP round trip: given a `BulkProgressRequest` (already or about-to-be device-stamped, see §7.4), POST it, decode into `BulkProgressResponse{Unmatched}`, normalize `Unmatched` to non-nil, return `(BulkProgressResponse, error)`.

### 8.2 Batching

Verified: `PROGRESS_BATCH = 100` in `bookorbit_sweep.lua`. Same split as §7.2 — the engine chunks via `util.BatchFunc` against `config.Bridge.ProgressBatchSize`, calling `Client.BulkProgress` once per chunk of ≤100 items. Not this package's job.

### 8.3 Limits

`config.Bridge.MaxBodyBytes` (900×1024, verified against `MAX_BODY_BYTES` in `bookorbit_api.lua`) is enforced by the client **after** JSON-encoding the fully device-stamped request and **before** dispatching: if `len(encoded) > maxBody`, return `ErrBodyTooLarge` and make no HTTP call. This mirrors the plugin's exact ordering (`request(...)`: encode → check length → only then build the socket request).

### 8.4 Response handling

`unmatched` is the only field consumed anywhere in the reference source (§4.2c). A hash appearing in `unmatched` means the engine should call `state.Store.SetUnmatched(hash, now)` and mark that book's push as failed for this pass (`pending.failed = true` in `stepProgressNext` — Phase 6 concern, not this client's).

---

## 9. Optional `UpdateProgress` fallback

**Should it exist?** Yes — implement it in Phase 5. Two independent justifications:
1. `docs/implementation-roadmap.md`'s own Phase 5 scope explicitly lists it: *"Fallback UpdateProgress(...) single-item PUT (optional but recommended for version fallback)."*
2. `docs/planning.md` §6 recommendation 1 and `docs/reverse-engineering-report.md` §6 recommendation 1 both independently conclude bulk-progress is the primary path and the singular PUT is a version-compatibility fallback, not a routine code path.

**When should it be used?** Only when `BulkProgress` is confirmed unsupported by the target server (e.g., an older BookOrbit build without `/koreader/plugin/progress`). **This phase does not decide the trigger condition or implement the fallback policy** — that is Phase 6's job, once the engine exists to hold "is bulk unsupported" state. The closest verified analog in the reference source is `bookorbit_sweep.lua`'s legacy annotation-upload fallback (`ctx.use_legacy_annotations`, triggered on `err == "unsupported_server"` from the exchange endpoint), but that is a **different endpoint pair** (annotations exchange vs. legacy upload) — there is no direct reference precedent for a bulk-progress → singular-PUT fallback trigger. Extrapolating that pattern to progress endpoints is therefore a **bridge-specific design decision**, not a verified plugin behavior, and is deferred entirely to Phase 6's design.

**Why not implement the fallback *policy* here too?** Because the trigger condition depends on `state.Store` (to remember "this server doesn't support bulk, stop trying") and on retry/backoff sequencing, both of which are explicitly Phase 6 concerns per the roadmap. Phase 5's job is only to make the endpoint callable.

---

## 10. Error taxonomy and retry classification

Following the established Phase 4 pattern exactly (distinct, `errors.Is`-matchable sentinels, each wrapped via `%w` with status + a bounded body snippet):

```go
var (
    ErrUnauthorized        = errors.New("bookorbit: unauthorized")            // 401/403 — terminal, credentials wrong
    ErrBadRequest          = errors.New("bookorbit: bad request")             // 400 — non-retryable
    ErrUnsupportedEndpoint = errors.New("bookorbit: endpoint not supported")   // 404/405 — signals version-fallback (§9)
    ErrRateLimited         = errors.New("bookorbit: rate limited")            // 429 — not documented but defensively handled, retryable
    ErrServer              = errors.New("bookorbit: server error")            // 5xx — retryable
    ErrNetwork             = errors.New("bookorbit: network error")           // transport/timeout — retryable
    ErrMalformedResponse   = errors.New("bookorbit: malformed response")      // 2xx but undecodable — non-retryable
    ErrBodyTooLarge        = errors.New("bookorbit: request body too large")  // client-side, never sent — non-retryable
)
```

`ErrUnsupportedEndpoint` (404/405) is distinct from `ErrBadRequest` (400) specifically so Phase 6 can key its fallback-trigger logic off `errors.Is(err, bookorbit.ErrUnsupportedEndpoint)` without conflating it with a generic client bug. This mirrors how Phase 4 gave `isDashboardUnsupported` (404/405) its own check in the BookOrbit KOReader plugin's own catalog code (`bookorbit_catalog.lua`), the closest verified precedent for "404/405 means feature unsupported" in this codebase — a legitimate pattern to reuse, even though it's from a different (catalog) endpoint.

| Failure | Sentinel | Retryable by engine? |
|---|---|---|
| 401/403 | `ErrUnauthorized` | No — credentials are wrong, retrying won't help |
| 400 | `ErrBadRequest` | No |
| 404/405 | `ErrUnsupportedEndpoint` | No (but signals: try the fallback / stop calling this endpoint) |
| 429 | `ErrRateLimited` | Yes, with backoff |
| 5xx | `ErrServer` | Yes, with backoff |
| network/timeout/ctx | `ErrNetwork` | Yes, with backoff (unless `context.Canceled`, which propagates transparently per Phase 4 precedent) |
| 2xx, bad JSON | `ErrMalformedResponse` | No |
| encoded body > `maxBody` | `ErrBodyTooLarge` | No — request is never sent; retrying without shrinking the batch cannot help |

`ErrBodyTooLarge` is a **bridge-specific improvement** over the reference plugin's own error model: `bookorbit_api.lua` returns a bare string `"body_too_large"` which its own caller-side `isTransportError(err) = type(err) ~= "number"` classifies as transport-retryable — almost certainly a latent bug in the Lua reference (a request that's too large stays too large on retry). The Go client should not copy this; it gets its own precise, non-retryable sentinel. This deviation is deliberate and documented here rather than silently diverging.

---

## 11. Interaction boundaries with the future sync engine

Phase 6, once built, will:

1. Compute the set of hashes to match-check: any hash from `readest.BookRow` not already resolved via `state.Store.Match(hash)`, and not in cooldown per `state.Store.UnmatchedAt(hash)` + `config.Bridge.UnmatchedCooldown`.
2. Chunk that set with `util.Batch(hashes, config.Bridge.MatchBatchSize)`; for each chunk, build the matching `[]MatchCandidate` and call `Client.MatchCheck` once.
3. On response: `SetMatch` for each `Match`; `SetUnmatched(hash, now)` for each `Unmatched` entry not already matched.
4. Build `[]ProgressItem` for every matched book whose `Percentage()` differs from `state.Store.Match(hash).LastPushedPct`.
5. Chunk with `util.BatchFunc(items, config.Bridge.ProgressBatchSize, ...)`; for each chunk, call `Client.BulkProgress`.
6. On response: for each hash in `Unmatched`, call `state.Store.SetUnmatched`; for the rest, update `MatchRecord.LastPushedAt`/`LastPushedPct`.
7. Apply `config.Bridge.Retry*` backoff around any call that returns `ErrRateLimited`/`ErrServer`/`ErrNetwork`.
8. Decide, using its own state (not this package's), whether to fall back to `Client.UpdateProgress` per book (§9).

This package exposes exactly what step 1–8 need and nothing else — no method here performs steps 1, 2, 3, 5, 6, 7, or 8.

---

## 12. Concurrency expectations

**No synchronization needed**, and for an even simpler reason than Phase 4's `readest.Client`: this client has no shared mutable resource at all. `Client`'s fields (`baseURL`, `username`, `authKey`, `device`, `http`, `log`, `maxBody`, `timeout`, `now`) are all immutable after construction (the `now` field is a function value, not shared mutable state). There is no token to refresh, no mutex-guarded critical section anywhere in this package. `Client` is safe for concurrent use by construction, not by discipline.

In practice, per `docs/implementation-brief.md` ("Polling daemon... No bidirectional sync"), the engine calls this client serially within one `RunOnce` pass, so concurrency safety is a free property rather than a load-bearing requirement — matching the identical conclusion reached for the Readest client in Phase 4 (§8).

---

## 13. Timeouts, body-size limits, and resource ownership

- **Per-call deadline:** `Client` applies `context.WithTimeout(ctx, c.timeout)` at the top of each exported method when `c.timeout > 0`, exactly as `readest.Client.PullBooks` does. Default value comes from `config.Bridge.HTTPTimeout` (30s), same source Phase 4 used, threaded through at the CLI wiring layer (main.go — see §16.6, cannot confirm exact current wiring from provided sources).
- **Body-size cap:** `c.maxBody`, enforced client-side after encoding, before dispatch (§8.3), sourced from `config.Bridge.MaxBodyBytes` (900×1024).
- **Error-body read cap:** a package-local `maxErrorBodyBytes = 512` constant, matching Phase 3/4's bound on how much of a non-2xx body is read into a wrapped error message.
- **Response-body read cap:** unlike `readest.Client.decodeBooks` (4 MiB, because a full library pull can be large), BookOrbit's responses here (`match-check`, `bulk-progress`, `auth`, singular PUT) are all small, bounded acknowledgments — a generous but much smaller cap (e.g. 256 KiB) is sufficient and prevents a misbehaving server from flooding memory. Exact value is a Phase 5 implementation detail, not architecturally significant; flagged for confirmation at implementation time rather than fixed here.
- **Ownership:** the `http.Client`/`Doer` itself (its own transport-level timeout, connection pooling, TLS config) is owned by whoever constructs the `httpclient.Doer` passed into `NewClient` — i.e., the CLI wiring layer, not this package. This package only owns the *per-call* deadline layered on top, same split as Phase 4.

---

## 14. Complete unit-test matrix

Mirrors the Phase 4 harness style: a scriptable `stubDoer` (reusable pattern already established in `auth_test.go`/`client_test.go` for `readest`), no real network, an injectable clock for `DeviceTime` assertions.

### `Auth()`
1. Success (200, any/empty body) → nil error.
2. 401 → `ErrUnauthorized`.
3. 403 → `ErrUnauthorized`.
4. Network error → `ErrNetwork`, wraps underlying message.
5. Headers: `x-auth-user`, `x-auth-key`, `accept` present; no `Content-Type`/`Content-Length` (GET, no body).
6. Method/path: `GET /koreader/users/auth`.

### `MatchCheck()`
7. Success with `matches` + `unmatched` + `libraryVersion` → all three decoded correctly.
8. Success with `matches`/`unmatched` absent or `null` → both normalize to non-nil empty slices.
9. Request shape: `hashes` and `books` always serialize as `[]`, never `null`, even for a nil/empty input slice.
10. Device fields (`deviceId`, `deviceModel`, `pluginVersion`, `deviceTime`) present at the top level of the request body, sibling to `hashes`/`books` (not nested).
11. `DeviceTime` reflects `c.now()` at call time, overwriting any value the caller pre-populated (freshness test, §7.4).
12. `MetadataAmbiguous` round-trips correctly for both `true` and `false` candidates.
13. Headers: `x-auth-user`/`x-auth-key`/`accept`/`Content-Type: application/json` present; `Content-Length` matches encoded body length (verified via Go's `net/http` auto-computation, not manually set).
14. 401/403 → `ErrUnauthorized`.
15. 400 → `ErrBadRequest`.
16. 404 → `ErrUnsupportedEndpoint`.
17. 405 → `ErrUnsupportedEndpoint`.
18. 429 → `ErrRateLimited`.
19. 500/502/503 → `ErrServer`.
20. Transport error → `ErrNetwork`.
21. 200 with invalid JSON → `ErrMalformedResponse`.
22. Request body (after device-stamping + encoding) exceeds `maxBody` → `ErrBodyTooLarge`, **zero** HTTP calls made.
23. `ctx` cancelled before dispatch → `context.Canceled` propagates untouched.
24. Single-hash call (mirrors `bookorbit_book_sync.lua`'s one-book match-check shape) succeeds identically to a multi-hash call.

### `BulkProgress()`
25. Success → `(BulkProgressResponse{Unmatched: [...]}, nil)`.
26. Request shape: `items` array present, device-wrapped, never `null` when empty.
27. `unmatched` absent/`null` in response → normalizes to empty, no error, no nil-deref.
28. 401/403 → `ErrUnauthorized`.
29. 400 → `ErrBadRequest`.
30. 404/405 → `ErrUnsupportedEndpoint`.
31. 429 → `ErrRateLimited`.
32. 5xx → `ErrServer`.
33. Network error → `ErrNetwork`.
34. 200 malformed JSON → `ErrMalformedResponse`.
35. Body exceeds `maxBody` → `ErrBodyTooLarge`, no request sent.
36. Empty `items` slice → client still sends the request faithfully (does not special-case zero items); documented as an engine-side responsibility to avoid calling with nothing to push.

### `UpdateProgress()`
37. Success → nil error.
38. Request shape: PUT, snake-case-ish keys (`document`, `percentage`, `progress`, `device`, `device_id`, `timestamp`) — explicitly asserted to differ from the camelCase device-wrapper shape used elsewhere.
39. No device-wrapper fields present (no `deviceId`/`pluginVersion`/`deviceTime` in this endpoint's body).
40. `device`/`device_id` reflect the client's static `DeviceInfo`, overwriting caller-supplied values.
41. `timestamp` passes through unmodified from the caller (not stamped by the client — it is domain data, not a freshness field).
42. 401/403 → `ErrUnauthorized`.
43. 404/405 → `ErrUnsupportedEndpoint`.
44. Network error → `ErrNetwork`.

### Cross-cutting
45. `NewClient` with `nil` logger defaults to `slog.Default()`.
46. `NewClient` with `timeout <= 0` disables the per-call deadline (falls through to whatever the `Doer` itself enforces).
47. Per-call deadline test: a slow `stubDoer` response exceeding `timeout` yields `context.DeadlineExceeded`/`ErrNetwork`.
48. Error messages bound the echoed body to `maxErrorBodyBytes` (long error bodies truncated).
49. Concurrent calls to `MatchCheck`/`BulkProgress` from multiple goroutines (`-race`) — no data race, since `Client` holds no shared mutable state.
50. Base-URL joining: client appends the endpoint path to `baseURL` as-is; it does **not** re-run `util.NormalizeBookOrbitURL` (that already happened in `config.finalize()`), so a client constructed with an unnormalized base is out of contract, not defended against here — matches `readest.Client`'s equivalent assumption.

---

## 15. Documentation discrepancies, ambiguities, and assumptions

1. **(Verified plugin source, required fix)** `BulkProgress` currently returns only `error`, discarding the response. The plugin's own sweep code reads `body.unmatched` off exactly this call. The interface must return `(BulkProgressResponse, error)`.
2. **(Verified plugin source, required fix)** `MatchCandidate` is missing `MetadataAmbiguous`, populated by both real call sites.
3. **(Verified plugin source, required fix)** `MatchCheckRequest` is missing the device wrapper (`deviceId`/`deviceModel`/`pluginVersion`/`deviceTime`) that `withDevice` applies to every match-check payload, not just bulk-progress.
4. **(Verified plugin source, required fix)** `BulkProgressResponse.Updated` is speculative and unconfirmed by any provided source; no plugin code reads such a field from a bulk-progress response. Replace with the verified `Unmatched []string`.
5. **(Bridge-specific design decision)** The client overwrites `DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime` on every request immediately before encoding, rather than trusting whatever the caller populated via the `WithDevice`/`WithMatchCheck`/`WithUpdateProgress` helpers. Justified in §7.4 as faithfully reproducing the plugin's "stamp at dispatch time" behavior and eliminating staleness across a batched engine loop.
6. **(Bridge-specific design decision)** `ErrBodyTooLarge` is a distinct, non-retryable sentinel, deliberately diverging from the plugin's own crude "any string error is transport-like, therefore retryable" classification (`isTransportError`), which appears to be an unintentional latent bug in the Lua reference rather than a deliberate choice.
7. **(Bridge-specific design decision, inferred/extrapolated)** `ErrUnsupportedEndpoint` (404/405) as the trigger signal for a future bulk→singular-PUT fallback is extrapolated from the *annotations* exchange endpoint's `unsupported_server` fallback pattern in `bookorbit_sweep.lua` — no reference source shows this exact fallback for the progress endpoints specifically. Flagged so it is not mistaken for verified behavior.
8. **(Assumption, Go-runtime detail, no live validation needed)** Go's `net/http` auto-computes `Content-Length` for `*bytes.Reader`/`*bytes.Buffer` request bodies, satisfying the plugin's explicit header-setting requirement without manual code. This is fully within our control to verify at implementation time via a unit test (§14 item 13), not an external unknown.
9. **(Assumption, Go-runtime detail, no live validation needed)** Go's `encoding/json` encodes non-nil empty slices as `[]`; the client must simply ensure it never marshals a `nil` slice for `hashes`/`books`/`items`. Also fully within our control.
10. **(Ambiguity)** Exact response-body read cap for `match-check`/`bulk-progress`/`auth`/`update-progress` is not architecturally significant (these are small acknowledgment bodies) and is left as an implementation-time constant rather than a design decision.

---

## 16. Live-validation items (cannot be determined from source)

1. Whether a real `bulk-progress` response ever carries fields beyond `unmatched` (would justify restoring some form of an `updated`/count field with actual evidence).
2. Whether `GET /koreader/users/auth`'s response body carries any usable fields, or is genuinely just `{}`/whatever-truthy on success.
3. **The exact status code(s) an older BookOrbit server returns when `/koreader/plugin/progress` doesn't exist** — 404, 405, or something else — which determines whether `ErrUnsupportedEndpoint`'s classification (404/405) is correctly scoped for Phase 6's eventual fallback trigger.
4. Whether BookOrbit's self-hosted servers ever return 429 in practice (handled defensively regardless, per the same posture Phase 4 took for Readest).
5. Real-world round-trip timing for a ~900 KiB bulk-progress upload against typical self-hosted NAS hardware, to validate whether the default 30s `HTTPTimeout` is sufficient or whether operators will need to raise it in `bridge.yaml`.
6. Whether `device_id`/`device` on the singular PUT endpoint have any server-side validation (e.g., rejecting an empty string) that could turn an edge case into a 400.
7. The exact numeric values of `socketutil.LARGE_BLOCK_TIMEOUT`/`LARGE_TOTAL_TIMEOUT` (module not present in the provided source), which would confirm whether BookOrbit's own KOReader plugin gives itself materially more headroom than the bridge's default 30s.

---

## 17. Proposed package layout

```
internal/bookorbit/
  doc.go            (package doc; drop "stub" language once implemented)
  client.go          (Client, NewClient, all four methods, sentinel errors, helpers)
  client_test.go     (new — the §14 matrix, stubDoer pattern reused from internal/readest)
  models.go          (extended: MatchCandidate.MetadataAmbiguous, MatchCheckRequest device
                       fields + WithMatchCheck, BulkProgressResponse.Unmatched,
                       UpdateProgressRequest + WithUpdateProgress)
  models_test.go     (extended: assert MetadataAmbiguous round-trips, assert MatchCheckRequest
                       device fields present; TestClientStubReturnsNotImplemented removed)
```

No new files beyond `client_test.go`; no subpackage — `internal/bookorbit` stays flat, matching the precedent set by keeping `internal/readest` flat through Phases 3–4 (same reasoning: the types are only consumed by this package and the future engine, and a split would be churn without benefit).

---

## 18. Files that will be modified or created

**Modify:**
- `internal/bookorbit/models.go` — §4.2 additions/corrections.
- `internal/bookorbit/client.go` — implement all four methods; add sentinel error block; add `timeout`/`now` fields; remove `ErrNotImplemented` once no method returns it.
- `internal/bookorbit/models_test.go` — remove `TestClientStubReturnsNotImplemented`; extend `TestMatchCheckRequestShape` (or add a sibling test) to cover the new device fields and `MetadataAmbiguous`.
- `cmd/bridge/main.go` — **assumption flagged, not verified**: the provided sources do not include `main.go`, so I cannot confirm whether it already constructs a `bookorbit.Client`. If it does, the call site needs the new `timeout` constructor argument (pending §20-D). This must be checked at implementation time rather than assumed.

**Create:**
- `internal/bookorbit/client_test.go` — the full §14 matrix.

---

## 19. Public API changes

- `API.BulkProgress` signature changes from `(ctx, req) error` to `(ctx, req) (BulkProgressResponse, error)` — **breaking**, but nothing in the codebase yet depends on the old signature (only the stub itself and its own test).
- `API` gains `UpdateProgress(ctx, req UpdateProgressRequest) error` — additive.
- `MatchCandidate` gains `MetadataAmbiguous bool` — additive.
- `MatchCheckRequest` gains `DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime` — additive (existing callers that don't set them get empty strings, then the client overwrites them anyway per §7.4).
- `BulkProgressResponse.Updated` is removed and replaced by `Unmatched []string` — **breaking**, but again nothing depends on the old field.
- New exported sentinel errors: `ErrUnauthorized`, `ErrBadRequest`, `ErrUnsupportedEndpoint`, `ErrRateLimited`, `ErrServer`, `ErrNetwork`, `ErrMalformedResponse`, `ErrBodyTooLarge`.
- `ErrNotImplemented` is removed from `internal/bookorbit` once no method returns it (confirmed unused elsewhere — `internal/sync/engine.go` has its own distinct `sync.ErrNotImplemented`, not this one).
- `NewClient` gains a `timeout time.Duration` parameter — pending approval (§20-D).

---

## 20. Explicit decisions requiring approval before implementation

- **A.** `BulkProgress` return-type fix (`error` → `(BulkProgressResponse, error)`). Recommended as effectively mandatory — confirm.
- **B.** `MatchCheckRequest`/`MatchCandidate`/`BulkProgressResponse` model corrections in §4.2(a–c). Recommended, verified gaps — confirm.
- **C.** Add `UpdateProgress` now, in Phase 5, per the roadmap's explicit call-out — vs. deferring the whole method to Phase 6 alongside its fallback policy. Recommend implementing the endpoint now, policy later.
- **D.** Add a `timeout time.Duration` constructor parameter, honored as a per-call deadline (mirrors Phase 4 Decision D) — vs. relying solely on the injected `Doer`'s own timeout.
- **E.** Client overwrites device-wrapper fields (`DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime`) on every request at send time, ignoring whatever the caller populated. Recommended per §7.4/§15.5 — confirm, since it means `WithDevice`/`WithMatchCheck`/`WithUpdateProgress` become convenience-only rather than load-bearing.
- **F.** Error taxonomy naming and the specific inclusion of `ErrUnsupportedEndpoint` (404/405) as a distinct sentinel for future fallback-trigger use, despite no direct reference precedent for this exact endpoint pair (§15.7) — confirm the extrapolation is acceptable.
- **G.** `ErrBodyTooLarge`'s classification as strictly non-retryable, deliberately diverging from the plugin's own (likely buggy) treatment of `"body_too_large"` as retryable — confirm the deviation.

Stopping here as instructed — awaiting approval before any Phase 5 implementation.
