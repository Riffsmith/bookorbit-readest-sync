# Future Feature — Status / Completion Sync

**Status:** design investigation only. No production code has been written for this feature. This document is recorded as a candidate for implementation *after* Phase 8 (formal integration-test validation against live servers) is complete, per `docs/implementation-roadmap.md`.

**Scope:** one-way Readest → BookOrbit sync of *reading status* ("Mark as finished", "Mark as Unread", "Clear status", etc.) as an extension to the already-shipped progress bridge. Two-way status sync is explicitly *not* in scope here; `docs/problem-statement-prompt.md:24` declares Readest the source of truth, and that principle extends cleanly to status (BookOrbit reflects it, never writes back).

**Inputs reviewed:** `reference/readest.koplugin/library/readingstatus.lua`, `reference/readest.koplugin/library/statussync.lua`, `reference/readest.koplugin/library/syncbooks.lua`, `reference/readest.koplugin/library/librarystore.lua`, `reference/readest.koplugin/docs/library-design.md`, `reference/readest.koplugin/spec/library/{readingstatus,statussync,librarystore,syncbooks}_spec.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_catalog_util.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_sidecar.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_book_sync.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_sweep.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_api.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_catalog_detail.lua`, plus the shipped Phase 2-7 design and ADR documents in this `docs/` tree.

---

## 1. What the reference sources confirm

### 1.1 Readest-side status model

The Readest cloud row's `reading_status` field is a TEXT column carrying exactly three *decisive* values plus one *non-decisive* one:

| `reading_status` value | Decisive? | Source |
|---|---|---|
| `unread` | Yes | `readingstatus.lua:39` (`readest_decisive`) |
| `finished` | Yes | `readingstatus.lua:39` |
| `abandoned` | Yes | `readingstatus.lua:39` |
| `reading` | **No** — never synced | `readingstatus.lua:5-9`: *"KOReader auto-sets `summary.status = "reading"` the first time a book is opened, so 'reading' (and 'New'/absent) is treated as NON-DECISIVE and never captured — otherwise opening a finished book on KOReader would downgrade it."* |
| (`nil` / "New") | No | treated identically to `reading` |

The `library-design.md:259` schema comment (`'unread'|'reading'|'finished'` — the abandoned value is added later in the v1→v2 migration confirmed by `librarystore_spec.lua:42, 99, 264`) and the wire-shape in `syncbooks.lua:168-169` (`readingStatus = row.reading_status, readingStatusUpdatedAt = num(row.reading_status_updated_at)`) confirm the field already arrives *verbatim* on the bulk pull response the v1 bridge already consumes (`GET /sync?type=books`) — **no new Readest endpoint is required**.

**There is no `on_hold`, `paused`, `want_to_read`, `re-reading`, or `skimmed` value anywhere in the Readest plugin source, its tests, or its schema comment.** A grep for `on.?hold|paused|want_to_read|re-?reading|skimmed` across `reference/readest.koplugin/` returns zero matches in any `.lua` or `.md` file. The four UI actions the operator described as "Mark as Unread / Mark as finished / Mark on hold / Clear status" therefore translate to the wire as follows:

| Readest web UI action | Most likely `reading_status` value |
|---|---|
| "Mark as finished" | `finished` (decisive, syncable) |
| "Mark as Unread" | `unread` (decisive, syncable) |
| "Clear status" | `nil` / "New" (non-decisive, treated as no-op) |
| "Mark on hold" | **unknown — no enum entry exists in the plugin source.** Whatever the web UI does is not represented in the KOReader-plugin-facing API contract. |

### 1.2 BookOrbit-side status model — two distinct endpoints with two distinct enums

This is the load-bearing detail for any future design. BookOrbit exposes *two separate* status-write channels, and the wire enums they accept are *different*:

**Channel A: `POST /koreader/plugin/book-states` (bulk)** — declared in `bookorbit_api.lua:291-293`, exercised in `bookorbit_sweep.lua:664` and `bookorbit_book_sync.lua:431`. The `payload.status` field it accepts is taken directly from KOReader's per-book `summary.status`, whitelisted by `bookorbit_sidecar.lua:21-25`:

```lua
local ALLOWED_STATUSES = { reading = true, complete = true, abandoned = true }
```

This endpoint accepts **only KOReader-native tokens**: `reading | complete | abandoned`. It is *not* the right endpoint for the rich BookOrbit web enum. v1 of the bridge deliberately does not call this endpoint (Phase 5 design §6 explicitly listed it as out-of-scope *"reading time, highlights, status/rating, sweep-complete signal"*), so no work in the shipped bridge assumes either enum on this endpoint.

**Channel B: `PUT /koreader/plugin/catalog/books/{book_id}/read-status` (per-book)** — declared in `bookorbit_api.lua:325-327`, exercised from `bookorbit_catalog_detail.lua:563 applyReadStatus → catalogSetReadStatus`. The enum it accepts is the BookOrbit web-native enum, defined in `bookorbit_catalog_util.lua:84-90`:

```lua
CatalogUtil.SETTABLE_READ_STATUSES = {
    { id = "want_to_read" },
    { id = "reading" },
    { id = "on_hold" },
    { id = "read" },
    { id = "abandoned" },
}
```

Note: there is no explicit `unread` in the *settable* set — `unread` is the implicit baseline that the server returns for books with no set status (consumed at `bookorbit_catalog_widgets.lua:61-62` reading `book.readStatus`). The full `READ_STATUS_LABELS` table at `bookorbit_catalog_util.lua:71-80` *shows* `unread`, `skimmed`, and `rereading`, but those three are observation-only labels for the catalog UI, not settable from the plugin surface. The server's actual settable surface is exactly the five values above.

### 1.3 The Readest plugin already ships a pure bidirectional mapping

`reference/readest.koplugin/library/readingstatus.lua` is a 108-line, KOReader-globals-free module that already defines the exact contract a future bridge needs. The mapping tables at lines 24 and 32:

```lua
local READEST_TO_KO = { finished = "complete", abandoned = "abandoned" }
local KO_TO_READEST = { complete = "finished", abandoned = "abandoned" }
```

`M.readest_to_ko("unread")` returns `nil` (clear/"New"), and `M.readest_to_ko("reading")` returns `nil` (non-decisive, not pushed).

`M.reconcile(cloud, ko, now_ms)` at lines 61-106 is a pure LWW (last-write-wins) decider with a documented bootstrap carve-out:

- Neither side has a decisive status → no-op.
- Only cloud has a decisive status → wins; if its `reading_status_updated_at` is 0 (baseline/unsynced), stamp with `now_ms`.
- Only the local side has a decisive status → wins, with the same zero-stamp fallback.
- Both sides decisive and they agree → no-op.
- Both sides decisive and they disagree → bootstrap conflict: Readest authoritative (matches `docs/problem-statement-prompt.md:24`'s "Readest should be the source of truth").
- Steady-state conflict (both decisive, both have positive timestamps) → higher `*_updated_at` wins; ties break to Readest.

This module is the cleanest reference in the entire source tree for a future Go port. Its author already resolved every edge case the future bridge would face.

### 1.4 BookOrbit's own plugin treats status as newest-change-wins too

`reference/koreader-plugin/README.md:9`: *"Status & ratings: reading/complete/abandoned status and star ratings sync both ways, newest-change-wins."*

So the reconcile policy of `readingstatus.lua` and the BookOrbit README agree: LWW on timestamps is the conflict policy both sides already expect. The future bridge does not need to invent a new conflict policy.

---

## 2. Recommended mapping table

Built directly from §1.3's `READEST_TO_KO` plus §1.2's `SETTABLE_READ_STATUSES`, with the BookOrbit Channel B token substituted for the KOReader Channel A token. Channel A (`book-states`) is *not* the right endpoint for the future bridge — it expects KOReader-native tokens (`complete`), not BookOrbit-native ones (`read`). The future bridge should use Channel B exclusively.

| Readest `reading_status` (source of truth) | Bridge writes via Channel B `PUT /catalog/books/{id}/read-status` | Fit | Rationale |
|---|---|---|---|
| `finished` | `read` | **Exact semantic match** | Direct conceptual equivalent. This is the operator's stated #1 priority. The two-stage chain `finished → complete → read` (Readest → KOReader → BookOrbit) is exactly what the plugins already encode; the future bridge collapses it to one stage (`finished → read`). |
| `abandoned` | `abandoned` | **Token-identical** | Both ecosystems use the same string. Free bonus from the existing `readingstatus.lua` mapping. |
| `unread` (decisive) | *(no-op)* | **Good fit** | Channel B has no explicit `unread` token in `SETTABLE_READ_STATUSES` — `unread` is BookOrbit's server-side baseline, returned for any book with no set status. Pushing a no-op would be spurious. A `nil` Readest value is also a no-op, by the same reasoning. Document and skip — *do not attempt to write* `unread` to BookOrbit. |
| `reading` (non-decisive) | *(skip)* | **Token match, semantic hazard** | Both sides agree on the token, but `readingstatus.lua:5-9` explicitly refuses to sync this value: KOReader auto-sets `reading` on every first-open, so pushing it would downgrade a finished book any time a book is briefly reopened. The future bridge must inherit this precedent and *never push `reading`*. |
| `nil` / "New" / "Clear" | *(no-op)* | Compatible | `readest_to_ko` returns `nil`, which the bridge should treat identically to `unread` (skip). |
| `on_hold` | **not implementable from source evidence** | — | No `on_hold` value exists anywhere in the Readest plugin source, schema, tests, or docs (verified by grep across `reference/readest.koplugin/`). Whatever the Readest web UI's "Mark on hold" button does internally is not represented on the KOReader-plugin-facing API contract. A speculative mapping here would risk one-sided state corruption. **Defer implementation until a live-API probe of the actual Readest web UI confirms what (if anything) it writes to `reading_status`.** This is a Phase-8-style live-validation item, not something designable from the reference alone. |
| `want_to_read`, `rereading`, `skimmed` | n/a | n/a | These are BookOrbit-only statuses with no Readest-side source-of-truth equivalent. There is nothing to read *from* Readest to drive a push *into* BookOrbit. Out of scope by construction. |

### Priority alignment with the operator's stated needs

The operator's stated priorities were:
1. `Mark as finished` ↔ `Read` — Exact match. Highest priority, fully supported by source evidence.
2. `Mark on hold` ↔ `On hold` — **Not implementable today.** No `on_hold` token exists on the Readest side of any reference source.
3. "remaining Readest option matched to its closest BookOrbit pair" — `abandoned` ↔ `abandoned` is free (token-identical), and `unread`/`nil` ↔ BookOrbit-baseline is correct as a no-op.

---

## 3. Architectural implications for the shipped bridge

Five concrete effects on the already-shipped code, in increasing order of design effort. None of them requires touching v1's business logic; this section is written only to confirm feasibility for a future phase, not to seed changes into the current codebase.

### 3.1 Readest client — one new `BookRow` field

The v1 bridge intentionally minimal-modeled `BookRow` (`docs/phase-4-design.md §4.4` explicitly lists `reading_status` as one of the un-modeled fields, by design). A future status-sync phase would add two fields:

- `ReadingStatus string` — ported from `syncbooks.lua:168` (`readingStatus = row.reading_status`).
- `ReadingStatusUpdatedAt int64` — ported from `syncbooks.lua:169` (`readingStatusUpdatedAt = num(row.reading_status_updated_at)`).

Both fields already arrive on the bulk pull response the v1 engine already consumes; no new Readest endpoint is needed. The `librarystore_spec.lua:272-279` test pins the exact parsing (`"2026-06-18T00:00:00+00:00" → 1781740800000` ms, ISO-to-ms port of `iso_to_ms` already in the bridge's `readest` package).

### 3.2 BookOrbit client — one new method on `internal/bookorbit`

The v1 client (`docs/phase-5-design-temp.md` §2.1) exposes `Auth / MatchCheck / BulkProgress / UpdateProgress`. A future phase would add:

- `SetReadStatus(ctx, bookID int64, status string) error` — a per-book `PUT /catalog/books/{book_id}/read-status` with body `{ "status": status }`, modeled directly on `bookorbit_api.lua:325-327`. This is Channel B, *not* the bulk `book-states` endpoint (Channel A) — see §1.2 for why the endpoint split matters.

This is additive: no change to any existing method, no change to the `API` interface's other four method signatures. The `bookID` parameter is already cached in the v1 state store: `state.MatchRecord.BookID` (Phase 2, referenced in `docs/phase-5-design-temp.md §4.1`).

### 3.3 State schema — two new fields on `MatchRecord`

The v1 `state.MatchRecord` carries `LastPushedAt` / `LastPushedPct` for progress. A future phase would add:

- `LastPushedStatus string` — the last BookOrbit token successfully pushed (`read`, `abandoned`, or `""` for never-pushed).
- `LastPushedStatusAt int64` — the timestamp of that push, for the LWW decision.

This slots in next to `LastPushedPct`/`LastPushedAt` with no schema restructuring. The planning doc's Phase 1 directive was explicit: *"Design the `sync/state.go` schema so it could store BookOrbit's own timestamp per book, leaving room for a v2 bidirectional mode without a rewrite"* (`docs/planning.md §7`, last paragraph). Adding the two fields above is exactly the slot the planning doc reserved.

### 3.4 Engine — a new step in `RunOnce`

`internal/sync/engine.go`'s `RunOnce` is currently:

```
pull → classify → match-check → push-progress → save
```

A future phase would insert a status step:

```
pull → classify → match-check → push-progress → push-status → save
```

The push-status step obeys the same unchanged-percentage-skip tolerance pattern v1 uses for progress, adapted to status:

- For every matched book with a *decisive* Readest `reading_status` whose value (after mapping per §2) differs from `state.MatchRecord.LastPushedStatus`, queue a `SetReadStatus` call.
- Skip non-decisive values (`reading`, `nil`) entirely, following `readingstatus.lua:5-9`'s documented precedent.
- Skip the `unread` → `nil`-in-BookOrbit case as a documented no-op (no server call needed; BookOrbit defaults to its own `unread` baseline already).
- Batch the calls per the existing retry helper (`withRetry`) — but note Channel B is *per-book*, not bulk, so this step is N HTTP calls per poll, not 1. With the existing `RetryMaxAttempts` and exponential backoff, this is a cost worth measuring in a future design, not now.

### 3.5 Conflict resolution policy — port `readingstatus.reconcile` to Go

`readingstatus.reconcile` (lines 61-106) is a 46-line pure function. Its unit tests at `reference/readest.koplugin/spec/library/readingstatus_spec.lua` (200+ lines, 14 named cases) are direct port targets — they encode the bootstrap carve-out, the LWW steady state, the tie-break-to-Readest rule, and the non-decisive skip.

For a one-way bridge (Readest source-of-truth, matching v1's principle), only the *cloud-half* of `reconcile` is needed:

- Readest decisive, BookOrbit-side `LastPushedStatus` differs → push mapped value.
- Readest non-decisive (`reading` / `nil`) → no-op.
- Readest `unread` → no-op (BookOrbit defaults to `unread` baseline).

The bootstrap and bidirectional branches of `reconcile` are *not* needed for v2-status-one-way. They would become relevant only if the bridge were ever extended to two-way status sync (BookOrbit → Readest), which is out of scope per §`problem-statement-prompt.md:24`.

---

## 4. What this feature does NOT include (explicitly out of scope)

- **Two-way status sync (BookOrbit → Readest).** BookOrbit's statuses `want_to_read`, `rereading`, `skimmed` have no Readest-side source-of-truth value (§2 last row); pushing them into Readest would mean inventing what `reading_status` they should map to, which the reference does not define. Reverse-direction status sync is a separate, larger design problem beyond this feature.
- **Syncing `reading`.** Both plugins' authors explicitly flag this as non-decisive. Pushing it would un-finish a finished book any time the operator reopens it. Inheriting the precedent is the only safe choice.
- **Syncing the `unread` baseline as an explicit BookOrbit write.** `SETTABLE_READ_STATUSES` has no `unread` token; BookOrbit treats it as the implicit default. No-op is correct.
- **`on_hold` mapping.** Not implementable from source evidence — no `on_hold` token exists on the Readest side of any reference source. Requires live-API verification of what Readest's web "Mark on hold" button actually writes to `reading_status`; that probe is an item for Phase 8's live-validation work, not something this future feature can design from the reference alone.
- **Status via Channel A (`POST /koreader/plugin/book-states`).** The bulk endpoint accepts only KOReader-native tokens (`reading | complete | abandoned`), not BookOrbit-native ones. Sending `read` there would be an unverified wire shape and likely a 400. Channel B is the correct endpoint for BookOrbit-native tokens.
- **Sweep-complete notifications (`POST /koreader/plugin/sweeps`).** Phase 5 design §6 already excluded this; status sync does not require it. (Calling `sweeps` exists only for the BookOrbit plugin's own "last sweep" UI affordance, which a headless bridge does not need.)

---

## 5. Interaction with Phase 8

Per `docs/implementation-roadmap.md`, Phase 8 is *"formal integration-test validation against live Readest/BookOrbit servers."* Phase 8 is the natural place to discover whether the one item this design flagged as unsourceable — *"what does Readest's web 'Mark on hold' button actually write to `reading_status`?"* — has a verifiable answer.

Concretely, the Phase 8 verification work could include a single manual probe: trigger "Mark on hold" in the Readest web UI on a test book, then call `GET /sync?type=books&since=0` against the operator's real account and inspect the resulting row's `readingStatus` field. If it carries a previously-unseen token (e.g. `"on_hold"` or `"paused"`), this future feature can be extended to include the on-hold mapping. If it carries `nil` / `"reading"` / `"unread"` (i.e. the web button only affects local-view state, not the cloud-synced field), the on-hold mapping is genuinely unimplementable and should be documented as such in a Phase 8 addendum.

No code in the shipped bridge needs to change for any of this. Status sync remains a design-investigation-only future feature until and unless Phase 8's live probe (or a separate task) confirms the on-hold question and a decision is made to implement §3 above.

---

## 6. Summary

The reference sources directly support a future Readest → BookOrbit status-sync feature for two of the operator's three priority mappings:

- `finished` ↔ `read`: exact semantic match, fully implementable from source evidence.
- `abandoned` ↔ `abandoned`: token-identical, free.
- `unread` / `nil` ↔ BookOrbit baseline: correct as a no-op; no server call needed.

The third priority (`on_hold` ↔ `on_hold`) cannot be designed from the reference sources alone — the Readest plugin source, schema, tests, and design doc contain no `on_hold` token anywhere. That mapping's implementability hinges on a live-API probe that belongs in Phase 8 (or a separate verification task), not in design-time documentation.

A future implementation phase would touch four packages additively — `internal/readest` (one new `BookRow` field pair), `internal/bookorbit` (one new `SetReadStatus` method), `internal/sync/state` (two new `MatchRecord` fields), `internal/sync` (one new step in `RunOnce` plus a Go port of `readingstatus.reconcile`) — without restructuring any existing v1 surface. The conflict policy, the per-status mapping, the endpoint selection (Channel B over Channel A), and the non-decisive-skip precedent are all already encoded in `readingstatus.lua` and `bookorbit_catalog_util.lua`. This is among the cleanest future extensions available to the bridge.
