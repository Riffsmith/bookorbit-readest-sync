# Design: `internal/bookorbit` Client (Phase 5)

**Status:** implemented and accepted. This document is the authoritative design record for the shipped client — it describes the code as built in `internal/bookorbit/client.go`, `models.go`, `client_test.go`, and `models_test.go`, not a proposal.
**Scope:** the BookOrbit-side REST client only — `Auth`, `MatchCheck`, `BulkProgress`, and `UpdateProgress` (the singular-PUT fallback endpoint); their models; error taxonomy; and their integration points with the Phase 3 config (`Config.AuthKey()`, `util.NormalizeBookOrbitURL`) and Phase 2 foundation (`internal/util/batch.go`, `internal/sync/state`). Everything Readest-side and the sync engine itself are out of scope except where a boundary must be named.

Authoritative behavior is taken from `reference/koreader-plugin/bookorbit.koplugin/bookorbit_api.lua` (the entire REST surface), cross-checked against `bookorbit_sweep.lua` (real call sites and real response-field usage), `bookorbit_book_sync.lua` (single-book match-check call site), `bookorbit_progress_sync.lua` (the singular-PUT fallback endpoint), and `bookorbit_state.lua` (the match-cache shape `internal/sync/state` already mirrors). Where the planning docs disagreed with the plugin source, the plugin won; those resolved discrepancies are recorded in §15.

---

## 1. Overall architecture and package responsibilities

`internal/bookorbit` owns exactly one thing: **turning domain-level sync facts (hashes + candidate metadata, progress items) into BookOrbit HTTP calls, and BookOrbit's JSON responses back into typed Go values or classified errors.**

This mirrors the boundary already drawn and accepted in Phase 4 (`docs/phase-4-design.md` §1): each layer owns one external system, nothing crosses a line except through the interface that line exists for.

| Concern | Owner | Crosses via |
|---|---|---|
| Static credentials (`x-auth-user`/`x-auth-key`) | `config.Config` (`AuthKey()`) | Constructor params |
| Server URL normalization | `internal/util.NormalizeBookOrbitURL` (applied in `config.finalize()`) | `cfg.BookOrbit.ServerURL` is pre-normalized by the time it reaches this package |
| BookOrbit transport + decode + classification | `bookorbit.Client` | `API` interface |
| Wire request/response shapes | `bookorbit` models (`models.go`) | value types |
| Match-cache, unmatched cooldown, per-book last-pushed percentage | `internal/sync/state.Store` | consumed by the engine, **not** by this package |
| Batching (500 hashes/match-check, 100 items/bulk-progress) | `internal/util.Batch`/`BatchFunc` + the sync engine (Phase 6) | the client sends exactly what it is given, once |
| Retry/backoff policy | `sync.Engine` (Phase 6), driven by `config.Bridge.Retry*` | the client returns classified sentinel errors only |

**What this package does NOT do**, matching the "client is faithful, engine is selective" principle already established for `internal/readest` (Phase 4 design §7):

- It does not decide which hashes need matching, or split a call into a series of batched calls — it sends the hashes/candidates/items slice it is given, in one HTTP round trip.
- It does not read or write `state.Store`. The engine reads `Match`/`UnmatchedAt` before calling, and writes `SetMatch`/`SetUnmatched`/`ClearUnmatched` after, using the response this client returns.
- It does not retry on transient failures. It classifies them into sentinel errors and returns immediately; the engine's backoff loop (Phase 6, not yet built) decides whether/when to call again.
- It does not decide whether the single-book `UpdateProgress` fallback should be used instead of `BulkProgress`. It only implements the endpoint faithfully; the *policy* of "bulk endpoint looks unsupported, switch to the fallback" is a Phase 6 concern (see §9).

---

## 2. Public API for `internal/bookorbit`

### 2.1 Interface

```go
type API interface {
    Auth(ctx context.Context) error
    MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error)
    BulkProgress(ctx context.Context, req BulkProgressRequest) (BulkProgressResponse, error)
    UpdateProgress(ctx context.Context, req UpdateProgressRequest) error
}
```

