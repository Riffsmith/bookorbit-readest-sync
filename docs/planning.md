# Readest → BookOrbit Standalone Sync Bridge: Feasibility Investigation

*(Investigation only — no production code below. A few short pseudocode/request snippets are included strictly to document API shapes.)*

---

## 1. Executive Summary

Both existing KOReader plugins are, for the narrow slice we care about (**one-way reading-progress sync**), thin HTTP clients wrapping two well-defined REST APIs. The KOReader-specific coupling (document objects, sidecar files, `statistics.sqlite3`, annotation reconciliation, cover extraction, library UI) lives almost entirely in code paths that are **out of scope** for the MVP you've described (progress-only, one-way, no status/highlights).

The one load-bearing fact that makes this whole idea work at all: **Readest's `partialMD5` and KOReader's `util.partialMD5` produce byte-identical hashes**, and the `readest.koplugin` codebase explicitly relies on this already (`docs/library-design.md`, "Merge strategy" section). BookOrbit's own KOReader plugin also keys everything off this same partial-MD5 digest (`bookorbit_api.lua:matchCheck`, `updateProgress`). That means a headless bridge never needs to touch a book file at all — it only needs to move a hash + a percentage between two cloud APIs.

**Verdict: Moderate difficulty, clearly feasible.** The riskiest unknowns are two behaviors of the live Readest Sync API that cannot be fully confirmed from the plugin source alone (flagged in §8) — everything else is a straightforward two-legged REST client with token refresh and a small state store.

---

## 2. Feasibility Verdict

| Criterion | Assessment |
|---|---|
| Is this mostly API-wrapper code? | **Yes**, for the paths we need (`readest_syncauth.lua`, `readest_syncclient.lua`, `bookorbit_api.lua`). |
| Does it need KOReader's document object? | **No**, not for progress-only sync — only for the parts we're excluding (annotations, status, page-stats-from-live-reader, sidecar files). |
| Does it need KOReader's database? | **No.** `bookorbit_stats_reader.lua` reads `statistics.sqlite3` only for reading-time analytics/highlights bookkeeping — irrelevant to progress. |
| Lua libraries needing replacement | Spore (RPC client), `ltn12`/`socket.http`, `ffi/sha2` (md5), `rapidjson`/`dkjson`, `LuaSettings` (state file) — all have trivial equivalents in any modern language. |
| KOReader event coupling | Only in scheduling/debounce code (`onPageUpdate`, `onCloseDocument`, `Trapper` coroutines) — replaced by a plain polling loop in a daemon. |

**Classification: Moderate.**
Not "Easy" because two behaviors of the *live* Readest server (see §8, items 1–2) must be empirically verified since they're not fully specified in the plugin source. Not "Difficult" because there is no undocumented binary protocol, no OAuth dance, no reverse-engineered crypto — just REST + a Supabase JWT + a static credential header.

---

## 3. Architecture Recommendation

**Language: Go.**

| | Rust | **Go** | Node.js | Python |
|---|---|---|---|---|
| Long-running daemon fit | Excellent | **Excellent** | Good (needs pm2/supervisor) | Good (needs supervisor, GIL non-issue here — I/O bound) |
| Memory footprint | Lowest | Very low | Higher (V8) | Higher (interpreter) |
| HTTP client ergonomics | Good, more boilerplate | **Excellent** (`net/http`, easy JSON) | Excellent (`fetch`/`axios`) | Excellent (`requests`/`httpx`) |
| Async model | `tokio` (steep-ish learning curve) | **goroutines + channels**, native, simple | Event loop, easy | `asyncio`, works but more ceremony for a simple poller |
| Single static binary / deploy | Yes | **Yes** (cross-compile trivially, no runtime) | No (needs Node runtime + node_modules) | No (needs interpreter + venv) |
| Maintenance burden for a solo/OSS maintainer | Moderate (borrow checker overhead for little payoff here) | **Low** | Low-moderate | Low |
| Cron/systemd-timer + CLI ergonomics | Fine | **Fine, idiomatic** | Fine | Fine |

