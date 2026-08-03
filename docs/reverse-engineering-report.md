# Readest → BookOrbit Standalone Bridge: Reverse-Engineering Report

**Scope:** One-way, Readest-as-source-of-truth, progress-only sync. No KOReader runtime, no annotations/highlights/status, no UI.

---

## 1. Overall Synchronization Flow

The existing KOReader-mediated flow works because **Readest’s `partialMD5` and KOReader’s `util.partialMD5` produce the same digest**. Both plugins key everything off that digest, so KOReader literally passes a hash from Readest to BookOrbit without ever comparing book metadata. The standalone bridge replaces only that relay.

### Current KOReader-mediated flow

```
Readest client
  │ writes book row: {book_hash, meta_hash, progress:[cur,total], updated_at}
  ▼
Readest Sync cloud
  │
  ├─ (A) readest.koplugin pulls per open book
  │     GET /sync?since=0&type=configs&book=<hash>&meta_hash=<hash>
  │     Auth: Bearer <supabase_access_token>
  │     applies config ( GotoPage / GotoXPointer )
  │
  ▼
KOReader in-memory reading position
  │
  ├─ (B) bookorbit.koplugin pushes
  │     digest = util.partialMD5(file)
  │     PUT /koreader/syncs/progress
  │       { document, percentage, progress, device, device_id, timestamp }
  │     Auth: x-auth-user / x-auth-key
  ▼
BookOrbit self-hosted server
```

### Proposed standalone bridge flow

```
poll timer
  │
  ├─ 1. Ensure Supabase access token fresh (refresh when < 50 % TTL)
  │
  ├─ 2. GET https://web.readest.com/api/sync?type=books&since=<cursor>
  │     ⇐ { books: [{ book_hash, meta_hash, title, author, progress, updated_at, deleted_at, synced_at, ... }] }
  │
  ├─ 3. For each book newer than the local watermark:
  │       percentage = progress[0] / progress[1]
  │
  ├─ 4. (first time per hash) POST BookOrbit /koreader/plugin/match-check
  │       { hashes: [book_hash], books: [{ hash, title, authors, lastOpen, source }] }
  │       ⇐ { matches: [{ hash, bookFileId, bookId }], unmatched: [...] }
  │       cache hash → bookFileId / bookId
  │
  ├─ 5. POST BookOrbit /koreader/plugin/progress (bulk)
  │       { deviceId, deviceModel, pluginVersion, deviceTime, items: [{ hash, percentage, progress, timestamp }] }
  │       Auth: x-auth-user / x-auth-key
  │
  └─ 6. Advance watermark to max(synced_at | updated_at | deleted_at)
```

Readest is the sole source of truth; BookOrbit is write-only. No conflict resolution is required.

---

## 2. Relevant Source Files

### Readest plugin (`reference/readest.koplugin/`)

These files define the API contract, authentication timing, and data shape we must replicate.