`BulkProgress` returns the decoded response, not just an error. The only field the reference plugin reads off a bulk-progress response is `body.unmatched` (`bookorbit_sweep.lua:stepProgressNext`, verified), and the engine cannot mark a book unmatched (`state.Store.SetUnmatched`) without it — the Phase 2 foundation's stub signature would have silently discarded that data.

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
    timeout  time.Duration
    now      func() time.Time // injectable clock for DeviceTime freshness
}

func NewClient(
    baseURL, username, authKey string,
    device DeviceInfo,
    hc httpclient.Doer,
    log *slog.Logger,
    maxBody int64,
    timeout time.Duration,
) *Client
```

`now` is not a constructor parameter — like Phase 3's `Auth.now`, it defaults to `time.Now` in `NewClient` and is only overridden directly on the struct in tests (same pattern as `auth_test.go`'s `fixedClock`).

---

## 3. Constructor dependencies and rationale

| Dep | Justification |
|---|---|
| `baseURL string` | Already-normalized BookOrbit API base (`{server}/api/v1`), from `config.BookOrbit.ServerURL` via `config.finalize()` → `util.NormalizeBookOrbitURL`. |
| `username string` | Sent verbatim as `x-auth-user`. From `config.BookOrbit.Username`. |
| `authKey string` | Sent verbatim as `x-auth-key`. From `config.AuthKey()` (MD5 of password, or pre-hashed userkey normalized to lowercase). No hashing happens inside this package; `config.AuthKey()` owns that. |
| `device DeviceInfo` | Static per-bridge identity (`DeviceID`, `DeviceModel`, `PluginVersion`) sent on every device-wrapped call. Verified: `bookorbit_api.lua:new()` stores these once at construction (`self.device_id`, `self.device_model`, `self.plugin_version`). |
| `hc httpclient.Doer` | Injected transport, same seam Phase 3/4 use for mockability. |
| `log *slog.Logger` | Structured lifecycle logging; nil → `slog.Default()`, matching `NewAuth`'s convention. |
| `maxBody int64` | Client-side request-body cap. Verified: `bookorbit_api.lua:requestBlocking` checks `#body_json > MAX_BODY_BYTES` (900×1024) **after encoding, before sending**, returning `"body_too_large"` without dispatching the request. From `config.Bridge.MaxBodyBytes` (defaulted to 900×1024, matching the plugin exactly). |
| `timeout time.Duration` | Honored as a per-call `context.WithTimeout`, mirroring Phase 4's `readest.Client`. The plugin does give BookOrbit calls their own timeout tier (`socketutil.LARGE_BLOCK_TIMEOUT`/`LARGE_TOTAL_TIMEOUT`, distinct from the file-transfer timeout tier) — verified in `bookorbit_api.lua:requestBlocking`, though the exact durations aren't in the provided reference source (see §16, live-validation item 7). A per-call deadline independent of the transport default matters more here than for Readest, since bulk-progress payloads can approach 900 KiB on slow self-hosted hardware. Defaults to `config.Bridge.HTTPTimeout` at the CLI wiring layer (`cmd/bridge/main.go`), same as `readest.NewClient`. |
| `now func() time.Time` | Not a constructor parameter; a settable field defaulting to `time.Now`, used only to stamp `deviceTime`/`device_time` fields fresh on every request (§7.4). Mirrors Phase 3's injectable-clock convention (`Auth.now`). |

**Not injected, deliberately:** `state.Store`. Unlike `readest.Auth`, this client has nothing to persist — no token, no rotation, no watermark. Match-cache ownership stays entirely with the engine, matching the boundary in §1.

---

## 4. Request/response models

### 4.1 Verified against the plugin, unchanged from the Phase 2 foundation

- `Match { Hash, BookFileID, BookID }` — verified against `bookorbit_sweep.lua:stepMatchNext` (`match.hash`, `match.bookFileId`, `match.bookId`).
- `MatchCheckResponse { Matches, Unmatched, LibraryVersion }` — verified against `stepMatchNext` and `bookorbit_book_sync.lua:stepMatch` (`body.matches`, `body.unmatched`, `body.libraryVersion`).
- `ProgressItem { Hash, Percentage, Progress, Timestamp }` — verified against `stepProgressNext`'s `progress_items` construction (`{hash, percentage, progress, timestamp}`).
- `DeviceInfo { DeviceID, DeviceModel, PluginVersion, DeviceTime }` and `DeviceTimeFormat` — verified against `withDevice`.
- `BulkProgressRequest { DeviceID, DeviceModel, PluginVersion, DeviceTime, Items }` and `DeviceInfo.WithDevice(items)` — verified against `bulkProgress(items)` → `self:withDevice({items = items})`.