**Why Go wins for this specific project:** it's an I/O-bound polling daemon that periodically calls two REST APIs, does light JSON transformation, and persists a small local state file — exactly Go's sweet spot. A single static binary is trivial to ship as a systemd service or Docker container (same deployment story as a self-hosted BookOrbit user would expect). Rust would be defensible if you cared about the lowest possible resource footprint on constrained hardware (e.g., running the bridge on the same box as a Raspberry-Pi BookOrbit instance) — that's a legitimate secondary choice, but Go's faster iteration speed matters more for an MVP whose main risk is *API-shape uncertainty*, not performance.

Python is the fallback if you want the fastest path to "it works" and don't mind a venv + supervisor — genuinely fine for personal use, weaker for something you might want to package/distribute as an OSS project.

---

## 4. Reverse-Engineered Synchronization Flow (current KOReader-mediated path)

```
Readest client (web/desktop/mobile)
   │  writes book_configs row: {bookHash, metaHash, progress:[page,total] | xpointer, updatedAt}
   ▼
Readest Sync backend (Next.js @ web.readest.com, Postgres via Supabase)
   │
   │ (A) KOReader readest.koplugin — PULL (per open book)
   │     GET /sync?since=0&type=configs&book=<bookHash>&meta_hash=<metaHash>
   │     Auth: Authorization: Bearer <supabase_access_token>
   │     ⇐ { configs: [{ bookHash, metaHash, progress, xpointer, updatedAt }] }
   │     readest.koplugin.SyncConfig:applyBookConfig() → GotoPage/GotoXPointer on local doc
   ▼
KOReader's own reading position (in-memory + sidecar file on device)
   │
   │ (B) KOReader bookorbit.koplugin — PUSH (kosync-compatible)
   │     digest = util.partialMD5(file)      -- IDENTICAL hash Readest computed
   │     PUT /koreader/syncs/progress
   │       { document: digest, percentage, progress, device, device_id, timestamp }
   │     Auth: x-auth-user / x-auth-key headers (static, no expiry)
   ▼
BookOrbit server (self-hosted)
   │  matches book via digest (already known from BookOrbit's own library scan,
   │  or resolved once via POST /koreader/plugin/match-check)
   ▼
BookOrbit UI shows reading progress
```

**Key observation:** steps (A) and (B) are only glued together because they run inside the *same* KOReader process against the *same* file, sharing the same in-memory reading position. There is no direct API call from Readest to BookOrbit today — KOReader is a literal man-in-the-middle relay, and the only reason it works is the shared hash algorithm. **This is exactly the coupling a standalone bridge needs to replace** — not by opening files, but by moving `{hash, percentage, timestamp}` triples directly.

### Proposed bridge flow (MVP, one-way)

```
[Poll timer, e.g. every N minutes]
   │
   ├─ 1. Ensure Readest Supabase token fresh (refresh if <50% TTL, mirrors
   │       readest_syncauth.lua:withFreshToken threshold)
   │
   ├─ 2. GET https://web.readest.com/api/sync?type=books&since=<cursor>
   │       (bulk library pull — readest-sync-api.json "pullBooks")
   │       ⇐ [{ book_hash, meta_hash, title, author, progress:[cur,total], updated_at, deleted_at, ... }]
   │
   ├─ 3. For each row with a newer updated_at than our local watermark:
   │       percentage = cur / total   (see §8 open question on precision/units)
   │
   ├─ 4. (first time per book) POST BookOrbit /koreader/plugin/match-check
   │       { hashes: [book_hash], books: { book_hash: {title, authors, source} } }
   │       ⇐ { matches: [{hash, bookFileId, bookId}] }
   │       cache the match in local state (hash → bookId)
   │
   ├─ 5. PUT BookOrbit /koreader/syncs/progress
   │       { document: book_hash, percentage, progress: "<opaque>", device: "readest-bridge",
   │         device_id: <stable synthetic id>, timestamp: updated_at }
   │       Auth: x-auth-user / x-auth-key
   │
   └─ 6. Persist new watermark (max(updated_at)) + per-book last-pushed percentage,
          to avoid redundant PUTs and support incremental `since` on next poll.
```

