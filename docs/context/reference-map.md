# Reference Map

This document maps the reference plugin source files to the standalone bridge implementation. Files are grouped by source plugin and tagged by relevance to the bridge.

**Legend**

- **In scope** — directly relevant to one-way progress sync and must be understood/ported.
- **Background pattern** — useful for state management, retries, or scheduling patterns, but not copied.
- **Out of scope** — KOReader-specific UI, annotations, catalog, storage, or statistics code.

---

## Readest plugin (`reference/readest.koplugin/`)

| File | Relevance | Notes |
|------|-----------|-------|
| `main.lua` | In scope | Default Supabase URL (`https://readest.supabase.co`), base-64 anon key, default sync API base (`https://web.readest.com/api`). Holds persisted auth/user fields. Defines KOReader-only code paths that are out of scope. |
| `readest_syncauth.lua` | In scope | Token-freshness logic (`needsLogin`, `tryRefreshToken`, `withFreshToken`). The 50 % TTL refresh threshold and blocking-refresh wrapper are authoritative. |
| `readest_supabaseauth.lua` | In scope | HTTP transport for Supabase auth: password grant, refresh token, sign-out, get-user. Shows `apikey` header requirement and request/response wiring. |
| `readest_syncclient.lua` | In scope | `pullBooks({ since })` for `GET /sync?type=books`. Bearer auth middleware. Async/Spore parts are KOReader-specific; endpoint semantics are not. |
| `readest-sync-api.json` | In scope | Literal sync API spec: `pullChanges`, `pushChanges`, `pullBooks`. Status codes. Base URL. |
| `supabase-auth-api.json` | In scope | Literal Supabase auth spec: `sign_in_password`, `refresh_token`, `sign_out`, `get_user`. |
| `library/librarystore.lua` | In scope | `parseSyncRow`, `iso_to_ms`, schema/upsert logic not needed, but row-field names and timestamp conversion are directly needed. Documents dummy-hash filter and dual-watermark pattern. |
| `library/syncbooks.lua` | In scope | `pullBooks` implementation, watermark advance logic (`max(synced_at, updated_at, deleted_at)`). Storage/upload/download helpers are out of scope. |
| `readest_syncconfig.lua` | Background pattern | Defines the `book_configs` payload shape (exact resume position/xpointer). Not used for percentage-only sync, but explains why `progress` on the books row is sufficient. |
| `docs/library-design.md` | Background pattern | Confirms partial-MD5 parity, DB-shaped `/sync` rows, watermark semantics, and lists large KOReader-specific features to ignore. |
| `readest_syncannotations.lua` | Out of scope | Annotations sync. |
| `readest_syncstats.lua` | Out of scope | Reading-statistics sync. |
| `readest_selfupdate.lua` | Out of scope | Plugin update checks. |
| `library/librarywidget.lua` / `libraryitem.lua` / `librarypaint.lua` / `libraryviewmenu.lua` / `localscanner.lua` / `coverprovider.lua` / `cloud_covers.lua` / `group_covers.lua` / `cloud_icons.lua` | Out of scope | Library UI, local file discovery, cover rendering. |
| `locales/*`, `icons/*`, `scripts/*`, `_meta.lua` | Out of scope | Localization, assets, build scripts, metadata. |

## BookOrbit plugin (`reference/koreader-plugin/bookorbit.koplugin/`)

