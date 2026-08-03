# Status Sync — Feasibility & Test-Strategy Investigation

**Status:** design investigation only. This file is the consolidated record of the two
pre-Phase-9 status-sync investigations the project ran. Phases 9 and 10 implemented
the feature; see

- [`../phases/phase-09-status-sync/design.md`](../phases/phase-09-status-sync/design.md)
- [`../phases/phase-09-status-sync/decision-record.md`](../phases/phase-09-status-sync/decision-record.md)
- [`../phases/phase-10-status-sync-decoupling/design.md`](../phases/phase-10-status-sync-decoupling/design.md)
- [`../phases/phase-10-status-sync-decoupling/decision-record.md`](../phases/phase-10-status-sync-decoupling/decision-record.md)

> **Single-glance summary (Part III gives the full record):**
> - Decisions A–F are settled against the live BookOrbit server source (Part III §6).
> - `finished → read` and `abandoned → abandoned` are the only two push mappings.
> - `unread`/`nil` is a documented no-op (BookOrbit baseline, no server write).
> - `reading` is non-decisive and is never pushed (precedent: `readingstatus.lua:5-9`).
> - The **`on_hold` mapping remains the only true open live-probe item** — no
>   `on_hold` token exists anywhere in the Readest plugin source, schema, tests,
>   or design docs. Resolution requires the operator's live Readest-web-UI probe
>   (see Part I §5 / Part III §6.8), tracked in
>   [`../live-validation-status.md`](../live-validation-status.md).

This file consolidates, without rewording, three chronological pieces:

| Part | Originally authored as | Date | Role |
|---|---|---|---|
| [Part I](#part-i--reference-source-feasibility-future-status-syncmd-verbatim) | `docs/future-status-sync.md` | 2026-07-28 | Reference-source feasibility investigation (Readest + BookOrbit plugin source) |
| [Part II](#part-ii--test-strategy-and-open-decisions-status-sync-design-investigationmd-1-5-verbatim) | `docs/status-sync-design-investigation.md` §1–§5 | 2026-07-28 | Test strategy for each additive touch-point; six open decisions (A–F) |
| [Part III](#part-iii--decisions-answered-after-bookorbit-server-source-review-status-sync-design-investigationmd-6-verbatim) | `docs/status-sync-design-investigation.md` §6 | 2026-07-31 | Decisions A–F resolved against the BookOrbit NestJS server source |

The three parts below are the verbatim text of those sources (only their H1 titles
removed, since they would collide with the headings here). No decision has been
re-litigated, reworded, or omitted. The original two files were removed when this
consolidated file was created; their content survives intact here.

---

# Part I — Reference-source feasibility (`future-status-sync.md`, verbatim)

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

---

# Part II — Test strategy and open decisions (`status-sync-design-investigation.md` §1–§5, verbatim)

**Status:** design investigation only, per `future-status-sync.md`'s own framing — no production code proposed or written here. This document takes that file's findings as given (I'm not re-deriving the mapping table, the Channel A/B endpoint split, or the `on_hold` unknown — those are settled by the reference-source evidence already gathered) and does two things the source file explicitly left open: (1) shape the four additive touch-points it names (§3 of `future-status-sync.md`) into concrete, testable units, and (2) design the test strategy for each — what it verifies and why it lives at that level. This is **one-way Readest → BookOrbit only**, per the source file's explicit scope.

---

## 1. Scope recap (from `future-status-sync.md`, not re-litigated)

- Readest `reading_status` (`unread | reading | finished | abandoned`, `reading` non-decisive) is the sole source of truth.
- BookOrbit is written to via **Channel B** (`PUT /catalog/books/{id}/read-status`, tokens `read | abandoned | on_hold | want_to_read | reading`), never Channel A.
- Mapping: `finished→read`, `abandoned→abandoned`, `unread`/`nil`→no-op, `reading`→skip (non-decisive).
- `on_hold` is **not implementable** from reference evidence alone; it stays a documented gap pending a Phase-8-style live probe. Nothing below designs or tests an `on_hold` path — that would be inventing behavior the reference doesn't support.
- Two-way sync, Channel A, and sweep notifications are explicitly out of scope (source file §4) and stay out of scope here too.

---

## 2. Proposed shape of the four additive touch-points

Naming these precisely is necessary before the test strategy in §3 can say anything concrete — but nothing here is implemented, and every signature is a proposal, not an existing API.

### 2.1 `internal/readest`: two new `BookRow` fields
```
ReadingStatus          string  // "unread" | "reading" | "finished" | "abandoned" | ""
ReadingStatusUpdatedAt int64   // ms epoch, via the existing ISOToMs port
```
Populated straight off the existing bulk-pull response (`future-status-sync.md` §1.1/§3.1 already confirms no new endpoint is needed).

### 2.2 A new pure function: `mapReadingStatus`
This is the Go port of `readingstatus.lua`'s `READEST_TO_KO`, but collapsed to the one-way, Channel-B-only shape §2 of the source file already derived:

```
func mapReadingStatus(readestStatus string) (token string, push bool)
```

- `"finished"` → `("read", true)`
- `"abandoned"` → `("abandoned", true)`
- `"unread"` → `("", false)` — decisive, but a documented no-op (BookOrbit's own baseline)
- `""`, `"reading"` → `("", false)` — non-decisive, skip
- anything else (a future/unrecognized token) → `("", false)` — see Decision D in §4

This function alone carries the entire semantic content of `future-status-sync.md` §2's mapping table. Keeping it pure and separate from the engine (mirroring how `util.Percent`/`ISOToMs` are separate from `sync.Engine`) is what makes it table-testable in isolation.

### 2.3 `internal/bookorbit`: one new client method
```
SetReadStatus(ctx context.Context, bookID int64, status string) error
```
Per-book `PUT`, body `{"status": status}`, modeled on `bookorbit_api.lua:325-327` exactly as `future-status-sync.md` §3.2 proposes. No device-field wrapping is implied by the reference (Channel B is book-scoped, not device-scoped, unlike the progress endpoints) — flagged as an assumption in §4, Decision B.

### 2.4 `internal/sync/state`: two new `MatchRecord` fields
```
LastPushedStatus   string  // "read" | "abandoned" | "" (never pushed / no-op state)
LastPushedStatusAt int64
```

### 2.5 `internal/sync`: one new `RunOnce` step
```
pull → classify → match-check → push-progress → push-status → save
```
For each matched book: compute `(token, push) := mapReadingStatus(row.ReadingStatus)`. If `push` and `token != record.LastPushedStatus`, call `SetReadStatus`; on success, set `LastPushedStatus = token`, `LastPushedStatusAt = now`. If the mapped result is a decisive-but-no-op case (`unread`), **still** update local bookkeeping (see Decision A, §4) so a later transition away from `unread` is detected as a real change rather than compared against stale data.

---

## 3. Proposed test strategy

### 3.1 `mapReadingStatus` — pure function, table-driven

| Test case | Behavior verified | Why this level |
|---|---|---|
| `"finished"` → `("read", true)` | The operator's #1 priority mapping is exact and stable. | **Unit.** Pure function, zero I/O, zero state — the textbook table-driven case, same shape as the existing `TestPercent`/`TestNormalizeBookOrbitURL`. |
| `"abandoned"` → `("abandoned", true)` | The token-identical mapping doesn't get accidentally transformed (e.g. no stray casing/whitespace handling breaks the passthrough). | **Unit.** Same reasoning — a pure identity mapping is exactly as cheap and exactly as worth pinning as a non-trivial one; regressions here are silent (both strings look "fine" individually). |
| `"unread"` → `("", false)` | The explicit no-op case is a *decision*, not an omission — a future edit that "helpfully" adds an `unread` token to the push path must fail this test. | **Unit.** Encodes a specific design decision (§2 of the source doc: "do not attempt to write `unread` to BookOrbit") as a regression guard, the same role `TestRunOnceUnmatchedWithinCooldownSkipsRecheck`-style tests play for engine decisions. |
| `""` and `"reading"` → `("", false)` | Non-decisive values never produce a push token. This is the single most safety-critical case in the whole feature — `readingstatus.lua:5-9`'s entire rationale is "don't downgrade a finished book on reopen." | **Unit.** Pure, and specifically the case most worth a named regression test given the source module's own commentary flags it as the one mistake that would silently corrupt user data. |
| An unrecognized/future string (e.g. `"skimmed"`, or a hypothetical new Readest enum value) → `("", false)`, not an error | Defines the function's behavior for values the reference doesn't currently define, so it fails safe rather than either pushing a garbage token to BookOrbit or panicking. | **Unit.** Same table, one more row — cheap insurance for schema drift, consistent with the project's general defensive posture toward the third-party APIs (`docs/reverse-engineering-report.md` risk #7). See Decision D, §4, for whether "fail-safe-skip" vs. "log-and-skip" is the right default. |

No integration test targets this function directly — it has no I/O, so there is nothing an integration test could exercise that the table above doesn't already cover exhaustively.

### 3.2 `readest.BookRow` — `ReadingStatus`/`ReadingStatusUpdatedAt` decoding

| Test case | Behavior verified | Why this level |
|---|---|---|
| A bulk-pull JSON fixture with `readingStatus: "finished"`, `readingStatusUpdatedAt: "2026-06-18T00:00:00+00:00"` decodes to `ReadingStatus == "finished"`, `ReadingStatusUpdatedAt == 1781740800000` | The wire-shape/ISO-parsing claim `future-status-sync.md` §3.1 cites from `librarystore_spec.lua:272-279` actually holds once ported to Go's JSON decoding + the existing `ISOToMs`. | **Unit.** This is exactly the same shape as the existing `TestBooksResponseDecode`/`TestProgressTupleDecoding` tests in `models_test.go` — a fixture-decode assertion, no network involved. |
| `readingStatus` absent / `null` on a row decodes to `""` (not a decode error) | Real Readest rows for books that have never had a status set must not break decoding — this mirrors the existing `null`-progress-tuple tolerance already tested for `ProgressTuple`. | **Unit.** Same file, same pattern; this is the row-tolerance discipline the codebase already applies everywhere a third-party field might be absent. |

### 3.3 `bookorbit.SetReadStatus` — mocked HTTP, `stubDoer` pattern

| Test case | Behavior verified | Why this level |
|---|---|---|
| Success (200): correct method (`PUT`), correct path (`/catalog/books/{id}/read-status`, id substituted, not templated wrong), correct body (`{"status":"read"}`), headers present | The client builds the exact request shape the reference source declares, before any real server is involved. | **Unit** (mocked HTTP). Directly mirrors every existing method's own "request shape" test (e.g. `MatchCheck`'s path/header test in `client_test.go`) — same doer-recording technique, no new tooling. |
| 401/403 → same auth-retry-then-classify behavior as the other three methods | Status push doesn't get a bespoke, inconsistent auth-handling path; it reuses the client's established force-refresh-and-retry convention. | **Unit.** This is a regression guard that the *existing* auth-retry logic (already unit-tested for `MatchCheck`/`BulkProgress`/`UpdateProgress`) actually gets invoked for the new method too — cheap, and catches "forgot to wire it into the shared retry wrapper" mistakes. |
| 404/405 → classified the same way `UpdateProgress`'s "unsupported endpoint" case is (or a new sentinel — see Decision C) | Defines what happens when an operator's BookOrbit deployment doesn't yet have Channel B at all. | **Unit**, once the classification decision (§4, Decision C) is made — until then this is a placeholder row, not a committed test. |
| Network error → wrapped client error, no partial state mutation | Consistent with every other method's network-error handling; status push shouldn't leave `MatchRecord` in a half-updated state on transport failure. | **Unit.** Same `stubDoer`-with-`err` pattern already used for `MatchCheck`/`BulkProgress`. |
| Malformed/oversized response body → same bounded-snippet handling as the other three methods (**if** Channel B returns a JSON body at all — see Decision B) | Consistency with the existing defensive parsing discipline. | **Unit**, contingent on Decision B resolving what the real response shape is. |
| Context cancellation mid-request | Consistent behavior with the other three methods; not a status-specific new capability. | **Unit.** Same `delay`+`context.WithTimeout` `stubDoer` technique already used four times in the existing suite. |

No new mocking infrastructure — this is `stubDoer` with a fifth recorded call type, exactly like `future-status-sync.md`'s own claim that this is "additive: no change to the `API` interface's other four method signatures."

### 3.4 `state.MatchRecord` — `LastPushedStatus`/`LastPushedStatusAt`

| Test case | Behavior verified | Why this level |
|---|---|---|
| Round-trip through both `MemStore` and `FileStore` (save, reload, fields intact) | The two new fields participate correctly in the existing persistence contract. | **Unit**, and specifically it should be added *inside* the existing `exerciseStore` helper (`state_test.go`) rather than as a new standalone test — that helper's entire design purpose is running one assertion set against both store implementations, and these two fields are exactly the kind of addition it was built to absorb without duplication. |
| `FileStore`'s atomic-write-leaves-valid-JSON guarantee still holds with the new fields present | No regression to the existing atomicity guarantee just because the struct grew. | **Unit.** Same reasoning — this is a property of the store, not of the new fields specifically, so it belongs in the existing atomicity test, extended, not a new one. |

No new file needed here — this is two lines added to an existing fixture struct and its already-comprehensive contract test.

### 3.5 Engine `push-status` step — scripted fakes, mirroring `engine_test.go`'s existing style

| Test case | Behavior verified | Why this level |
|---|---|---|
| Matched book, `ReadingStatus="finished"`, `LastPushedStatus=""` → `SetReadStatus(ctx, bookID, "read")` called once; state updated to `LastPushedStatus="read"` | The core forward-progress case: a genuinely new decisive status gets pushed exactly once. | **Unit.** This is the direct analog of `TestRunOnceFreshMatchAlwaysPushes` for progress — same engine, same `scriptedBookOrbit`-style fake, just asserting on a different call and a different state field. |
| Matched book, `ReadingStatus="finished"`, `LastPushedStatus="read"` already → no `SetReadStatus` call | Unchanged status doesn't re-push every poll — the same "unchanged-pct skip" discipline v1 already applies to progress, ported to status. | **Unit.** Direct analog of `TestRunOnceUnchangedPctSkipsPush`. |
| Matched book, `ReadingStatus="reading"` (or `""`) → no `SetReadStatus` call, regardless of `LastPushedStatus` | Non-decisive values are never pushed — this is the single highest-value test in the whole feature (see §3.1's identical concern at the pure-function level; this test proves the *engine* actually honors it, not just the mapper function in isolation). | **Unit**, but deliberately duplicated in spirit from §3.1: the pure-function test proves `mapReadingStatus` returns `push=false`; this engine-level test proves `RunOnce` actually *skips the call* when that's what the mapper says — the same "two levels, two behaviors" reasoning `future-status-sync.md`'s own dummy-hash-filter discussion already applies (pure predicate vs. engine consequence are different tests, not duplication). |
| Matched book, `ReadingStatus="unread"` → no `SetReadStatus` call, **but** local bookkeeping still updated (per Decision A) so a later `unread→finished` transition is correctly detected as new | Proves the "decisive no-op still updates state" design choice from §2.5 actually prevents a stale-comparison bug, mirroring the care `engine_test.go` already takes with the delete→re-add "no stale-pct bleed-through" test (`TestRunOnce...ReappearedHashTreatedAsBrandNew`). | **Unit.** Same class of regression as that existing test — a state-staleness bug that would only surface days later in real use, exactly the kind of thing worth pinning at the unit level before it ever reaches a live account. |
| Unmatched book (no `BookID` yet) → status step skipped entirely for that book, no panic on a zero/absent `BookID` | Status push depends on match-check having already resolved a `BookID`; an unmatched book must not crash or send a garbage ID. | **Unit.** Same defensive-ordering concern already proven for progress push against unmatched books. |

> **Phase 10 supersedent note (2026-08-02).** This row's premise — "match-check has already resolved a `BookID`" — was never guaranteed before Phase 10, because the shipped Phase 9 engine's row-classification gate dropped null-progress decisive-status rows from the match-check queue before they were ever considered (see `internal/sync/engine.go` prior to Phase 10). Phase 10 (`docs/adr/phase-10-decision-record.md`; design at `docs/phase-10-status-sync-decoupling-design.md`) decoupled the status step from the progress classifier so the premise now holds: a row carrying a decisive `reading_status` is status-eligible on its own merits, and the unified MatchCheck pass resolves `BookID`s for rows regardless of whether they also carry a usable progress tuple. The row's *test* behavior ("status step skips a still-unmatched book without panicking") is unchanged; what changed is which rows reach the status step at all.
| `SetReadStatus` returns an error for one book in a multi-book `RunOnce` → that book's state is left unchanged (no partial `LastPushedStatus` update on failure); **and** — pending Decision E — either (a) that book's watermark retreats like a progress-push failure, or (b) the failure is isolated to the status step only and the book's progress/watermark still advance | Proves failure atomicity per book, and pins whichever of (a)/(b) is decided in §4. | **Unit** for the "no partial update" half (uncontroversial, mirrors existing `Save()`-called-once-per-outcome tests); the watermark-interaction half can't be written as a real test until Decision E is resolved — it's a placeholder here, not a committed case. |
| A `RunOnce` with several matched books, mixed statuses (`finished`, `reading`, `abandoned`, already-synced `read`) processes each independently — one push, one skip, one push, one skip, in the same pass | Confirms the step composes correctly across a realistic mixed batch, not just single-book cases. | **Unit.** Direct analog of the existing multi-book engine tests (e.g. the multi-row deleted-book case in Test 5's live evidence, but exercised here as a fast scripted-fake unit test rather than requiring a live account). |
| `RunOnce` continues to call `Save()` exactly once even when the new step is added | Guards against the new step accidentally introducing a second/duplicate persistence call. | **Unit.** Mirrors the existing `TestRunOnceSaveCalledExactlyOnce`-style assertions; cheap and catches a real class of bug (double-writes) that a manual reviewer might miss. |

### 3.6 Manual/live integration tests (once implementation is approved and built)

These follow the exact template `docs/phase-8-design.md` recommends (§5.2 of that document: state the target claim, concrete setup steps, exact expected log/state-file assertions, always include an immediate second run for idempotency).

| Test | Behavior verified | Why this level |
|---|---|---|
| Mark a real book "finished" in the Readest web UI → run the bridge → confirm BookOrbit's catalog shows `read` for that book, and a second immediate run makes zero additional `SetReadStatus` calls | Proves the entire chain (real `reading_status_updated_at` field shape, real Channel B response, real idempotent re-run) end-to-end — this is exactly the kind of "does the real server actually behave the way the reference source says" question no unit test (which only exercises fakes) can answer. | **Integration/manual.** Same justification `future-status-sync.md`'s own Phase-8-analogy already gives for the equivalent progress-push live test (Test 2 in `live-test-reports.md`). |
| Mark a real book "unread" (or clear its status back to New) → run the bridge → confirm zero `SetReadStatus` calls are made (check debug logs) and BookOrbit's catalog is untouched | Confirms the no-op decision (§2 of the source doc) holds against the real API, not just the mapper's unit test — specifically, that BookOrbit doesn't require an explicit "reset" call to un-set a status, which the reference source implies but doesn't prove. | **Integration/manual.** This is a genuine "does the real server require a null-write we didn't anticipate" question — can't be answered from source alone. |
| Mark "abandoned" in Readest → confirm the token-identical mapping actually round-trips with no server-side surprise (e.g., BookOrbit doesn't reject `abandoned` if the book has no prior status) | Confirms the free/token-identical mapping isn't secretly gated behind some precondition the reference source didn't mention. | **Integration/manual.** Low-risk, but cheap to fold into the same probe session as the two tests above. |
| **The `on_hold` probe** described in `future-status-sync.md` §5: trigger "Mark on hold" in the Readest web UI, then inspect the raw `reading_status` field via `GET /sync?type=books&since=0` | Answers the one question the reference source explicitly could not answer: does Readest's web UI write anything at all to the cloud-synced field for "on hold," and if so, what token? | **Integration/manual — and it must happen before any `on_hold` design work is attempted**, not after. This is not a test *of* the feature; it's a prerequisite fact-finding step the feature's design is explicitly blocked on (source doc §4/§5/§6). |
| Trigger a genuine conflicting/edge condition — e.g., mark "finished" in Readest, let the bridge push it, then re-open the book in a KOReader-side flow that the plugin ecosystem treats as resetting `reading_status_updated_at` to a lower/equal timestamp — and confirm the bridge does not oscillate | Since this feature is one-way (no BookOrbit→Readest write-back), there is no bidirectional conflict to test — but it's worth confirming that repeated identical pushes from Readest's side alone don't cause chatter. | **Integration/manual**, low priority — mostly a sanity check that the "unchanged-skip" unit test (§3.5) generalizes to a real repeated-poll scenario, similar in spirit to `live-test-reports.md` Test 1's idempotent re-run. |

**What's deliberately *not* on this integration list:** any two-way/conflict-resolution scenario (out of scope per source doc §4), and any `want_to_read`/`rereading`/`skimmed` probe (no Readest-side source of truth exists to drive a push, so there's nothing to test even manually).

---

## 4. Decisions requiring approval

Kept to the items that are genuinely undetermined by the reference evidence — not re-opening anything `future-status-sync.md` already settled (mapping table, Channel B selection, `on_hold` deferral, out-of-scope list).

**A. Should a decisive-but-no-op Readest status (`unread`) still update local bookkeeping even though no BookOrbit call is made?**
*Recommend: yes.* Without recording that the bridge has "seen" `unread` at a given timestamp, a later transition `unread → finished` has nothing correct to diff against on the BookOrbit-token side, and a transition `finished → unread` (an explicit "clear status" after having pushed `read`) would need *some* record of the clear to avoid perpetually appearing "changed." This likely means the `MatchRecord` needs to track the last-*seen* Readest status separately from the last-*pushed* BookOrbit token (two related but distinct fields), which is a small addition beyond what `future-status-sync.md` §3.3 proposed. Flagging because it's a real schema question, not just an implementation detail.

**B. Does Channel B require device-scoped fields, and what does a success response actually look like?**
The reference evidence (`bookorbit_api.lua:325-327`, `bookorbit_catalog_detail.lua:563`) shows the request is book-scoped, unlike the progress endpoints — but the KOReader-plugin call site isn't the same actor as this future headless bridge, and neither is confirmed to require `deviceId`/`pluginVersion` the way `MatchCheck`/`BulkProgress` do. Similarly, no reference source specifies whether a successful call returns `200` with a body, `204` with none, or an echo of the updated book row.
*Recommend: this is a live-probe item, not a design-time guess* — bundle it into the same Phase 8/9-style manual session as the `on_hold` probe (§3.6), before writing the client method, rather than assuming a shape and discovering it's wrong in production.

**C. How should 404/405 on Channel B be classified — as a hard error, or as a "feature unsupported on this BookOrbit version" soft-disable (analogous to progress's bulk→singular fallback, but with no secondary endpoint to fall back to)?**
*Recommend: soft-disable* — log a single `WARN` the first time it's seen, then skip the status step for the remainder of that run (and future runs, via a small in-memory or persisted flag) rather than treating it as a fatal `RunOnce` error, since an operator running an older BookOrbit deployment shouldn't lose progress sync just because status sync isn't available yet. This mirrors the spirit of the existing bulk-progress-unsupported fallback design, adapted to "no fallback exists, so disable gracefully" instead of "fall back to another endpoint."

**D. Should an unrecognized/future Readest status value be silently skipped, or logged at `warn` once?**
*Recommend: log at `warn` (rate-limited/once-per-value, following the existing `debug`-vs-`warn` log-level discipline from the Addendum 2 fix), not silent.* A future Readest schema change introducing a genuinely new decisive value (however unlikely, given none has appeared in years of the plugin's history) should be visible to an operator, not silently dropped — this is a cheap, low-risk logging decision but it's still a real choice between "fail silently" and "fail loud," so flagging it rather than assuming.

**E. Does a `SetReadStatus` failure retreat that book's watermark (like a progress-push failure does), or is it isolated so progress still advances even if status fails?**
This is the most architecturally meaningful open question in this document. Two reasonable positions:
- **(a) Couple them** — treat "fully synced this book" as requiring both progress and status to succeed, so a status failure retreats the watermark exactly like a progress failure does today, guaranteeing the next poll retries the whole book.
- **(b) Decouple them** — since status and progress are semantically independent facts about a book, a transient status-push failure (e.g., a 500 on Channel B) shouldn't block progress from advancing, and the status step could carry its own lightweight retry-next-poll marker independent of the watermark.
*No recommendation forced here* — this genuinely depends on whether the project wants "the bridge doesn't advance past a book until everything about it succeeded" (v1's existing philosophy for progress) or "each write channel to BookOrbit is independently retryable" (more resilient, more moving parts). This should be decided before §3.5's engine tests are finalized, since it changes what those tests assert.

**F. Should status sync be gated behind a config toggle (e.g., a `sync_status: true|false`, defaulting to off) for the first release of this feature?**
Not addressed in `future-status-sync.md` at all, but worth raising: unlike progress (which is purely additive/informational), a status write changes what the operator's BookOrbit catalog *displays as the book's state* — an operator who wants progress bars but doesn't want the bridge editing their manually-curated BookOrbit read-status shelf has no way to opt out under the design as currently sketched.
*Recommend: yes, default-off, opt-in* — consistent with the project's general caution around write side-effects on a service it doesn't own, and cheap to add to the existing `config` package (one more boolean, one more `Validate()` no-op case) without disturbing anything in Phases 1–7.

---

## 5. What this document does not do

- Does not re-derive or second-guess the mapping table, the Channel A/B split, or the `on_hold` deferral — those are `future-status-sync.md`'s findings, taken as settled.
- Does not propose any two-way/reconcile logic — one-way only, per scope.
- Does not write any Go code, test code, or struct definitions beyond the signatures needed to make the test strategy concrete.
- Does not assume answers to Decisions B, C, E, or F — those are named specifically because the reference evidence doesn't resolve them, and building the feature around a guessed answer would risk exactly the kind of rework Phase 8's own audit was designed to avoid.

---

# Part III — Decisions answered (after BookOrbit server source review) (`status-sync-design-investigation.md` §6, verbatim)

**Date of review:** 2026-07-31. **Evidence source:** the BookOrbit server source shipped at `reference/bookorbit/server/` (NestJS Fastify + Drizzle ORM, per `reference/bookorbit/docs/DEVELOPMENT.md`). All §4 decisions (A–F) are answered below with the binding server-side citation(s). Findings are also summarized in `docs/reverse-engineering-report.md` §9.

The §4 recommendations are left in place above as the "as-proposed" record; this section explicitly states where each was **confirmed as-is**, **modified by new evidence**, or **obsoleted by source inspection**. Per convention since Phase 5, where the server source contradicts an earlier inference, the server wins.

### 6.1 Method of resolution

This investigation was originally blocked on Decisions B and C precisely because the Lua plugin source was insufficient — we could see *what the plugin sends* but not *what the server accepts/refuses/returns*. The TS server source shipped under `reference/bookorbit/server/` closes that gap. Decisions B and (the original premise of) C were flagged for "live-probe" in the original doc; the source inspection here resolves them analytically, so the live probe is no longer a precondition for implementation of the client method. (The operator's `on_hold` Readest-side probe, which is unrelated to Decisions A–F, remains the only true live-probe item, per `future-status-sync.md §5`.)

### 6.2 Decision A — decisive-no-op `unread` and local bookkeeping

**Answer: Yes, with the two-field addition.**

Confirmed and slightly extended from the §4 recommendation: add **both** pairs to `internal/sync/state.MatchRecord` (currently four fields at `internal/sync/state/state.go:23-28`):
- `LastSeenStatus string` and `LastSeenStatusAt int64` — the last *seen* Readest decisive value (`unread` / `finished` / `abandoned` / `""` for non-decisive).
- `LastPushedStatus string` and `LastPushedStatusAt int64` — the last *pushed* BookOrbit token (`read` / `abandoned` / `""` for never-pushed or no-op).

The §4 doc proposed only the second pair. Why two pairs are needed:
- The `unread → finished` transition must compare the *new* Readest value against the previously-*seen* Readest value, not against the previously-*pushed* BookOrbit token, otherwise "previously seen `unread` (non-decisive, no-op)" looks indistinguishable from "never observed" (zero value), causing the transition to be missed.
- The `finished → unread` transition has nothing correct to diff on the BookOrbit-token side either: BookOrbit's `read` was the last pushed token; the new Readest value is `unread`; the correct local action is "no BookOrbit call, *but* record locally so the next `unread → finished` is correctly detected."

Pure local bookkeeping; no API surface change. The two new field pairs are additive to the `MatchRecord` schema, slotting in next to the existing `LastPushedAt`/`LastPushedPct` exactly the way `docs/planning.md §7`'s last paragraph anticipated ("Design the `sync/state.go` schema so it could store BookOrbit's own timestamp per book").

**Test impact:** extend the existing `exerciseStore` helper in `state_test.go` (already the canonical place where additive `MatchRecord` field changes are pinned) — see §3.4 of the original investigation doc.

### 6.3 Decision B — device-scoped fields, response shape — RESOLVED BY SOURCE

**Answer: The live-probe item is closed analytically. The known shape is documented below.**

Server evidence at `reference/bookorbit/server/`:

- **No device-scoped fields required.** The DTO `KoreaderCatalogSetReadStatusDto` at `server/src/modules/koreader/dto/koreader-catalog-query.dto.ts:245-248` declares only a `status` field. The global `ValidationPipe` runs with `forbidNonWhitelisted: true` (`server/src/main.ts:64-70`), so sending `deviceId`/`deviceModel`/`pluginVersion`/`deviceTime` (which the bridge already sends on `MatchCheck`/`BulkProgress`) would be actively **rejected with 400**, *before* the service method runs. The §4 doc's "assumption (no device-field wrapping)" was correct; the §3.3 placeholders (`SetReadStatus` request-shape unit tests) can now write the asserted request shape with confidence: `{"status": "read"}`, nothing else.
- **Success response is `200 {"readStatus": "<token>"}`.** Service method at `server/src/modules/koreader/koreader-catalog.service.ts:275-279`: `return { readStatus: status }`. Return type at `packages/types/src/koreader.ts:370-372`. No `startedAt`/`finishedAt`/`updatedAt` echoed, no body normalization beyond the token. The controller returns the promise with no `@HttpCode` override, so the default `200` applies for `@Put` (verified by `controller.test.ts:47` and `koreader-catalog.service.test.ts:867`).
- **Server-side edge case to be aware of:** the response echoes the *requested* token, but the server's projection in `reading-attempt.service.ts:114` can persist a different token for one case (`reading` on a previously-completed book projects to `rereading` in the DB). This doesn't affect the bridge because Readest `reading` is non-decisive and always skipped, but it is documented for completeness.

The §3.3 placeholder rows contingent on "Decision B" — *malformed/oversized response body handling*, *context cancellation mid-request* — can now be committed as real test cases. The request shape is known.

**No live probe needed for Channel B's request/response contract.** (The operator's `on_hold` probe — `future-status-sync.md` §5 — is unrelated to Decision B and remains a Phase-8 item; this decision is purely about the contract for the tokens we do know.)

### 6.4 Decision C — 404/405 classification — REFRAMED AND ANSWERED

**Answer: Recast.** The §4 doc's premise (treat 404/405 as "feature unsupported on this BookOrbit version, soft-disable") is **invalidated by source inspection.** Channel B is a first-class, always-present route in the same controller that ships `bookDetail` (`server/src/modules/koreader/koreader-catalog.controller.ts:24`: `@Controller('koreader/plugin/catalog')`). There is no "older BookOrbit build without Channel B" to fall back from. The "soft-disable on older BookOrbit" logic is therefore unreachable; 405 should never occur for theChannel B path in a sane deployment.

What the server actually returns on this endpoint (cited in §9.5 of `docs/reverse-engineering-report.md`):

| Failure | HTTP | What it means to the bridge |
|---|---|---|
| Bad/missing `x-auth-user`/`x-auth-key`, account inactive, `KoreaderSync` permission revoked | 401 | Classify as `ErrUnauthorized` (terminal, already handled uniformly with the other three methods) |
| Real book, library access denied for the KOReader-authenticated user | 403 | `ErrUnauthorized` (per Phase 5 §10's existing classification — BookOrbit's own sentinels treat 401 and 403 together under `ErrUnauthorized`, since both are terminal auth failures) |
| Book deleted from user's library, **or** book filtered out by content filters (both reported as 404 deliberately, `book.service.ts:472-481`) | **404** | The cached `bookId` is **stale** |
| Bad `status` token, extra body field, non-integer `bookId` | 400 | `ErrBadRequest` (non-retryable) — would indicate a bridge logic bug |
| Truly other | 500 | `ErrServer` (retryable) |

**The correct classification of a Channel-B 404 is "drop this book from the status-sync set for this hash" — not "feature unsupported" and not "watermark retreat."** This mirrors exactly the engine's existing treatment of a `BulkProgress` response listing a previously-matched hash as `unmatched` (Phase 6 §6.3): `state.DeleteMatch(hash)` + `state.SetUnmatched(hash, now)`, watermark advances normally (NOT retreated), and the `UnmatchedCooldown` recheck gate keeps the bridge from hammering a lost book.

This is the recommendation confirmed by the operator: a Channel-B 404 → drop from sync set. Documented behavior:

1. On `SetReadStatus` returning an error classified as "book gone" (introduce a small NEW sentinel `bookorbit.ErrBookGone` keyed off HTTP 404 specifically, OR reuse `ErrBadRequest` and classify on the caller side — see "Mechanism" below), the engine:
   - Calls `state.DeleteMatch(hash)` (defensive; normally the match was deleted by BookOrbit already but a stale local `Match` may remain).
   - Calls `state.SetUnmatched(hash, now)` so the BookOrbit `UnmatchedCooldown` recheck gate calms future polls.
   - **Does NOT** append the row's `WatermarkMs` to `failedWatermarks` — the watermark advances normally (Phase 6 §5.4).
   - **Does NOT** retreat the BookOrbit-bridge's bookkeeping `LastSeenStatus`/`LastPushedStatus`: the entries still set from the prior match become irrelevant once the local `Match` is deleted, and the next match resolution will start fresh.
2. On a Class-B 400 (`ErrBadRequest`): non-retryable per Phase 5 §10's existing convention. The bridge treats it as a one-off log+skip for this hash on this poll; it's a logic bug if it ever fires, so the log is a `WARN` with the status token and bookId attached for diagnosis.
3. On 401/403 (`ErrUnauthorized`): existing terminal auth-abort path (Phase 6 §6.3 and §11), unchanged.
4. The "soft-disable for the remainder of the run" mechanism from the §4 recommendation is **not used** — it was a workaround for a case that doesn't exist for Channel B.

**Mechanism (decision for implementation).** Two equivalent options:

- **(Mech-α)** Add a new sentinel `bookorbit.ErrBookGone` to `internal/bookorbit`, classified from HTTP 404 (and only HTTP 404) in `classifyErrorResponse`. The engine's status step inspects the returned error with `errors.Is(err, bookorbit.ErrBookGone)` and performs the drop-from-sync-set action.
- **(Mech-β)** Reuse `bookorbit.ErrBadRequest` for 400 *and* 404 on Channel B (both are non-retryable; in practice the engine rarely cares about the distinction in code paths other than this one), and have the engine's status step call a small helper `classifyStatusPushErr(err)` that returns one of `drop`, `retry`, `fatal`, `skip-per-poll`. This keeps the client's sentinel surface unchanged.

Either mechanism supports the chosen behavior; the choice is purely whether the client grows a new sentinel. **Recommend Mech-α** for symmetry with the existing taxonomy (`ErrUnsupportedEndpoint` exists specifically so the engine can key off it; an `ErrBookGone` for the same reason is consistent). This is an implementation detail outside the scope of this design investigation, but flagged so the implementation can decide during code review.

**Test impact:** the §3.3 placeholder row for "404/405 → classified the same way UpdateProgress's `unsupported endpoint` case is" is replaced by three new committed test cases: (a) 404 → `ErrBookGone` (or whatever sentinel Mech-α/β settle on) and the engine performs drop-from-sync-set; (b) 400 → `ErrBadRequest`, non-retryable; (c) 403 → `ErrUnauthorized` terminal abort. The 405 and 501 fallthrough to `ErrUnsupportedEndpoint` retains its Phase-5 semantics but is unreachable here in practice; the test for it stays a pure classification-row test in the client suite, not an engine-behavior test.

### 6.5 Decision D — unrecognized Readest status, log-or-skip

**Answer: Log at `warn`, once-per-value (in-memory rate-limit), per the operator's confirmation.**

Confirmed exactly as §4 recommends. The set of accepted `status` *tokens* on the BookOrbit server is irrelevant to this decision — this decision is about *Readest-side* schema drift on the `reading_status` field the bridge reads. Concretely:

- The bridge maintains a per-process in-memory set of already-warned unrecognized Readest `reading_status` tokens (started on engine initialization, lives in the `Engine` struct, not in `state.Store`).
- On the first time a previously-unseen unrecognized token appears in a `BookRow.ReadingStatus`, emit one `WARN sync: unrecognized readest reading_status value` log line with the value and book hash attached, then add the token to the set.
- On subsequent occurrences of the same token, silent (no logging).
- The set is process-lifetime only; a restart re-warns once for any token still appearing. This matches the existing per-source deduping pattern (Phase 6 ADR Addendum 2) and is the smallest state addition.
- Returns `("", false)` from `mapReadingStatus` ("fail safe, skip") *and* does NOT update `LastSeenStatus`/`LastPushedStatus` — a token we don't understand is not a status we can claim to have processed.

This is purely a logging/discipline decision, no API change. The §3.1 table row for "unrecognized/future string → `("", false)`, not an error" in the original doc stands; this decision adds only "and log it once."

**Test impact:** a couple of new unit cases: (a) an unrecognized token yields `("", false)` from `mapReadingStatus` (already in §3.1); (b) at engine level, an unrecognized token for the first time emits exactly one `WARN`, and for the second time the test asserts zero further log lines (capture via a `slog` test handler). Both are additive to the existing `engine_test.go` style.

### 6.6 Decision E — status-failure coupling with the progress watermark

**Answer: (b) Decouple them — status failure is isolated to the status step.**

Confirmed against §4's option (b), with concrete server-side justification beyond the §4 "philosophy":

- **Channel B is per-book HTTP**, one call per matched book per poll, not a batched single call per 100 like `BulkProgress`. The cost of treating a one-book status failure as a *per-batch* watermark retreat is mismatched to the actual surface. A transient 500 on one book should not re-pull and re-validate progress for *every* book on the next cycle's READ-side cursor.
- The decisive-status side channel is **semantically independent** of progress: progress is a `percentage` float on `reading_progress`; status is a `status` enum token on `user_book_status`. They live in different tables (`koreader.repository.ts:536-562` for `reading_progress`, `user_book_status` projected from `reading_attempts` — see §9.6 of `docs/reverse-engineering-report.md` for the transaction/locking distinction). Decisions on one should not stall the other.

The concrete engine rules for `SetReadStatus` failure classification:

| `SetReadStatus` outcome | Engine action for this book |
|---|---|
| Success (2xx) | Update `MatchRecord.LastSeenStatus`, `LastSeenStatusAt`, `LastPushedStatus`, `LastPushedStatusAt` to the current poll's values. Progress watermark/state unchanged. |
| `ErrBookGone` (HTTP 404, Decision C) | Drop-from-sync-set (§6.4's `DeleteMatch` + `SetUnmatched`). Progress state untouched — including its own advances this poll. |
| `ErrUnauthorized` (HTTP 401/403) | Terminal auth abort (Phase 6 §6.3/§11) — abort the rest of `RunOnce`, preserve prior mutations, still call `Save()`. Same as auth aborts on `BulkProgress`. |
| `ErrBadRequest` (HTTP 400) | Non-retryable per-book warning + skip; **do not** advance `LastPushedStatus`; **do not** retreat the progress watermark. Treat as a bridge logic bug to investigate. |
| `ErrServer` / `ErrNetwork` / `ErrRateLimited` (transient, retryable) | Advancing-plus-`withRetry`(Phase 6 §7) eventually exhausts attempts; if still failing, treat as "skip status update for this book this poll": **do not** advance `LastPushedStatus`, **do not** retreat the progress watermark. On the next poll the next `mapReadingStatus` call will see that the decisive value still differs from `LastPushedStatus` (since `LastPushedStatus` was NOT advanced), and retry. |
| `context.Canceled` (shutdown) | Standard propagation — abort this step, let `RunOnce`'s caller handle. Matches the existing engine behavior. |

The §3.5 placeholder row for "A `RunOnce` with several matched books, mixed statuses... → one push, one skip, one push, one skip" can now be finalized without the "pending Decision E" caveat.

**One additional requirement surfaced by source inspection:** the bridge must keep its `device`/`deviceId` distinct from `bookorbit-web` (per `koreader.repository.ts:559`'s deliberate `updatedAt`-preservation semantics). This is already true for the *progress* path per the shipped Phase 6 bridge; the *status* path is **per-book**, no `deviceId` is sent in the Channel B body (per Decision B/§6.3), so the requirement is automatically met for status.

**Test impact:** the §3.5 placeholders ("`SetReadStatus` returns an error → that book's state is left unchanged" and the "(a) vs (b)" watermark-interaction half) are finalized as cases (a) "no partial `LastPushedStatus` update on failure" and the new (b) "watermark does NOT retreat on a status-push failure, and progress's `LastPushedPct` DOES advance when progress succeeded". A few new mixed-progress-+-status-failure scenarios should be added to the multi-book engine tests.

### 6.7 Decision F — config toggle, default-off

**Answer: Yes, default-off for v1.**

Confirmed exactly as §4 recommends, with concrete justification from `docs/implementation-brief.md` (line 39–40 explicitly states "No status sync" as a constraint of the current scope). A future release that turns status sync on should therefore require explicit opt-in:

- Add one `bool` field `Bridge.SyncStatus` (or `SyncStatusEnabled`) to `internal/config` per the existing `config.Bridge` pattern. Default `false`.
- Add one `BRIDGE_SYNC_STATUS` env override, mirroring the existing `BRIDGE_*` convention.
- `Config.Validate()` treats it as a no-op case (no cross-field invariants).
- The engine's status step (Phase 6 §4 of this design doc's §2.5 proposal, the `push-status` step in `RunOnce`) is gated behind this bool — `if e.cfg.Bridge.SyncStatus { /* run step */ }`.

This is structurally identical to how the existing `config.Bridge` booleans (e.g., `EnableHTTPS`) work and adds no schema disruption to Phases 1–7.

**Test impact:** a small unit test asserts that the engine's `RunOnce` skips the status step entirely when `SyncStatus` is false (and that the step runs when true). Additive to the existing `engine_test.go` matrix.

### 6.8 Decisions not made here (flagged as live-probe items that genuinely remain)

Only one live probe genuinely survives this review, and it is `on_hold`-specific (not part of Decisions A–F):

- **`on_hold` mapping** — Readest's web "Mark on hold" UI button may or may not write *anything* to the cloud-synced `reading_status` field. The Readest plugin source (`reference/readest.koplugin/library/readingstatus.lua`, `librarystore.lua`) carries no `on_hold` token anywhere (verified by `grep on.?hold|paused|want_to_read|re.?reading|skimmed` returning zero matches across the Readest plugin source tree). The probe is: trigger "Mark on hold" in the Readest web UI on a test book, then `GET /sync?type=books&since=0` against the operator's real account, and inspect the row's `readingStatus` field. This remains a Phase-8-style live-validation item per `future-status-sync.md §5`, and it gates any future extension of the bridge's status-sync feature to cover `on_hold` — not the implementation of the four additive touch-points proposed in §2.

### 6.9 Consolidated decisions table (single-glance summary)

| Decision | §6 answer | Server-side citation anchoring it |
|---|---|---|
| A — record `unread` in local bookkeeping? | **Yes, two pairs of fields** (`LastSeenStatus`/`At` + `LastPushedStatus`/`At`) | `internal/sync/state/state.go:23-28` (existing `MatchRecord`); `docs/planning.md §7` last paragraph |
| B — Channel B body shape, response? | **`{"status": token}` only (no device fields); 200 `{"readStatus": token}`** | `koreader-catalog-query.dto.ts:245-248`, `main.ts:64-70` (`forbidNonWhitelisted: true`), `koreader-catalog.service.ts:275-279`, `packages/types/src/koreader.ts:370-372` |
| C — 404/405 classification? | **404 = "drop from sync set" (deleteMatch+setUnmatched, no retreat); 400 = ErrBadRequest; 401/403 = ErrUnauthorized; 405 unreachable** | `koreader-catalog.controller.ts:24,81-84` (route is first-class); `book.service.ts:472-481` (404 source); `library.service.ts:63-67` (403 source); `koreader-auth.guard.ts:13-58` (401 source) |
| D — unrecognized Readest status? | **Log once-per-value at `warn` (in-memory rate limit); skip (`("", false)`)** | N/A (Readest-side schema drift, no server-side relevance) |
| E — status failure coupling with progress watermark? | **Deouple — status failure does NOT retreat the progress watermark; advances `LastPushedStatus` only on 2xx** | `koreader.repository.ts:536-562` (separate `reading_progress` table; status is on `user_book_status`), `reading-attempt.repository.ts:85-111` (status write path is distinct) |
| F — config toggle, default-off? | **Yes, `Bridge.SyncStatus` bool, default false, `BRIDGE_SYNC_STATUS` env override** | `docs/implementation-brief.md` (line 39–40: "No status sync" as current-scope constraint); existing `internal/config` patterns |

---

*Added 2026-07-31 after a source-inspection pass of `reference/bookorbit/server/` (NestJS Fastify + Drizzle). Cross-referenced with `docs/reverse-engineering-report.md` §9 for the same evidence set.*