No conflict resolution is needed for v1 because Readest is declared the sole source of truth and BookOrbit never writes back.

---

## 5. API Documentation (inferred from plugin source)

### 5.1 Readest — Auth (Supabase, `supabase-auth-api.json`)

Base URL: `{supabase_url}/auth/v1/` (default `https://readest.supabase.co/auth/v1/`)

| Method | Path | Auth | Notes |
|---|---|---|---|
| POST | `/token?grant_type=password` | `apikey: <anon key>` header | body `{email, password}` → `{access_token, refresh_token, expires_at, expires_in, user:{id, user_metadata}}` |
| POST | `/token?grant_type=refresh_token` | `apikey` header | body `{refresh_token}` → same shape |
| POST | `/logout` | Bearer | 204 on success |
| GET | `/user` | Bearer | current user info |

The anon key is public (bundled base64 in `main.lua`), not a secret — it just identifies the Supabase project.

### 5.2 Readest — Sync API (`readest-sync-api.json`)

Base URL: `https://web.readest.com/api` (hardcoded default in the plugin; presumably configurable for self-hosted Readest deployments — verify).

| Method | Path | Params | Purpose |
|---|---|---|---|
| GET | `/sync` | `since,type=configs,book,meta_hash` | Per-book progress/annotation/config pull |
| POST | `/sync` | body `{books,notes,configs,statBooks,statPages}` | Push (not needed for one-way bridge) |
| GET | `/sync?type=books` | `since` | **Bulk library pull** — every book row incl. `progress` tuple, `updated_at`, `hash`, `meta_hash` |
| GET | `/storage/download` | `fileKey` | Signed download URL (irrelevant to progress) |

Auth: `Authorization: Bearer <supabase_access_token>` on every call (`readest_syncclient.lua:ReadestAuth` middleware).

`book_configs` row shape (from `/sync?type=configs`):
```json
{ "bookHash": "...", "metaHash": "...", "progress": "[42,250]", "xpointer": "...", "updatedAt": 1234567890000 }
```
`books` row shape (from `/sync?type=books`, per `librarystore.parseSyncRow`):
```json
{ "book_hash": "...", "meta_hash": "...", "title": "...", "author": "...",
  "progress": [42, 250], "updated_at": "2026-...+00:00", "deleted_at": null, "synced_at": "..." }
```

### 5.3 BookOrbit (`bookorbit_api.lua`)

Base URL: `{server}/api/v1` (self-hosted, user-configured).
Auth: static headers `x-auth-user: <username>`, `x-auth-key: md5(password)` — no expiry, no refresh.

| Method | Path | Purpose |
|---|---|---|
| GET | `/koreader/users/auth` | Validate credentials |
| GET | `/koreader/syncs/progress/{digest}` | kosync-style progress pull (not needed, one-way) |
| PUT | `/koreader/syncs/progress` | `{document, percentage, progress, device, device_id, timestamp}` — **the write we need** |
| POST | `/koreader/plugin/match-check` | `{hashes:[...], books:{hash:{title,authors,lastOpen,source}}}` → `{matches, unmatched, libraryVersion}` |
| POST | `/koreader/plugin/progress` | Bulk progress `{items:[...]}` — **worth using instead of per-book PUTs** |

`bulkProgress` (item 4 in `bookorbit_api.lua`) is a materially better fit for a polling bridge than the singular `updateProgress` the progress-sync mixin uses — it lets us push N books in one request instead of N requests.

---

## 6. Important Source Files (by role)

**Readest side (reuse as spec, not as code):**
- `apps/readest.koplugin/readest_syncauth.lua` — Supabase login/refresh logic & thresholds
- `apps/readest.koplugin/readest_syncclient.lua` — Spore-based transport wrapper (replace transport, keep endpoint list)
- `apps/readest.koplugin/readest-sync-api.json` / `supabase-auth-api.json` — literal API contracts
- `apps/readest.koplugin/library/librarystore.lua` (`parseSyncRow`, `iso_to_ms`) — pure, portable parsing logic worth porting almost verbatim
- `apps/readest.koplugin/library/syncbooks.lua` (`pullBooks`) — bulk-pull + watermark pattern to imitate
- `apps/readest.koplugin/readest_syncconfig.lua` — shows the `configs` wire shape (only the parsing half is relevant)