### 4.2 Corrections applied during this phase (verified against the plugin source)

**(a) `MatchCandidate` gained `MetadataAmbiguous`.**
Verified: `bookorbit_api.lua:matchCheck` builds each candidate as `{hash, title, authors, lastOpen, source, metadataAmbiguous = cand.metadata_ambiguous}`. Both real call sites populate it — `bookorbit_sweep.lua`'s `ctx.candidates[md5].metadata_ambiguous` and `bookorbit_book_sync.lua`'s `ctx.snap.metadata_ambiguous`.

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

**(b) `MatchCheckRequest` gained the device wrapper.**
Verified: `matchCheck` dispatches via `self:request("POST", "/koreader/plugin/match-check", self:withDevice(payload))` — **every** match-check payload is device-wrapped, not just bulk-progress. Mirrors the `BulkProgressRequest` pattern exactly:

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

**(c) `BulkProgressResponse.Updated` replaced with the verified `Unmatched`.**
No line in any provided reference source read an `updated` field from a bulk-progress response; that field in the Phase 2 stub was speculative. The only field the plugin consumes is `body.unmatched` (`stepProgressNext`, verified):

```go
type BulkProgressResponse struct {
    Unmatched []string `json:"unmatched"`
}
```

**(d) New: `UpdateProgressRequest`** (fallback endpoint, §9), verified against `bookorbit_api.lua:updateProgress`:

```go
// The field naming deliberately does NOT match the plugin's own camelCase
// convention (deviceId, pluginVersion, ...) used everywhere else in this
// package. That's not a bridge inconsistency — /koreader/syncs/progress
// is the kosync-compatible endpoint shared with vanilla KOReader's stock sync
// plugin, not a BookOrbit-native /koreader/plugin/* endpoint, and it uses
// kosync's own wire shape. Verified: bookorbit_api.lua:updateProgress.
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

No response model exists for `Auth` or `UpdateProgress`: the plugin's own call sites only check success/failure and never read a field back from either body (`bookorbit_main_menu.lua:testConnection` checks `body ~= nil`; `bookorbit_progress_sync.lua` treats the PUT as success/failure only). `Auth(ctx) error` and `UpdateProgress(ctx, req) error` reflect that.

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
`hashes` and `books` serialize as `[]`, never `null`, even when empty — see §7.5.

Response:
```json
{
  "matches": [ { "hash": "h1", "bookFileId": 42, "bookId": 7 } ],
  "unmatched": ["h2"],
  "libraryVersion": "opaque-token"
}
```
`matches`/`unmatched` may be absent or `null` on the wire; the plugin treats both as `{}` (`body.matches or {}`), so the client normalizes to non-nil empty slices on decode (mirrors `readest.decodeBooks`'s `Books == nil` normalization, an established pattern in this codebase).

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

No request body. Response body shape is unmodeled (§16.2) — success is simply "the request returned a 2xx status," matching `testConnection`'s `if body then ...`.

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
- `Content-Length` — the plugin sets this explicitly (`request.headers["Content-Length"] = #body_json`); in Go, `net/http` computes `Content-Length` automatically when the request body is a `*bytes.Reader` (which implements `Len()`), so no manual header is set. Confirmed via `TestMatchCheckHeaders`, which asserts `req.ContentLength` matches the encoded body length.

**Authentication:** static, no expiry, no refresh, no OAuth, no signing beyond the two fixed headers above. This is a hard architectural contrast with `readest.Auth`: there is no token-refresh code path in this package, matching the reference plugin (`bookorbit_api.lua` sends `x-auth-user`/`x-auth-key` unconditionally on `NewClient`, with no rotation logic anywhere in the file).

**On a 401/403:** unlike the Readest client, there is **no** re-auth-and-retry dance, because there is no second credential state to refresh. A 401/403 means the configured username/authKey are wrong (or the server rejected them for another reason) and is terminal for that call.

---

## 7. `MatchCheck` behavior, batching, caching, response handling

### 7.1 What the client does