| File | Why it matters |
|------|----------------|
| `main.lua` | Hardcoded defaults: Supabase URL (`https://readest.supabase.co`), the base-64 anon key, sync API base (`https://web.readest.com/api`). Shows which fields are persisted (`access_token`, `refresh_token`, `expires_at`, `expires_in`, `user_id`). Identifies heavy UI/library paths that are out of scope. |
| `readest_syncauth.lua` | **The authoritative token-freshness / refresh logic.** `needsLogin`, `tryRefreshToken`, and `withFreshToken` all implement the same threshold: refresh when `expires_at < now + expires_in / 2`. `withFreshToken` blocks the API call until refresh completes, fixing the race in `ensureClient`. |
| `readest_supabaseauth.lua` | Supabase HTTP transport. Confirms request shape for password grant, refresh, sign-out, and user endpoint. Shows that `apikey` header is required on every request and must equal the decoded anon key. |
| `readest_syncclient.lua` | Readest Sync API methods we need: `pullBooks({ since })` → `GET /sync?type=books`. Also lists storage/download endpoints that are irrelevant for progress-only sync. Confirms `authorization: Bearer <token>` and JSON content type. |
| `readest-sync-api.json` | Literal Spore spec: `pullChanges` (per-book, out of scope), `pushChanges` (out of scope), `pullBooks` (in scope). Expected status codes. |
| `supabase-auth-api.json` | Literal Supabase spec: `sign_in_password`, `refresh_token`, `sign_out`, `get_user`. Paths and parameters. |
| `library/librarystore.lua` | Contains `parseSyncRow` (the canonical row parser) and `iso_to_ms`. Tells us exactly which fields arrive on the `/sync?type=books` wire, which are ISO timestamps, how `synced_at` is handled, and that the dummy `0000…0` hash must be filtered. Also documents the dual-watermark pattern (pull cursor vs push watermark). |
| `library/syncbooks.lua` | `pullBooks` implementation: calls `client:pullBooks`, parses rows, advances watermarks. Confirms the pull cursor is `max(synced_at, updated_at, deleted_at)` and that an empty response leaves the cursor unchanged. Out-of-scope parts: upload/download/storage helpers. |
| `readest_syncconfig.lua` | Shows the `book_configs` row shape pushed by Readest clients. For the bridge we only need the `books.progress` tuple returned by the library pull; this file explains why `configs` is a separate, per-book path (exact resume position/xpointer) that we deliberately avoid. |
| `docs/library-design.md` | Design-level confirmation that partial-MD5 parity is assumed proven, that `/sync` returns snake_case DB rows, and that `synced_at` is the server-authoritative pull cursor. Also catalogues KOReader-specific UI/storage paths that are irrelevant. |

### BookOrbit plugin (`reference/koreader-plugin/bookorbit.koplugin/`)

| File | Why it matters |
|------|----------------|
| `bookorbit_api.lua` | **The entire REST surface.** Auth headers, server URL normalization, request/response timeouts, JSON encoding quirks, and every endpoint. The bridge needs `auth`, `matchCheck`, and `bulkProgress` (or `updateProgress`). Defines request body shapes precisely and the 900 KiB body limit. |
| `bookorbit_state.lua` | Persistent match-state pattern: `books[md5] = { fileId, bookId, ... }`, `unmatched[md5] = last_check_ts`, plus watermark fields. This is the pattern to port for the bridge’s local state store. |
| `bookorbit_progress_sync.lua` | Live progress push/pull. Shows that percentage is 0–1, that progress string is empty for audio/web-derived pushes (fallback to percentage), and that `timestamp = os.time()` (seconds). Confirms conflict paths we will not implement. |
| `bookorbit_book_sync.lua` | `BookOrbitBookSync.capture()` shows how a progress item is built from a live reader for the single-book `updateProgress` path; `stepProgress` calls `client:updateProgress(digest, pct, progress, ts)`. |
| `bookorbit_sweep.lua` | Full-library sweep. Contains the **bulk-progress** path: how `progress_items` are assembled and how `bulkProgress(batch)` is called. Batching sizes: `MATCH_BATCH = 500`, `PROGRESS_BATCH = 100`. Also shows how match-check responses (`matches`, `unmatched`) are handled and how the `libraryVersion` token is used. |
| `main.lua` | Plugin lifecycle, settings schema, `device_id` source, and how `userkey` is stored. Confirms the bridge needs a stable `device_id` and that `x-auth-key` is already an MD5 digest in settings. |

### Explicitly out of scope

- Readest: all UI/library code (`library/librarywidget.lua`, `libraryitem.lua`, `librarypaint.lua`, `libraryviewmenu.lua`, `localscanner.lua`, `coverprovider.lua`, `cloud_covers.lua`, etc.), annotations/stats/self-update modules, i18n assets.
- BookOrbit: annotations, catalog, sidecar, statistics reader, highlight summary, scheduler, sync coordinator/job runner, updater, menu pin, sweep phases other than match + bulk progress, live pull/conflict UI.

---

## 3. Discrepancies Between Planning Documents and Plugin Source

The plugin source is authoritative for behavior. The following items in `planning.md` differ from the reference code or need correction.