**BookOrbit side:**
- `koreader-plugin/bookorbit.koplugin/bookorbit_api.lua` — the entire REST surface, portable almost 1:1
- `koreader-plugin/bookorbit.koplugin/bookorbit_progress_sync.lua` — conflict-strategy ideas for a *future* two-way mode (not needed now)
- `koreader-plugin/bookorbit.koplugin/bookorbit_state.lua` — local persisted match-state pattern to imitate (hash → bookId/watermark)

**Explicitly out of scope for MVP** (heavy KOReader coupling, not needed): `bookorbit_book_sync.lua`, `bookorbit_sweep.lua`, `bookorbit_sidecar.lua`, `bookorbit_stats_reader.lua`, `bookorbit_annotations.lua`, `library/readingstatus.lua`, `library/statussync.lua`.

---

## 7. Project Design (no code — structure only)

```
readest-bookorbit-bridge/
  cmd/bridge/           main.go            — CLI entry, flags: --config, --once, --daemon
  internal/
    readest/
      auth.go           login, refresh, token-fresh check (mirrors withFreshToken threshold)
      client.go         GET /sync?type=books, GET /sync?type=configs (if needed)
      models.go         BookRow, ConfigRow, isoToMs()  (port of parseSyncRow)
    bookorbit/
      client.go         matchCheck, updateProgress / bulkProgress
      models.go
    sync/
      engine.go         orchestration: pull → diff against watermark → match → push
      state.go          local state store (BoltDB/SQLite/JSON — hash → {bookOrbitBookId, lastPct, lastPushedAt})
    config/
      config.go         YAML/TOML: readest creds or stored token, bookorbit server+creds, poll interval
  configs/
    bridge.example.yaml
  docs/
    api-notes.md        the inferred API doc from §5, kept as living reference
```

**Config & secrets:** credentials on disk (0600 file, or OS keychain via a small abstraction) — same trust model KOReader's `G_reader_settings` uses today, just outside KOReader.

**Auth storage:** persist Supabase `access_token/refresh_token/expires_at` alongside sync state; refresh proactively at the same 50%-TTL threshold `readest_syncauth.lua` uses, so behavior matches the known-working reference implementation.

**Sync engine — polling, not event-driven.** Neither API exposes a webhook/push channel in the source we have; a poll loop (configurable interval, default maybe 15–30 min, matching casual reading cadence) is the correct model and mirrors what KOReader itself does (`onReaderReady`, debounced page-turn push, no push channel).