One HTTP round trip: given a `MatchCheckRequest` (hashes + candidates), POST it, decode the response into `MatchCheckResponse`, normalize `Matches`/`Unmatched` to non-nil, return.

### 7.2 Batching — NOT the client's job

Verified: `MATCH_BATCH = 500` in `bookorbit_sweep.lua`. The chunking loop (`ctx.match_queue`, built with `for i = 1, #to_check, MATCH_BATCH`) lives in the sweep orchestration, not in `bookorbit_api.lua:matchCheck` itself — `matchCheck` always sends exactly the hashes it's handed. **The engine (Phase 6) chunks using `internal/util.Batch`/`BatchFunc` against `config.Bridge.MatchBatchSize`, calling `Client.MatchCheck` once per chunk.** No batching logic exists in this package.

### 7.3 Caching — NOT the client's job

The match cache (hash → `bookFileId`/`bookId`) and the unmatched cooldown (`config.Bridge.UnmatchedCooldown`) are entirely `state.Store`'s responsibility (Phase 2: `Match`/`SetMatch`/`DeleteMatch`, `UnmatchedAt`/`SetUnmatched`/`ClearUnmatched`). This client is stateless between calls — it holds no cache.

### 7.4 Response handling and the device-time freshness problem

`withDevice` in Lua calls `os.date(...)` fresh, immediately before every dispatch (`bookorbit_api.lua:withDevice`, called from inside `matchCheck`/`bulkProgress` themselves, not by the caller). If the Go engine builds a batch of `MatchCheckRequest` values up front (e.g., while iterating `ctx.match_queue`) and only *later* calls `Client.MatchCheck` on each, a caller-stamped `DeviceTime` could go stale relative to when the request actually goes out — a divergence from the plugin's exact behavior.

**Implemented:** `Client.MatchCheck`/`BulkProgress`/`UpdateProgress` overwrite `DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime` on the request they receive, using the client's own static `device` field and `c.now()`, immediately before marshaling — regardless of what the caller populated. This:
- Exactly reproduces the plugin's "stamp at dispatch time" behavior.
- Removes any need for the engine to worry about staleness across a batch loop.
- Makes `DeviceInfo.WithMatchCheck`/`WithDevice`/`WithUpdateProgress` convenience constructors rather than the only correct way to build a request — a caller can pass a zero-value device wrapper and the client will still stamp it correctly. `TestMatchCheckDeviceFieldsStampedAtTopLevel` and `TestUpdateProgressOverridesCallerDeviceFields` pin this by asserting that deliberately stale/garbage caller-supplied device values are overwritten on the wire.

### 7.5 Empty-array encoding

Verified: the plugin's `rapidjson.encode(body, {empty_table_as_array = true})` exists specifically because "the backend rejects empty `{}` where it expects arrays" (`reference-map.md`, Key Behavioral Constants table). Go's `encoding/json` already encodes a **non-nil, empty** slice as `[]` — the only failure mode is a **nil** slice encoding as `null`. The client normalizes `Hashes`/`Books`/`Items` to non-nil empty slices (`make([]T, 0)`) before marshaling when the caller passed `nil`, confirmed by `TestMatchCheckEmptySlicesEncodeAsArrayNotNull` and `TestBulkProgressRequestShapeDeviceWrapped`.

---

## 8. `BulkProgress` behavior, request construction, response handling, limits

### 8.1 What the client does

One HTTP round trip: given a `BulkProgressRequest`, POST it, decode into `BulkProgressResponse{Unmatched}`, normalize `Unmatched` to non-nil, return `(BulkProgressResponse, error)`.

### 8.2 Batching

Verified: `PROGRESS_BATCH = 100` in `bookorbit_sweep.lua`. Same split as §7.2 — the engine chunks via `util.BatchFunc` against `config.Bridge.ProgressBatchSize`, calling `Client.BulkProgress` once per chunk of ≤100 items. Not this package's job.

### 8.3 Limits

`config.Bridge.MaxBodyBytes` (900×1024, verified against `MAX_BODY_BYTES` in `bookorbit_api.lua`) is enforced by the client **after** JSON-encoding the fully device-stamped request and **before** dispatching: if `len(encoded) > maxBody`, `ErrBodyTooLarge` is returned and no HTTP call is made. This mirrors the plugin's exact ordering (`request(...)`: encode → check length → only then build the socket request). `TestMatchCheckBodyTooLargeMakesNoRequest` and `TestBulkProgressBodyTooLargeMakesNoRequest` assert zero requests are dispatched.