| # | Planning doc claim | Plugin reality / correction |
|---|---|---|
| 1 | **match-check body** (`§5.3`): `{ hashes: [hash], books: { hash: {title, authors, source} } }` — a keyed object. | The plugin sends `books` as an **array** of objects: `[{ hash, title, authors, lastOpen, source, metadataAmbiguous }]`. `bookorbit_api.lua:matchCheck` iterates over `hashes` and builds `payload.books` as an array. The bridge must match this shape. |
| 2 | **bulk progress body** (`§5.3`): implies `{ items: [...] }` only. | `BookOrbitApi:bulkProgress` calls `self:withDevice({ items = items })`. The real body is `{ deviceId, deviceModel, pluginVersion, deviceTime, items: [...] }`. |
| 3 | **BookOrbit `x-auth-key`** (`§5.3`): described as `md5(password)`. | True, but the plugin **stores** the MD5 in settings as `userkey` and sends it verbatim. The bridge config can accept a password and hash it, or accept a pre-hashed key. The header value must be lowercase hex MD5. |
| 4 | **PUT `/koreader/syncs/progress` fields** (`§5.3`): `{ document, percentage, progress, device, device_id, timestamp }`. | Correct. Note `percentage` is 0–1 float, `progress` is an opaque string (page number or xpointer), `timestamp` is **seconds** (`os.time()`). |
| 5 | **Progress tuple semantics** (`§8 issue 2`): units “unconfirmed (pages? locations?)”. | The plugin (and Readest library view) treat it simply as `cur / total` for display. For the bridge, that same ratio is the correct percentage to send. Exact resume position (xpointer) belongs to the per-book `configs` path, which is out of scope. |
| 6 | **Readest Sync base URL** (`§5.2`): “presumably configurable for self-hosted Readest deployments — verify.” | The plugin hardcodes `https://web.readest.com/api` in `readest-sync-api.json` and uses it without configuration. There is no evidence of a user-configurable sync base URL in the source. The bridge should expose it as config for future-proofing, but the default is fixed. |
| 7 | **Refresh token threshold** (`§4`, `§7`): “mirrors `withFreshToken` threshold at < 50% TTL.” | Confirmed exact in `readest_syncauth.lua`. However, `needsLogin` also treats a token that expires within 60 seconds as expired. The bridge should follow the 50 % rule for proactive refresh and the “expires within 60 s” rule before any request. |
| 8 | **Readest `/sync?type=books` watermark** (`§4`): “max(updated_at)” only. | Plugin uses `max(synced_at, updated_at, deleted_at)`; `synced_at` wins when present. Using only `updated_at` would miss server-side changes that carry an older client timestamp. |
| 9 | **Supabase auth response** (`§5.1`): lists `expires_at`. | The plugin reads `response.expires_at` directly. That field is indeed present in Supabase GoTrue `/token` responses, but relying on it means the bridge must handle a missing `expires_at` defensively or derive from `expires_in`. |
| 10 | **BookOrbit progress timestamp**: planning doc uses Readest `updated_at` unconverted. | Readest sends ISO/unix-ms; BookOrbit plugin sends `os.time()` seconds. The bridge must convert to seconds, not send milliseconds. |
| 11 | **Device identity** (`§4` draft): `device: "readest-bridge"`. | BookOrbit plugin sends `Device.model` and a persistent `device_id` from KOReader settings. The server likely uses these for conflict display and duplicate filtering. The bridge should generate a stable `device_id` (per config/host) and a sensible `device` string. |
| 12 | **Retry strategy** (`§7`): exponential backoff described generically. | The KOReader plugins have almost no retry logic of their own; they rely on UI/network rerun helpers. The bridge’s retry design is therefore new behavior we must define, not copy. |

---

## 4. Additional Implementation Details Discovered in Source

### 4.1 Readest side

1. **Supabase anon key.** It is embedded in `main.lua` as the base-64 constant `SUPABAE_ANON_KEY_BASE64`. The bridge can include the same constant or its decoded value; it is public and only identifies the Supabase project.
2. **Auth headers.** Every Supabase request needs `apikey: <anon_key>` *and* (after login) `authorization: Bearer <access_token>`.
3. **Token refresh returns a new refresh token.** The plugin overwrites the stored refresh token on every refresh. The bridge must persist the new refresh token atomically.
4. **`/sync?type=books` response row shape.** Fields seen and parsed by `parseSyncRow`:
   - `book_hash` / `hash`
   - `meta_hash`
   - `title`, `source_title`, `author`
   - `format` (uppercase, e.g. `EPUB`)
   - `metadata` → JSON string or table; `series` and `seriesIndex` extracted
   - `group_id`, `group_name`
   - `uploaded_at`, `updated_at`, `deleted_at`, `created_at` as ISO-8601 strings (with or without fractional seconds)
   - `synced_at` as ISO-8601 string (server-authoritative pull cursor)
   - `progress` as JSON tuple `[cur, total]`
   - `reading_status`, `reading_status_updated_at`