**Retry strategy:** exponential backoff on transient HTTP failures; treat 401/403 as "refresh token, retry once"; treat repeated match-check misses as "not yet matched in BookOrbit" and skip silently (don't error the whole run).

**Duplicate prevention:** local watermark (`max(updated_at)` from Readest bulk pull) + per-book "last pushed percentage" so we don't re-PUT unchanged progress every poll cycle — same idea as `getLastPushedAt`/`getLastPulledAt` in `librarystore.lua`.

**Conflict resolution:** none needed for v1 (Readest wins unconditionally, BookOrbit never writes back). Design the `sync/state.go` schema so it *could* store BookOrbit's own timestamp per book, leaving room for a v2 bidirectional mode without a rewrite.

**Extensibility:** keep `readest/` and `bookorbit/` clients ignorant of each other; `sync/engine.go` is the only place that knows both — makes it easy to add a third target later (e.g., Hardcover, StoryGraph) without touching the Readest client.

---

## 8. Potential Problems & Proposed Solutions

| # | Issue | Solution |
|---|---|---|
| 1 | **Unverified: does `/sync?type=configs` support a bulk/no-book pull?** Source only shows per-book calls. | Rely on the bulk `books` row's `progress` tuple instead (see §3/§4) — avoids the question entirely for percentage-level sync. Only fall back to per-book `configs` if exact resume position is ever required. |
| 2 | **Percentage semantics for reflowable (EPUB) books.** `progress:[cur,total]` on the books row — units unconfirmed (pages? "locations"? CFI-based ordinal?) from source alone. | Empirically test against the live API with a real account; if units are unclear, treat as best-effort percentage and document the caveat; degrade gracefully (skip percentage, still record "book started/updated" if truly unknown). |
| 3 | Readest sync API base URL is hardcoded (`web.readest.com`) — unclear if self-hosted Readest deployments exist/are supported. | Make base URL configurable in the bridge regardless; test against the actual hosted service first. |
| 4 | Supabase token expiry mid-run. | Proactive refresh at 50% TTL (matches existing plugin logic) + reactive refresh-and-retry-once on 401. |
| 5 | BookOrbit rate limits (none documented, self-hosted so likely soft). | Respect a configurable min-interval between requests; batch via `bulkProgress` where possible. |
| 6 | Missing/ambiguous book metadata → match-check misses. | Cache "unmatched" hashes with a cooldown before re-attempting (mirrors `bookorbit_state.lua:setUnmatched`), never hard-fail the run. |
| 7 | Multiple Readest devices updating the same book near-simultaneously. | Not our problem for v1 — Readest server already resolves this before we ever pull; we just take whatever `updated_at` is newest at pull time. |
| 8 | Book deleted in Readest (`deleted_at` set). | Skip/ignore on bridge side; optionally surface a log line; don't attempt to un-sync in BookOrbit for v1. |
| 9 | Book renamed / re-imported with new hash (rare, but happens if the file itself changes). | Nothing to reconcile automatically — old hash's progress simply stops updating; treat as a known limitation, document it. |
| 10 | ISBN/identifier mismatches for BookOrbit's own library scan vs. Readest metadata. | We don't rely on ISBN at all — matching is by partial-MD5 hash only, which sidesteps this entirely (this is the strongest part of the design). |
| 11 | Offline reading on Readest client (queued local writes). | Not our concern — by the time the bridge polls, Readest's own sync has already reconciled the client's offline queue into the cloud. |
| 12 | API schema drift on either service (unversioned self-hosted software). | Treat both client modules as adapters behind an interface (§7); add integration smoke-tests that hit real endpoints in CI/manually before each release. |

---

## 9. Existing Work

No evidence of a prior Readest ↔ BookOrbit direct bridge was found in the provided source trees, comments, or design docs (`docs/library-design.md`'s "GSTACK REVIEW REPORT" discusses only the Library-view feature, not a bridge). The KOReader-mediated path (readest.koplugin + bookorbit.koplugin coexisting on one device) appears to be the only documented workaround, and it is exactly the workaround you're trying to eliminate. I don't have live internet/GitHub search results to cite for external projects — recommend a manual check of the Readest and BookOrbit GitHub issue trackers / Discord for "bridge," "kosync," or "direct integration" threads before starting implementation, since this is the one part of the investigation I can't verify from the supplied documents alone.

---

## Assumptions Made

1. I have (or can obtain) valid Readest account credentials and a BookOrbit username/password, and are comfortable storing both on the machine running the bridge.
2. "Progress" for BookOrbit's purposes only needs to be *approximately* right (percentage-level), not the exact resume xpointer — this is what makes the bulk `books.progress` tuple sufficient and avoids the per-book `configs` pull.
3. Polling cadence of minutes-to-tens-of-minutes is acceptable (no real-time requirement was stated).

## Open Questions Before Implementation

1. Do you use the official hosted Readest sync (`web.readest.com`) or a self-hosted instance?
- No. I run the official hosted Readest sync
2. Is a resume-exact-position (xpointer) sync ever desirable, or is percentage-only genuinely sufficient long-term?
- It is desirable but not currently. Only if our bridge works than we can think in that direction
3. Preferred deployment target (bare-metal/systemd, Docker, NAS app) — affects whether Go's single-binary advantage matters as much as I'm weighting it?
- systemd other docker.