### 8.4 Response handling

`unmatched` is the only field consumed anywhere in the reference source (§4.2c). A hash appearing in `unmatched` means the engine should call `state.Store.SetUnmatched(hash, now)` and mark that book's push as failed for this pass (`pending.failed = true` in `stepProgressNext` — a Phase 6 concern, not this client's).

---

## 9. `UpdateProgress` fallback

The endpoint is implemented in this phase, per two independent justifications:
1. `docs/implementation-roadmap.md`'s own Phase 5 scope explicitly lists it: *"Fallback UpdateProgress(...) single-item PUT (optional but recommended for version fallback)."*
2. `docs/planning.md` §6 recommendation 1 and `docs/reverse-engineering-report.md` §6 recommendation 1 both independently conclude bulk-progress is the primary path and the singular PUT is a version-compatibility fallback, not a routine code path.

**When it should be used:** only when `BulkProgress` is confirmed unsupported by the target server (e.g., an older BookOrbit build without `/koreader/plugin/progress`). **This phase does not decide the trigger condition or implement the fallback policy** — that is Phase 6's job, once the engine exists to hold "is bulk unsupported" state. The closest verified analog in the reference source is `bookorbit_sweep.lua`'s legacy annotation-upload fallback (`ctx.use_legacy_annotations`, triggered on `err == "unsupported_server"` from the exchange endpoint), but that is a **different endpoint pair** (annotations exchange vs. legacy upload) — there is no direct reference precedent for a bulk-progress → singular-PUT fallback trigger. Extrapolating that pattern to the progress endpoints was a deliberate bridge-specific design decision (§15.7), not a verified plugin behavior, and remains deferred to Phase 6.

**Why the fallback *policy* isn't implemented here too:** the trigger condition depends on `state.Store` (to remember "this server doesn't support bulk, stop trying") and on retry/backoff sequencing, both explicitly Phase 6 concerns per the roadmap. This phase's job was only to make the endpoint callable — done.

---

## 10. Error taxonomy and retry classification

Following the Phase 4 pattern (distinct, `errors.Is`-matchable sentinels, each wrapped via `%w` with status + a bounded body snippet):

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

`ErrUnsupportedEndpoint` (404/405) is distinct from `ErrBadRequest` (400) specifically so Phase 6 can key its fallback-trigger logic off `errors.Is(err, bookorbit.ErrUnsupportedEndpoint)` without conflating it with a generic client bug. This mirrors how the BookOrbit KOReader plugin's own catalog code gives "404/405 means feature unsupported" its own check (`isDashboardUnsupported`, `bookorbit_catalog.lua`) — the closest verified precedent for this pattern in the codebase, even though it comes from a different (catalog) endpoint. The extrapolation to the progress endpoints (§15.7) has no direct reference precedent and is a deliberate bridge-specific decision, not verified plugin behavior.

One classifier (`classifyErrorResponse`) is applied uniformly across every endpoint in the package:

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

`ErrBodyTooLarge` is a **bridge-specific improvement** over the reference plugin's own error model: `bookorbit_api.lua` returns a bare string `"body_too_large"` which its own caller-side `isTransportError(err) = type(err) ~= "number"` classifies as transport-retryable — almost certainly a latent bug in the Lua reference (a request that's too large stays too large on retry). The Go client does not copy this; it gets its own precise, non-retryable sentinel. This is a deliberate, documented deviation, not an oversight.

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

This package exposes exactly what steps 1–8 need and nothing else — no method here performs steps 1, 2, 3, 5, 6, 7, or 8.

---

## 12. Concurrency

**No synchronization was needed**, for an even simpler reason than Phase 4's `readest.Client`: this client has no shared mutable resource at all. `Client`'s fields (`baseURL`, `username`, `authKey`, `device`, `http`, `log`, `maxBody`, `timeout`, `now`) are all immutable after construction (the `now` field is a function value, not shared mutable state). There is no token to refresh, no mutex-guarded critical section anywhere in this package. `Client` is safe for concurrent use by construction, not by discipline — confirmed by `TestConcurrentCallsNoRace` under `-race`.