5. **Dummy hash filtering.** `00000000000000000000000000000000` is sent by the server on the initial `since=0` pull and must be ignored.
6. **Deleted books.** `deleted_at` set means the book should be ignored by the bridge; not pushed, and any cached match should be treated as stale.
7. **Progress-only source.** `progress` lives directly on the `books` row. The separate `book_configs` endpoint is only needed for exact xpointer/page resume, which is out of scope.

### 4.2 BookOrbit side

1. **Server URL normalization.** `BookOrbitApi.normalizeServerUrl` strips trailing slashes, collapses `/api/v1/koreader` to `/api/v1`, and appends `/api/v1` if missing. The bridge should replicate this.
2. **Auth endpoint.** `GET /koreader/users/auth` validates credentials. Useful as a health/check call on startup.
3. **Body encoding quirks.** `rapidjson` is used with `empty_table_as_array = true`; the backend rejects empty `{}` where it expects arrays. The bridge must not send `null` for arrays.
4. **Body size limit.** `MAX_BODY_BYTES = 900 * 1024`; this caps batch size. The plugin limits `PROGRESS_BATCH = 100`; the bridge can start with the same cap.
5. **Timeouts.** The plugin uses `socketutil.LARGE_BLOCK_TIMEOUT` / `LARGE_TOTAL_TIMEOUT` for BookOrbit requests. The bridge can map these to sensible defaults (e.g., 30 s connect, 120 s total for bulk uploads).
6. **Match-check batch size.** `MATCH_BATCH = 500` books per request in the sweep.
7. **match-check response shape.** `{ matches: [{ hash, bookFileId, bookId }], unmatched: [hash], libraryVersion: string? }`. Unmatched hashes should be cached and rechecked only after new activity.
8. **bulkProgress item shape.** From the sweep code:
   ```lua
   {
       hash = md5,
       percentage = extract.percent_finished,  -- 0-1 float
       progress = extract.last_position,       -- string, may be xpointer or ""
       timestamp = cand and cand.last_open      -- unix seconds
   }
   ```
9. **Timestamp for progress items.** In the live per-book path, `os.time()` seconds is used. In the sweep bulk path, `last_open` (also seconds) is used. The bridge should send `updated_at / 1000` converted to seconds.
10. **Device wrapper.** `withDevice` adds `deviceId`, `deviceModel`, `pluginVersion`, and `deviceTime` (local wall clock in `"%Y-%m-%d %H:%M:%S"`). The bridge must include these.

### 4.3 What the bridge must compute locally

- Percentage from Readest progress tuple: `cur / total` as a 0–1 float. Guard `total == 0`.
- Progress string: the bridge has no open document, so it must send `""` (empty string). This matches the plugin’s fallback behavior for percentage-only sync sources.
- Timestamp: convert Readest `updated_at` from milliseconds to seconds.
- Match state: persistent map `hash → { bookFileId, bookId, lastPushPct, lastPushTs }`.
- Watermark: `max(synced_at, updated_at, deleted_at)` in milliseconds.
- Unmatched cooldown: cache `unmatched[hash] = checkTime` and skip re-check for a configurable period (mirrors `bookorbit_state:setUnmatched`).

---

## 5. Technical Risks and Unknowns