| File | Relevance | Notes |
|------|-----------|-------|
| `bookorbit_api.lua` | In scope | **Authoritative REST client** for BookOrbit. Defines URL normalization, auth headers, request body shapes (`matchCheck`, `bulkProgress`, `updateProgress`), timeouts, JSON quirks, and body-size limit. |
| `bookorbit_state.lua` | Background pattern | Persistent state pattern: matched/unmatched maps, per-book watermarks. Good reference for the bridge’s local state schema. |
| `bookorbit_progress_sync.lua` | Background pattern | Live progress push/pull. Shows percentage/timestamp semantics and that an empty `progress` string is a valid fallback for percentage-only sources. Conflict UI not needed. |
| `bookorbit_book_sync.lua` | Background pattern | `capture()` and `stepProgress` show how the single-book `updateProgress` path is invoked. Useful for reference values only; bridge should prefer `bulkProgress`. |
| `bookorbit_sweep.lua` | In scope (select sections) | Full-library sweep contains the canonical usage of `matchCheck` and `bulkProgress`: batch sizes, response handling (`matches`, `unmatched`, `libraryVersion`), and device-field wrapping via `withDevice`. |
| `main.lua` | Background pattern | Plugin lifecycle, settings schema, `device_id` source, `userkey` storage. Confirms bridge needs stable `device_id` and MD5-hashed password for `x-auth-key`. |
| `bookorbit_annotations.lua` | Out of scope | Highlight/annotation exchange. |
| `bookorbit_book_sync.lua` (annotations/state paths) | Out of scope | Per-book annotation and status/rating upload. |
| `bookorbit_sweep.lua` (stats/annotations/pag states/done phases) | Out of scope | Statistics, annotation, and state phases of the sweep. |
| `bookorbit_catalog*.lua` | Out of scope | Catalog browser/dashboard. |
| `bookorbit_sidecar.lua` | Out of scope | Sidecar read/write. |
| `bookorbit_stats_reader.lua` | Out of scope | `statistics.sqlite3` access. |
| `bookorbit_highlight_summary.lua` / `bookorbit_open_annotation_scheduler.lua` | Out of scope | Annotation scheduling and summaries. |
| `bookorbit_sync_coordinator.lua` / `bookorbit_sync_job_runner.lua` | Out of scope | KOReader job scheduling/coordination UI. |
| `bookorbit_main_menu.lua` / `bookorbit_menu_pin.lua` | Out of scope | Menu UI. |
| `bookorbit_updater.lua` | Out of scope | Self-update. |

---

## Key Behavioral Constants to Port

| Constant | Source | Value / Rule |
|----------|--------|--------------|
| Supabase URL | `main.lua` | `https://readest.supabase.co` |
| Sync API base | `readest-sync-api.json` | `https://web.readest.com/api` |
| Anon key | `main.lua` | `SUPABAE_ANON_KEY_BASE64` decoded |
| Token refresh threshold | `readest_syncauth.lua` | Refresh when `expires_at < now + expires_in / 2`; also treat `expires_at < now + 60 s` as expired. |
| Dummy hash | `library/librarystore.lua` | `00000000000000000000000000000000` |
| Pull watermark | `library/syncbooks.lua` | `max(synced_at, updated_at, deleted_at)` (ms). Empty response leaves cursor unchanged. |
| BookOrbit path prefix | `bookorbit_api.lua` | `/api/v1` appended if missing. |
| Auth headers | `bookorbit_api.lua` | `x-auth-user: <username>`, `x-auth-key: <md5(password)>` |
| Match-check batch | `bookorbit_sweep.lua` | `MATCH_BATCH = 500` hashes per request. |
| Bulk progress batch | `bookorbit_sweep.lua` | `PROGRESS_BATCH = 100` items per request. |
| Max request body | `bookorbit_api.lua` | `900 * 1024` bytes. |
| Percentage range | `bookorbit_progress_sync.lua` | 0–1 float. |
| Progress timestamp | `bookorbit_progress_sync.lua` / `bookorbit_sweep.lua` | Unix seconds. |

---

## What Not to Port

- Any KOReader UI widgets, event handlers, menus, or notifications.
- Any file/disk access, partial-MD5 computation, or sidecar parsing.
- Any annotation, highlight, reading-statistics, status/rating, or catalog logic.
- Any download/upload/storage logic from Readest.
- Any coroutine / `Trapper` / `UIManager` scheduling patterns; replace with a plain Go polling loop.