In practice, per `docs/implementation-brief.md` ("Polling daemon... No bidirectional sync"), the engine calls this client serially within one `RunOnce` pass, so concurrency safety is a free property rather than a load-bearing requirement — matching the identical conclusion reached for the Readest client in Phase 4 (§8).

---

## 13. Timeouts, body-size limits, and resource ownership

- **Per-call deadline:** `Client` applies `context.WithTimeout(ctx, c.timeout)` at the top of each exported method when `c.timeout > 0`, exactly as `readest.Client.PullBooks` does. The default value comes from `config.Bridge.HTTPTimeout` (30s), threaded through at the CLI wiring layer (`cmd/bridge/main.go`, where `boClient := bookorbit.NewClient(..., cfg.Bridge.HTTPTimeout)`).
- **Body-size cap:** `c.maxBody`, enforced client-side after encoding, before dispatch (§8.3), sourced from `config.Bridge.MaxBodyBytes` (900×1024).
- **Error-body read cap:** a package-local `maxErrorBodyBytes = 512` constant, matching Phase 3/4's bound on how much of a non-2xx body is read into a wrapped error message.
- **Response-body read cap:** unlike `readest.Client.decodeBooks` (4 MiB, because a full library pull can be large), BookOrbit's responses here (`match-check`, `bulk-progress`, `auth`, singular PUT) are all small, bounded acknowledgments — a `maxResponseBodyBytes = 256 KiB` cap is generous headroom while still preventing a misbehaving server from flooding memory.
- **Ownership:** the `http.Client`/`Doer` itself (its own transport-level timeout, connection pooling, TLS config) is owned by whoever constructs the `httpclient.Doer` passed into `NewClient` — i.e., the CLI wiring layer, not this package. This package only owns the *per-call* deadline layered on top, same split as Phase 4.

---

## 14. Unit-test coverage