| # | Risk / Unknown | Evidence | Mitigation |
|---|---|---|---|
| 1 | **Readest `progress` tuple units for EPUBs are opaque.** The plugin treats them as `cur/total` for its library progress bar, but the underlying values may be internal “locations,” not stable page numbers. | `librarystore.lua` stores the raw tuple; `syncbooks.lua` round-trips it. No conversion. | Accept the ratio as best-effort percentage. Test against live account. No exact-position guarantee. |
| 2 | **BookOrbit bulkProgress behavior with empty `progress` string.** The plugin always sends a real progress value from an open document. Sending `""` is untested against the BookOrbit server. | `progress_sync.lua` only falls back to GotoPercent on the *client* side; server acceptance of empty `progress` is unverified. | Test first; if rejected, send `"0"` or the first element of the Readest tuple as a string. |
| 3 | **BookOrbit timestamp comparison may advance progress incorrectly.** If the bridge sends older Readest `updated_at` seconds for a book while BookOrbit already has a newer timestamp from another device, the server may reject or keep its newer value. | `progress_sync.lua` uses `body.timestamp > local_timestamp` to decide remote-newer. | One-way sync means Readest wins only if BookOrbit accepts the write unconditionally. If the server silently ignores older timestamps, that is still acceptable behavior for one-way sync. |
| 4 | **Self-hosted BookOrbit version differences.** The plugin has fallback paths for pre-0.4 servers (legacy annotation upload). The bridge only needs progress endpoints, which are kosync-compatible and likely stable across versions, but unconfirmed. | `bookorbit_api.lua` lists legacy endpoints. | Target current BookOrbit API first; document minimum supported version. |
| 5 | **Rate limiting / body size behavior on BookOrbit.** Self-hosted, not documented. | Only the 900 KiB client-side cap is visible. | Make `PROGRESS_BATCH` and `MATCH_BATCH` configurable; default to plugin values (100 / 500). |
| 6 | **How Readest sends the anon key/Supabase project ID in the future.** If the project ID changes, the embedded base64 key becomes stale. | Currently hardcoded in `main.lua`. | Make Supabase URL and anon key configurable; ship hardcoded values as defaults only. |
| 7 | **Readest Sync API schema drift.** The API is unversioned. | `readest-sync-api.json` lacks a version path prefix. | Treat the Readest client as an adapter behind an interface; add integration smoke tests against a real account before releases. |
| 8 | **No webhook / push mechanism.** Polling is the only model supported by the source. | Both plugins are purely request/response with coroutine-based scheduling. | Polling daemon is correct; default interval should be user-configurable (e.g., 5–60 min). |
| 9 | **Multi-device Readest races.** If the user reads on two devices between polls, the bridge writes whichever `updated_at` is newest at pull time — exactly what the plugins already do. | Readest server resolves concurrent writes before the bridge sees them. | Not a bridge concern for v1. |
| 10 | **Book hash stability when a book is re-imported into Readest.** A new file upload produces a new partial MD5; progress stops updating under the old hash. | `librarystore.lua` dedupes only by exact hash match. | Document as known limitation; no cross-hash reconciliation exists in either plugin. |

---

## 6. Recommendations for the Architecture

1. **Adopt the bulk endpoint, not the per-book PUT.**
   - `POST /koreader/plugin/progress` is designed for many books in one request and is already used by the BookOrbit sweep. Use it for the bridge; fall back to `PUT /koreader/syncs/progress` only if the bulk endpoint is ever confirmed unsupported.
   - Batch size should default to 100 progress items, matching `PROGRESS_BATCH` in `bookorbit_sweep.lua`, with the total request body kept under 900 KiB.

2. **Model the state store after `bookorbit_state.lua`.**
   - Keep per-hash records: `bookFileId`, `bookId`, `lastPushedAt`, `lastPushedPercentage`.
   - Keep an `unmatched` map with `lastCheck` timestamps.
   - Keep auth tokens separate in the same store file so writes are atomic.
   - Because the bridge is headless, BoltDB, SQLite, or even a JSON file are fine; choose whichever makes atomicity easiest in Go. The key requirement is surviving partial runs and crashes.

3. **Mirror the Supabase refresh timing exactly.**
   - Proactive refresh when `expires_at - now <= expires_in / 2`.
   - Pre-request check: if token expires within 60 seconds, refresh first.
   - On a 401/403 from the sync API, refresh once and retry the request; if refresh fails, stop and surface auth failure.

4. **Do not reimplement partial MD5.**
   - The bridge never sees a book file. It receives hashes from Readest and forwards them to BookOrbit, relying on the existing partial-MD5 parity. This is the single most important simplification.