Mirrors the Phase 4 harness style: a scriptable `stubDoer` (the same reusable pattern established in `internal/readest`'s `auth_test.go`/`client_test.go`), no real network, an injectable clock for `DeviceTime` assertions. The full suite lives in `internal/bookorbit/client_test.go` (client behavior) and `internal/bookorbit/models_test.go` (wire-shape assertions), and includes:

- **`Auth()`** — success; 401/403 → `ErrUnauthorized`; network error → `ErrNetwork`; required headers (`x-auth-user`, `x-auth-key`, `accept`, no `Content-Type` on a bodyless GET); method/path.
- **`MatchCheck()`** — success decoding `matches`/`unmatched`/`libraryVersion`; `null`/absent fields normalizing to non-nil empty slices; `hashes`/`books` always encoding as `[]`, never `null`; device fields present at the top level and always overwritten with the client's own identity and current time even when the caller supplies stale values; `MetadataAmbiguous` passing through; headers including a `Content-Length` that matches the encoded body; the full status-code classification matrix (400/401/403/404/405/429/500/502/503); malformed JSON; body-too-large making zero HTTP calls; context cancellation propagating untouched; a single-hash call behaving identically to a multi-hash call.
- **`BulkProgress()`** — success returning `(BulkProgressResponse{Unmatched}, nil)`; device-wrapped request shape with a non-null `items` array; `unmatched` absent/`null` normalizing to empty; the same error-classification matrix; body-too-large making zero calls; an empty `items` slice sent faithfully (the client does not special-case it — avoiding that call with nothing to push is the engine's job).
- **`UpdateProgress()`** — success; the kosync snake_case-ish request shape asserted to differ from the camelCase device-wrapper shape used elsewhere, and to carry no `deviceId`/`pluginVersion`/`deviceTime` fields; `device`/`device_id` overwritten from the client's static identity even when the caller supplies different values; `timestamp` passed through unmodified (domain data, not a freshness field); 401/403/404/405/network classification.
- **Cross-cutting** — `NewClient` with a `nil` logger defaulting to `slog.Default()`; `timeout <= 0` disabling the per-call deadline; a configured timeout being honored against a slow response; error messages bounding the echoed body to `maxErrorBodyBytes`; concurrent calls to `MatchCheck` under `-race` confirming no shared mutable state; base-URL joining appending the endpoint path to `baseURL` as-is (the client does not re-run `util.NormalizeBookOrbitURL` — that already happened in `config.finalize()`), matching `readest.Client`'s equivalent assumption.

---

## 15. Documentation discrepancies, ambiguities, and assumptions resolved during implementation

1. **(Verified plugin source)** `BulkProgress` previously returned only `error`, discarding the response. The plugin's own sweep code reads `body.unmatched` off exactly this call. The interface now returns `(BulkProgressResponse, error)`.
2. **(Verified plugin source)** `MatchCandidate` was missing `MetadataAmbiguous`, populated by both real call sites. Added.
3. **(Verified plugin source)** `MatchCheckRequest` was missing the device wrapper (`deviceId`/`deviceModel`/`pluginVersion`/`deviceTime`) that `withDevice` applies to every match-check payload, not just bulk-progress. Added.
4. **(Verified plugin source)** `BulkProgressResponse.Updated` was speculative and unconfirmed by any provided source; no plugin code reads such a field from a bulk-progress response. Replaced with the verified `Unmatched []string`.
5. **(Implemented, bridge-specific design decision)** The client overwrites `DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime` on every request immediately before encoding, rather than trusting whatever the caller populated via the `WithDevice`/`WithMatchCheck`/`WithUpdateProgress` helpers. This faithfully reproduces the plugin's "stamp at dispatch time" behavior and eliminates staleness across a batched engine loop; pinned by `TestMatchCheckDeviceFieldsStampedAtTopLevel` and `TestUpdateProgressOverridesCallerDeviceFields`.
6. **(Implemented, bridge-specific design decision)** `ErrBodyTooLarge` is a distinct, non-retryable sentinel, deliberately diverging from the plugin's own crude "any string error is transport-like, therefore retryable" classification (`isTransportError`), which appears to be an unintentional latent bug in the Lua reference rather than a deliberate choice.
7. **(Implemented, bridge-specific design decision, extrapolated)** `ErrUnsupportedEndpoint` (404/405) as the trigger signal for a future bulk→singular-PUT fallback is extrapolated from the *annotations* exchange endpoint's `unsupported_server` fallback pattern in `bookorbit_sweep.lua` — no reference source shows this exact fallback for the progress endpoints specifically. Recorded here so it is not mistaken for verified behavior; the fallback *policy* itself is still Phase 6's to design.
8. **(Verified implementation)** Go's `net/http` auto-computes `Content-Length` for `*bytes.Reader` request bodies, satisfying the plugin's explicit header-setting requirement without manual code. Confirmed by `TestMatchCheckHeaders`.
9. **(Verified implementation)** Go's `encoding/json` encodes non-nil empty slices as `[]`; the client normalizes `Hashes`/`Books`/`Items` before marshaling so a caller-supplied `nil` never reaches the wire as `null`. Confirmed by `TestMatchCheckEmptySlicesEncodeAsArrayNotNull` and the bulk-progress equivalent.
10. **(Implementation detail, not architecturally significant)** The response-body read cap for `match-check`/`bulk-progress`/`auth`/`update-progress` is `maxResponseBodyBytes = 256 KiB`, a package-local constant — these are small acknowledgment bodies, not full library pages.

---

## 16. Live-validation items (cannot be determined from source; unaffected by this implementation)

1. Whether a real `bulk-progress` response ever carries fields beyond `unmatched` (would justify restoring some form of an `updated`/count field with actual evidence).
2. Whether `GET /koreader/users/auth`'s response body carries any usable fields, or is genuinely just `{}`/whatever-truthy on success.
3. **The exact status code(s) an older BookOrbit server returns when `/koreader/plugin/progress` doesn't exist** — 404, 405, or something else — which determines whether `ErrUnsupportedEndpoint`'s classification (404/405) is correctly scoped for Phase 6's eventual fallback trigger.
4. Whether BookOrbit's self-hosted servers ever return 429 in practice (handled defensively regardless, per the same posture Phase 4 took for Readest).
5. Real-world round-trip timing for a ~900 KiB bulk-progress upload against typical self-hosted NAS hardware, to validate whether the default 30s `HTTPTimeout` is sufficient or whether operators will need to raise it in `bridge.yaml`.
6. Whether `device_id`/`device` on the singular PUT endpoint have any server-side validation (e.g., rejecting an empty string) that could turn an edge case into a 400.
7. The exact numeric values of `socketutil.LARGE_BLOCK_TIMEOUT`/`LARGE_TOTAL_TIMEOUT` (module not present in the provided source), which would confirm whether BookOrbit's own KOReader plugin gives itself materially more headroom than the bridge's default 30s.

None of these affect the client's correctness as implemented; they inform tuning and Phase 6's fallback-trigger design.

---

## 17. Package layout (as implemented)

```
internal/bookorbit/
  doc.go            (package doc)
  client.go          (Client, NewClient, all four methods, sentinel errors, helpers)
  client_test.go     (full unit-test matrix; stubDoer pattern reused from internal/readest)
  models.go          (MatchCandidate.MetadataAmbiguous, MatchCheckRequest device fields +
                       WithMatchCheck, BulkProgressResponse.Unmatched,
                       UpdateProgressRequest + WithUpdateProgress)
  models_test.go     (asserts MetadataAmbiguous round-trips, MatchCheckRequest device
                       fields present, BulkProgressResponse decoding, UpdateProgressRequest
                       wire shape)
```

No subpackage — `internal/bookorbit` stays flat, matching the precedent set by keeping `internal/readest` flat through Phases 3–4 (the types are only consumed by this package and the engine, and a split would be churn without benefit).

---

## 18. Files modified or created

**Modified:**
- `internal/bookorbit/models.go` — §4.2 additions/corrections.
- `internal/bookorbit/client.go` — implemented all four methods; sentinel error block; `timeout`/`now` fields; the Phase 2 foundation's `ErrNotImplemented` was removed once no method returned it (`internal/sync/engine.go` has its own distinct `sync.ErrNotImplemented`, unaffected).
- `internal/bookorbit/models_test.go` — removed `TestClientStubReturnsNotImplemented`; extended coverage for the new device fields and `MetadataAmbiguous`.
- `cmd/bridge/main.go` — the `bookorbit.NewClient(...)` call site was updated with the new trailing `timeout` argument, sourced from `cfg.Bridge.HTTPTimeout`, matching how `readest.NewClient` is already wired.

**Created:**
- `internal/bookorbit/client_test.go` — the full unit-test matrix (§14).

---

## 19. Public API — net changes from the Phase 2 foundation

- `API.BulkProgress` signature changed from `(ctx, req) error` to `(ctx, req) (BulkProgressResponse, error)` — breaking, but nothing in the codebase depended on the old signature besides the stub itself and its own test.
- `API` gained `UpdateProgress(ctx, req UpdateProgressRequest) error` — additive.
- `MatchCandidate` gained `MetadataAmbiguous bool` — additive.
- `MatchCheckRequest` gained `DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime` — additive.
- `BulkProgressResponse.Updated` was removed and replaced by `Unmatched []string` — breaking, but nothing depended on the old field.
- New exported sentinel errors: `ErrUnauthorized`, `ErrBadRequest`, `ErrUnsupportedEndpoint`, `ErrRateLimited`, `ErrServer`, `ErrNetwork`, `ErrMalformedResponse`, `ErrBodyTooLarge`.
- `ErrNotImplemented` was removed from `internal/bookorbit` (no method returns it anymore).
- `NewClient` gained a `timeout time.Duration` parameter, wired through from `cmd/bridge/main.go`.

---

## 20. Status

Implemented and accepted. All decisions previously flagged for approval in this design were resolved as follows and are now load-bearing in the shipped code, not open questions:

- `BulkProgress` returns `(BulkProgressResponse, error)`.
- The §4.2(a–c) model corrections are in place.
- `UpdateProgress` is implemented now; the fallback *policy* (when to use it instead of `BulkProgress`) is deferred to Phase 6, per the roadmap.
- `NewClient` takes a `timeout time.Duration`, honored as a per-call deadline.
- The client overwrites the device-wrapper fields on every request at send time; `WithDevice`/`WithMatchCheck`/`WithUpdateProgress` are convenience constructors only.
- `ErrUnsupportedEndpoint` (404/405) exists as its own sentinel for Phase 6's future fallback-trigger use.
- `ErrBodyTooLarge` is classified as strictly non-retryable, a deliberate divergence from the plugin's own (likely buggy) treatment of `"body_too_large"` as retryable.

Phase 6 (the sync engine) is next.