5. **Use a stable synthetic `device_id`.**
   - The bridge has no KOReader `device_id` setting. Generate and persist one on first run (e.g., a UUID). Use `device` = `"readest-bridge"` or a user-configurable value.

6. **Convert progress tuple to 0–1 percentage with robust guards.**
   - For each book row where `progress` is a two-element numeric array and `total > 0`: `percentage = cur / total`.
   - Round to a small number of decimal places (the plugin uses `Math.roundPercent`, effectively 4–5 decimals) to avoid floating-point churn.
   - Skip rows where `progress` is missing, non-numeric, or `total == 0`.

7. **Filter deleted and dummy rows.**
   - Skip `book_hash == "00000000000000000000000000000000"`.
   - Skip rows with non-null `deleted_at`.
   - Optionally remove a cached match for a deleted hash on the next full recheck.

8. **Implement a polling + graceful-shutdown model.**
   - Default interval: start with 15 minutes, make user-configurable.
   - A `--once` flag for cron/systemd timer deployments is useful even though the primary mode is a daemon.
   - Sleep between polls should be interruptible so `--once` and shutdown are fast.

9. **Observability over UI.**
   - Replace all KOReader `InfoMessage`/`Notification` calls with structured logging.
   - Expose metrics counters: books pulled, progress pushed, match-check misses, auth failures, retry counts.
   - Optional HTTP health endpoint for systemd / Docker.

10. **Resist scope creep in the package layout.**
    - Keep `internal/readest` and `internal/bookorbit` independent; only `internal/sync` imports both.
    - This makes future two-way sync or a third target (e.g., Calibre, another KOReader-compatible server) possible with minimal change.

11. **Testing strategy.**
    - Unit-test pure parsing (`parseSyncRow` equivalent, percentage calculation, URL normalization, timestamp conversion, body-size batching).
    - Add thin HTTP transport interfaces so the Readest and BookOrbit clients can be mocked.
    - Before any release, run manual integration tests against a real Readest account and a real BookOrbit server.

---

## 7. Open Questions That Must Be Resolved Before Implementation

1. **What is the minimum BookOrbit server version, and does it expose `/koreader/plugin/progress`?** If in doubt, the fallback to per-book `PUT /koreader/syncs/progress` must be implemented and configurable.
2. **Does the BookOrbit server accept an empty `progress` string in `bulkProgress`?** If not, what constant value should substitute for an unknown exact position?
3. **Does BookOrbit honor the `timestamp` field for one-way updates, or does it ignore older timestamps?** This determines whether duplicate pushes (same percentage, unchanged timestamp) are harmless or need client-side filtering.
4. **What `device`/`device_id` does the user want displayed in BookOrbit?** Default to `readest-bridge` + stable UUID, but allow override.
5. **Deployment target confirmation:** systemd service on the BookOrbit host or a separate container? This affects how credentials and state file paths are configured, and whether the binary should ship with a systemd unit.

---


## 8. BookOrbit server-side findings (cross&#8209;cutting — server source review)

*This section of the report was originally the dashboard-triggered-sync server-source write-up (§8.1–§8.4, the real-time chain, dashboard cache paths, custom-guard precedents, server lifecycle/hooks) plus the status-sync server-source analysis (§9.1–§9.8, Channel B contract, settable-enum, error classification, atomicity, `unread` non-settability rule, no-Socket.IO-subscriber-for-`book.status-changed` caveat). During the 2026-08-02 documentation reorganization, the full §8.1–§8.5 and §9.1–§9.8 content moved verbatim to the investigation docs where it belongs canto —* 

- Dashboard-triggered-sync server analysis → [`investigations/dashboard-triggered-sync.md` §3](./investigations/dashboard-triggered-sync.md)
- Status-sync Channel B server analysis → [`investigations/status-sync.md` Part III §6](./investigations/status-sync.md#part-iii--decisions-answered-after-bookorbit-server-source-review-status-sync-design-investigationmd-6-verbatim)

**No fact was removed.** The only shared cross-cutting finding preserved here is the `device ≠ bookorbit-web` rule (§8.5 in old numbering), which is load-bearing for the shipped bridge's `internal/sync/state/state.go:39` and cited by Phase 6 ADR Addendum 1. The other subsections are not lost — they're in their canonical folders.
