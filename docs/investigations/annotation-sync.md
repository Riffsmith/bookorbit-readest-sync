# Feasibility: Syncing Annotations/Highlights + Progress + Status

**Scope:** One-way Readest → BookOrbit, Readest as source of truth, annotations/highlights included alongside progress and status. This document extends the shipped progress-only bridge (Phases 0–8, see phases/phase-09-status-sync/ for status) to cover **annotation sync**.

**Date:** 2026-08-01

---

## 1. Executive Summary

**Feasible, but non-trivial.** The bridge can push annotations from Readest into BookOrbit without touching book files, but annotation sync is **an order of magnitude harder than progress or status sync** because it must:

1. Pull annotations from Readest's per-book `configs` API (not the bulk `books` API that carries progress/status).
2. Convert Readest's xpointer-based positions (KOReader crengine format) to BookOrbit's internal `annotation_positions` model, which expects KOReader-native xpointers and has a full position-conversion subsystem (`xpointer ↔ CFI ↔ pdf`).
3. Respect BookOrbit's **annotation-exchange protocol** (two-phase: ingest + ack), which requires the client to track per-annotation identity datetimes, manage watermarks, and handle three apply modes (live/sidecar/skip) plus deletion detection.
4. Handle color/style/drawer mappings across three distinct representation systems (Readest 6-color palette, KOReader BlitBuffer 8-color hex palette, BookOrbit 3-color + style enum).

**Recommendation:** Ship Phase 9 (status sync) first — it's almost free compared to annotations (three additive touch-points, zero position resolution) and delivers the most user-visible value. Annotations should be **Phase 10**, a separate design investigation with its own implementation plan.

| Feature                    | Effort    | Value | Risk            |
| -------------------------- | --------- | ----- | --------------- |
| Progress sync (shipped)    | Done      | High  | Low             |
| Status sync (Phase 9)      | Small     | High  | Low             |
| Annotation sync (Phase 10) | **Large** | High  | **Medium-high** |

---

## 2. How Readest Sends Annotations

### 2.1 The API contract (confirmed from plugin source and live curl)

The Readest sync API has **one** endpoint, `GET/POST /api/sync`, with a `type` discriminator:

| `type`    | Method                  | Direction     | What it carries                                                                  |
| --------- | ----------------------- | ------------- | -------------------------------------------------------------------------------- |
| `books`   | GET                     | Pull          | Book metadata + `progress` tuple + `reading_status` (used by the shipped bridge) |
| `configs` | GET                     | Pull per-book | `book_configs` rows: exact resume position as xpointer + `progress` tuple        |
| `configs` | POST                    | Push per-book | Same shape                                                                       |
| `notes`   | GET                     | Pull per-book | `notes` array: annotations/bookmarks/highlights                                  |
| `notes`   | POST                    | Push per-book | `notes` array: annotations/bookmarks/highlights                                  |
| `notes`   | (implicit in full pull) | —             | **NOT** returned by `type=books` — confirmed by live curl                        |

**The user's live curl confirmed:** `GET /sync?type=books&since=0` returns `books: [...], configs: [], notes: [], statBooks: [], statPages: []` — progress is in `books`, but **notes are always per-book**, never in the bulk pull.

### 2.2 The per-book note pull procedure (from `readest_syncannotations.lua:332-337`)

> **Live-verified (2026-08-01):** `GET /api/sync?type=books&since=0` returns `books` populated but `configs: []`, `notes: []`, `statBooks: []`, and `statPages: []`. The bulk pull carries only the books table — notes and configs are **always per-book**, never in the bulk response. This is the contract the live service enforces.

```
GET /api/sync?type=notes&book=<book_hash>&meta_hash=<meta_hash>&since=<ms>
Authorization: Bearer <supabase_access_token>
```

**Response shape** (inferred from the plugin's `readest_syncannotations.lua:358-486` consumer and `library-design.md` notes schema):

```json
{
  "notes": [
    {
      "id": "<md5-7-char>",
      "bookHash": "<partial-md5>",
      "metaHash": "<partial-md5>",
      "type": "annotation" | "bookmark",
      "xpointer0": "/body/DocFragment[2]/p[14]",
      "xpointer1": "/body/DocFragment[2]/p[16]",
      "text": "highlighted passage",
      "note": "user comment",
      "style": "highlight" | "underline" | "squiggly",
      "color": "yellow" | "red" | "green" | "blue" | "violet" | "#ff8800" | "#00bcd4" | "#808000" | "#9e9e9e",
      "page": 42,
      "createdAt": "2026-07-31T14:22:11.954+00:00",
      "updatedAt": "2026-07-31T14:25:00.000+00:00",
      "deleted_at": "2026-07-31T14:30:00.000+00:00" | null
    }
  ]
}
```

**Push shape** (from `readest_syncannotations.lua:275-279`):

```json
{
  "books": [],
  "notes": [
    {
      "bookHash": "<partial-md5>",
      "metaHash": "<partial-md5>",
      "id": "<md5-7-char>",
      "type": "annotation" | "bookmark",
      "xpointer0": "...",
      "xpointer1": "...",
      "text": "...",
      "note": "..." | null,
      "style": "highlight" | "underline" | "squiggly",
      "color": "yellow" | "red" | ... | "#ff8800" | ...,
      "page": 42,
      "createdAt": <unix-ms>,
      "updatedAt": <unix-ms>
    }
  ],
  "configs": []
}
```

### 2.3 Key Readest-side facts

1. **Positions are KOReader-style xpointers**, not CFIs, not PDFs. The xpointer format `/body/DocFragment[N]/p[M]` is used for both start and end of a highlight range. The `page` field is a KOReader page number for the bookmark-type notes.
2. **Bookmarks are a separate note type** with a single `xpointer0`, no `xpointer1`, no `style`/`color`, and `type = "bookmark"`.
3. **Note IDs are deterministic MD5s** of `"ko:" .. book_hash .. ":" .. note_type .. ":" .. pos0 .. ":" .. (pos1 or "")`, truncated to 7 characters. These IDs are stable across pushes (same inputs → same ID), which is how the KOReader plugin dedupes on re-push and matches tombstones.
4. **Deletions are tombstones** — `deletedAt` is set on the note, and the note is pushed with the tombstone so the server drops it. This is the KOReader-plugin pattern (not the Readest-native pattern, which the bridge doesn't need to mirror pixel-for-pixel).
5. **Text is auto-extracted from the xpointer range** by crengine's `getTextFromXPointers` on the client side — the bridge does NOT have this capability without opening a book. This is a **hard dependency on the KOReader plugin** for live position resolution.

---

## 3. How BookOrbit Receives Annotations

### 3.1 The exchange protocol (from `koreader-annotation-exchange.service.ts` + DTOs)

BookOrbit's annotation sync is **two-phase**, not one-shot:

**Phase 1 — Ingest (client → server):**

```
POST /koreader/plugin/annotations/exchange
Authorization: x-auth-user / x-auth-key
Content-Type: application/json

{
  "deviceId": "<stable-uuid>",
  "deviceModel": "readest-bridge",
  "pluginVersion": "0.1.0",
  "deviceTime": "2026-08-01 14:30:00",
  "books": [
    {
      "hash": "<partial-md5>",
      "keys": [
        { "k": "<md5(datetime|pos0)>", "dt": "2026-07-31 14:22:11" }
      ],
      "keysComplete": true | false,
      "changes": [
        {
          "datetime": "2026-07-31 14:22:11",
          "datetimeUpdated": "2026-07-31 14:25:00",
          "drawer": "lighten" | "underscore" | "strikeout" | "invert",
          "color": "yellow" | "red" | "green" | ... | "#6d28d9",
          "text": "highlighted text",
          "note": "user comment" | null,
          "chapter": "Chapter 1",
          "pageno": 42,
          "posFormat": "xpointer",
          "pos0": "/body/DocFragment[2]/p[14]",
          "pos1": "/body/DocFragment[2]/p[16]"
        }
      ]
    }
  ]
}
```

**Phase 1 response:**

```json
{
  "results": [
    {
      "hash": "<partial-md5>",
      "bookId": 123,
      "applied": {
        "created": 5,
        "updated": 2,
        "moved": 0,
        "unchanged": 10,
        "skippedDeleted": 1,
        "deviceDeleted": 2
      },
      "toApply": {
        "add": [ ... ],    // annotations the server wants to push down (web-created)
        "edit": [ ... ],   // annotations edited on web since last sync
        "delete": [ ... ]  // annotations deleted on web since last sync
      },
      "more": false,
      "skippedNoPosition": 0
    }
  ],
  "unmatched": [ "<hash>" ]
}
```

**Phase 2 — Ack (client → server):**

```
POST /koreader/plugin/annotations/exchange-ack

{
  "deviceId": "...",
  "books": [
    {
      "hash": "...",
      "applied": [
        {
          "serverId": 456,
          "version": 2,
          "status": "applied" | "failed",
          "verified": true,
          "corrected": false,
          "pos0": "...",
          "pos1": "...",
          "pageno": 42,
          "datetimeUpdated": "2026-07-31 14:30:00"
        }
      ],
      "deleted": [
        { "serverId": 789, "status": "applied" }
      ]
    }
  ]
}
```

### 3.2 The two endpoint options

BookOrbit actually provides **two** annotation endpoints, and the choice matters:

**Option A — Exchange (two-phase) — current canonical:**

```
POST /koreader/plugin/annotations/exchange
POST /koreader/plugin/annotations/exchange-ack
```

This is what the BookOrbit plugin ≥0.4 uses. It's bidirectional (server can push web-created annotations down), tracks per-annotation identity for dedup/deletion, and is BookOrbit's maintained annotation-sync path. Complex but complete.

**Option B — Legacy one-way upload — simplest:**

```
POST /koreader/plugin/annotations
```

Called from `bookorbit_plugin.annotation.service.uploadAnnotations` (service at `koreader-plugin-annotation.service.ts:26-80`). This is the deprecated path used by plugin 0.3.x. Same DTO shape, same server-side `AnnotationSyncService.ingestDeviceAnnotations`, but:

- **No ack phase** — single POST, no push-down, no exchange-ack.
- **No deletion detection** — no `keys`/`keysComplete` bookkeeping, no tombstone tracking.
- **No server push-down** — no `toApply` response; appropriate for a one-way bridge.

The legacy endpoint is marked "Deprecated" but still present and functional. For a bridge MVP (one-way, Readest → BookOrbit annotations), Option B is the **much simpler path**: one POST per book-chunk instead of the two-phase exchange dance. Deletion support is the trade-off — the bridge can add exchange-based deletion detection later (Phase 10b) once the MVP is stable.

### 3.3 The BookOrbit internal annotation model

From `db/schema/reader.ts:345-466` and `annotation-sync.service.ts`:

**Canonical annotations table** (per user, per book):

- `id` — serial primary key
- `text` — highlighted passage (required, NOT NULL)
- `color` — hex string (default `#FACC15` = yellow)
- `style` — enum: `highlight | underline | strikethrough | squiggly | invert`
- `note` — text, nullable
- `chapterTitle` — varchar(500), nullable
- `origin` — enum: `web | koreader | kobo`
- `version` — integer, bumped on every content mutation
- `deletedAt` — soft delete timestamp (tombstone)
- `deviceCreatedAt` — **identity datetime**, the datetime the device first saw this annotation. Used as the KOReader-side dedup key. Wall-clock `"YYYY-MM-DD HH:MM:SS"` with no timezone.
- `deviceUpdatedAt` — same format, updated on every device edit

**Annotation positions table** (one row per format per annotation):

- `format` — enum: `cfi | xpointer | pdf | kobo_span`
- `pos0`, `pos1` — position text
- `status` — enum: `exact | repaired | failed | pending`
- `converterVersion` — which version of the position converter produced this

**Annotation sync state table** (per device per book):

- `externalKey` — `md5(deviceCreatedAt | pos0)` — the device-side dedup key
- `lastAppliedVersion` — tracks what's been delivered to this device
- `deleteAckedAt` — tombstone acknowledgment

### 3.3 Position conversion: the hardest part

BookOrbit's `position-converter.service.ts` implements bidirectional xpointer ↔ CFI conversion by parsing the EPUB's HTML DOM server-side. The exchange logic:

1. When ingesting an annotation with `posFormat: "xpointer"` (from KOReader), the server stores the xpointer as-is and can later convert it to `cfi` via `xpointerToCfi()`.
2. When pushing down to a device, if the annotation only has a `cfi` position (from a Kobo-device upload), the server **must convert it to xpointer** before sending, because KOReader's crengine only understands xpointers. This conversion is done by `cfiToXpointer()`, which searches the EPUB's DOM for the highlighted text near the CFI position.
3. If conversion fails, the annotation is skipped (`skippedNoPosition` counter) and pushed as a `pending` position with an empty pos0.

**For the bridge** — since Readest xpointers are already in KOReader-compatible format (both use `/body/DocFragment[N]/...`), the bridge can pass them through as-is, `posFormat: "xpointer"`.

---

## 4. The Gap: Why This Is Hard

### 4.1 The per-book loop

Progress sync pulls **all books in one request** (`GET /sync?type=books&since=<watermark>`). Annotation sync **cannot** — it requires one `GET /sync?type=notes&book=...&meta_hash=...&since=...` **per book**. With a library of 350 books, that's 350 HTTP requests per full sync cycle, and each response could contain hundreds of annotations.

The bridge must:

1. First pull `books` (which it already does) to enumerate the user's library.
2. Then, for every book with a non-null `meta_hash`, issue a separate `type=notes` pull.
3. Batch the pushes to BookOrbit — the exchange DTO caps at `MAX_CHANGES_PER_REQUEST = 50` annotations per request and `20` books per request.

### 4.2 The identity problem

**The single hardest sub-problem.** Readest and BookOrbit use completely different annotation identity schemes:

|                      | Readest                                                                          | BookOrbit                                                                                             |
| -------------------- | -------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| **Identity**         | Deterministic MD5-7 of `"ko:" + bookHash + ":" + type + ":" + pos0 + ":" + pos1` | Auto-increment integer PK + per-device `deviceCreatedAt` + `md5(deviceCreatedAt + pos0)` external key |
| **Dedup key**        | Note ID (7-char MD5)                                                             | `externalKey = md5(deviceCreatedAt                                                                    | pos0)` |
| **Position**         | xpointer (KOReader format)                                                       | xpointer / CFI / PDF / kobo_span                                                                      |
| **Timestamp format** | Unix milliseconds (numeric)                                                      | Wall-clock `"YYYY-MM-DD HH:MM:SS"`                                                                    |

The bridge must:

1. For each Readest note, synthesize a `deviceCreatedAt` that is **stable** — the same Readest note must map to the same BookOrbit annotation across every poll cycle, or you get duplicates. Deriving it from Readest's `createdAt` (a millisecond epoch) converts to the same wall-clock string every time for the same note.
2. Convert Readest's `createdAt` (ms epoch) to `deviceCreatedAt` ("YYYY-MM-DD HH:MM:SS") deterministically.
3. Compute BookOrbit's `externalKey` as `md5(deviceCreatedAt + "|" + pos0)` to match the plugin's dedup logic (`bookorbit_annotations.lua:36`: `md5(deviceCreatedAt.."|"..pos0)`).
4. Track which Readest note IDs have already been successfully pushed, so polling with `since=<watermark>` doesn't re-push the same notes.

### 4.3 The watermark problem

The Readest per-book notes pull accepts `since` as a ms timestamp, but the response contains `createdAt`/`updatedAt` as ISO strings. The bridge must maintain a **per-book notes watermark** — NOT the same as the progress watermark, because notes change much less frequently than progress.

But there's a subtlety: the Readest sync API's `pullChanges` uses `since` as a **delta filter** — it returns only notes updated after that timestamp. The bridge needs to store, per book:

- The last `since` value used for notes
- The mapping of Readest note ID → BookOrbit annotation ServerID
- Whether a tombstone (deleted note) has been acked

### 4.4 The color/style mapping problem

Three different color systems must be reconciled:

**Readest colors** (7 named + 3 hex):

- Named: `yellow`, `red`, `green`, `blue`, `purple=violet`, `orange=#ff8800`, `cyan=#00bcd4`, `olive=#808000`, `gray=#9e9e9e`

**KOReader colors** (BlitBuffer HIGHLIGHT_COLORS):

- `yellow=#FACC15`, `red=#F87171`, `green=#4ADE80`, `blue=#38BDF8`, `purple=#F472B6`, `orange=#FF8800`, `cyan=#22D3EE`, `olive=#84CC16`, `gray=#9CA3AF`

**BookOrbit colors** (3 app colors + nearest-neighbor mapping):

- `ANNOTATION_HIGHLIGHT_COLORS`: `yellow=#FACC15`, `green=#4ADE80`, `blue=#38BDF8`, `purple=#F472B6`, plus fallbacks for everything else

The mapping **loses fidelity** in the bridge direction because Readest supports 9 distinct colors but BookOrbit's app palette has only 4 canonical values. The server uses `koreaderColorFromHex()` to pick the nearest chromatic neighbor for non-canonical hexes, which is defined in `annotation-style-map.ts`.

**Style mapping:**

| Readest     | KOReader drawer | BookOrbit style |
| ----------- | --------------- | --------------- |
| `highlight` | `lighten`       | `highlight`     |
| `underline` | `underscore`    | `underline`     |
| `squiggly`  | `strikeout`     | `squiggly`      |

### 4.5 The apply-mode problem (live vs. sidecar vs. skip)

The KOReader plugin's annotation exchange handles three apply modes depending on whether the book is currently open:

1. **Live** — the book is open in KOReader; annotations are verified against crengine (`isXPointerInDocument`, `getTextFromXPointers`), repaired if broken, and applied directly to the in-memory annotation list. The server sends corrections back and the client acks them.
2. **Sidecar** — the book is closed; annotations are merged into the doc settings file, marked as `annotations_externally_modified`, and KOReader revalidates on next open.
3. **Skip** — upload-only mode (used during page-turn sync); pending server changes stay queued because no ack is sent.

**The bridge has no live reader.** It cannot verify xpointers, cannot repair broken positions, and cannot ack with `verified: true`. It is permanently in "skip" mode for receiving changes (which is fine for one-way) and cannot do the live-verify-repair loop for sending changes.

### 4.6 The deletion detection problem

The server detects device-side deletions by asking: "Which annotations did this device have before that are NOT in the current `keys` list?" If a note is missing from the keys, it's soft-deleted server-side.

The bridge's "keys" list is synthesized from the Readest notes it just pulled. But Readest's `since` filter means the bridge only sees **changed** notes, not all notes. The bridge would need to maintain a **per-book full annotation state cache** to correctly compute the keys-complete list for deletion detection. This is a significant state-tracking burden.

---

## 5. Recommended Architecture

### 5.1 What the bridge should do (one-way, upload-only)

For each Readest book with `meta_hash`:

1. Pull notes: `GET /sync?type=notes&book=<book_hash>&meta_hash=<meta_hash>&since=<per-book-notes-watermark>`
2. For each note:
   - Skip if `deleted_at` is set (Readest-side tombstone; the bridge doesn't need to honor deletions in the BookOrbit direction because the bridge is not the source of BookOrbit annotations — Readest web edits to annotations don't flow from Readest directly; they flow from the KOReader plugin which does live delete detection)
   - Actually wait — **wrong.** Readest web and mobile DO allow deleting highlights. When deleted on the Readest side, the Readest cloud tombstones the note. The next `pullChanges` from the KOReader plugin (or from a bridge) will see the tombstone and must propagate it to BookOrbit. The bridge MUST handle deletions.
3. Convert each note to a BookOrbit `KoreaderAnnotationDto`:
   - `datetime` = convert `createdAt` ms → `"YYYY-MM-DD HH:MM:SS"`
   - `datetimeUpdated` = convert `updatedAt` ms → `"YYYY-MM-DD HH:MM:SS"`
   - `drawer` = map from Readest style → KOReader drawer
   - `color` = map from Readest color → KOReader color name (via `KO_TO_READEST_COLOR` reverse or from the note's raw color string)
   - `text` = note.text
   - `note` = note.note
   - `chapter` = null (Readest doesn't sync chapter)
   - `pageno` = note.page
   - `posFormat` = `"xpointer"`
   - `pos0` = note.xpointer0
   - `pos1` = note.xpointer1
4. Build `keys` array from ALL notes (not just changed ones) — this requires a **per-book full annotation cache** in the bridge's state store
5. Push to `POST /koreader/plugin/annotations/exchange` with `keysComplete: true`
6. Process the response's `toApply` section — for one-way sync, **ignore all server-pushed annotations** (they have no Readest representation; the bridge must not apply web-created annotations to Readest)
7. Send `exchange-ack` with `applied: []` for pushed-down changes (the bridge consumed none of them)

### 5.2 What the bridge must store (new state)

Beyond the existing `state.MatchRecord` fields, the annotation sync needs per-book:

```
AnnotationState:
  lastNotesWatermark      int64   — ms since-epoch for the notes pull cursor
  noteIDToServerID        map[string]int  — Readest note ID → BookOrbit annotation id
  lastSeenNotes           map[string]NoteSnapshot  — for keys-complete tracking
  notesToDelete           []string  — Readest note IDs tombstoned since last push
```

This is materially heavier than the current state schema, which tracks only `LastPushedPct` and `LastPushed*` for status.

### 5.3 What the bridge must NOT do

- **Never apply server-side changes to a local document** — the bridge has no document. Server-pushed annotations (`toApply.add`, `toApply.edit`, `toApply.delete`) are simply ignored. They're stored in BookOrbit's `annotation_positions` with status `pending` until a real KOReader device connects and acks them.
- **Never push a `keysComplete: true` list** on the first sync of a book — the bridge hasn't done a `since=0` full pull yet, so it doesn't know the full local annotation set. Wait, no — with a `since=0` full pull, the bridge DOES see the full set. So `keysComplete: true` is correct on the initial push too.
- **Never push `status: "failed"` in an ack** without a real reason — the bridge permanently can't verify positions, so it should skip the ack phase entirely for the one-way use case (or ack with `status: "applied"` and `verified: false`).

---

## 6. Feasibility Assessment

| Criterion                                                             | Assessment                                                                                                                                                                                                                                                    |
| --------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Can the bridge pull annotations from Readest?                         | **Yes** — `GET /sync?type=notes&book=...&meta_hash=...&since=...` is per-book but works. The shipped bridge already has Bearer auth.                                                                                                                          |
| Can the bridge push annotations to BookOrbit without a live document? | **Yes** — the exchange protocol has an explicit "skip" apply mode and accepts uploads from the client side. The server stores them with `origin: 'koreader'`.                                                                                                 |
| Can the bridge convert Readest note → KOReader annotation DTO?        | **Yes, with mapping loss** — colors are lossy (9 → 4 canonical values), style maps cleanly, positions pass through as xpointers.                                                                                                                              |
| Can the bridge handle deletions?                                      | **Yes, with extra state** — need per-book note tracking so tombstones can be generated when a note disappears from the Readest pull.                                                                                                                          |
| Can the bridge handle BookOrbit's position conversion?                | **Yes** — since Readest xpointers are already KOReader-compatible, the bridge just sets `posFormat: "xpointer"`. No CFI conversion needed on the bridge side. The server handles xpointer → CFI conversion internally if a PDF or Kobo device later needs it. |
| Can the bridge handle the identity/dedup problem?                     | **Yes, but it's the hardest part** — deterministic `deviceCreatedAt` + `externalKey` computation per note is required to avoid duplicates.                                                                                                                    |
| Does annotation sync work with the current one-way-only model?        | **Yes** — the bridge pushes Readest annotations to BookOrbit and ignores BookOrbit's push-downs (no live reader to apply them to).                                                                                                                            |

**Verdict: Moderate-to-hard.** Not because the APIs are secret or undocumented (they're both visible in the plugin sources), but because:

- **N requests per sync** (one per book) vs. 1 request for progress
- **Complex identity management** (deterministic keys to prevent duplicates)
- **Extra state store requirements** (per-book note tracking)
- **Color/style normalization** across three different systems
- **Deletion detection** requires full-key tracking

---

## 7. What Would Change vs. the Shipped Bridge

### 7.1 New packages

```
internal/
  readest/
    notes.go          — PullNotes(book_hash, meta_hash, since) → []NoteRow
    models.go         — NoteRow struct + JSON tags
  bookorbit/
    annotations.go    — ExchangeAnnotations(books []ExchangeBook) → ExchangeResponse
                      ExchangeAck(books []ExchangeAckBook) → ExchangeAckResponse
    models.go         — ExchangeBookDto, KoreaderAnnotationDto, etc.
  sync/
    annotations.go    — RunOnceAnnotations() orchestration
```

### 7.2 Extended state schema

```go
// Added to MatchRecord or new AnnotationState struct
type AnnotationState struct {
    // ... existing progress/status fields ...
    NotesWatermarkMs     int64            `json:"notesWatermarkMs,omitempty"`
    LastSeenNotes        map[string]NoteSnapshot `json:"lastSeenNotes,omitempty"`
    NoteIDToServerID     map[string]int   `json:"noteIDToServerID,omitempty"`
    NotesToDelete        []string         `json:"notesToDelete,omitempty"`
}

type NoteSnapshot struct {
    ReadestID      string
    ServerID       int    // BookOrbit annotation ID
    DeviceCreatedAt string // "YYYY-MM-DD HH:MM:SS"
    LastPushedHash  string // content hash to detect changes
}
```

### 7.3 New config knobs

```yaml
bridge:
  sync_annotations: false # opt-in like sync_status
  annotation_batch_size: 50 # MAX_CHANGES_PER_REQUEST
  annotation_books_per_req: 20 # max books per exchange request
```

### 7.4 The flow would become

```
[Poll timer]
  │
  ├─ 1. Pull books (existing) — progress + status
  │
  ├─ 2. For each book with meta_hash and notes watermark:
  │       Pull notes: GET /sync?type=notes&book=...&since=<per-book>
  │       Convert notes → KOReader annotation DTOs
  │       Detect tombstones (notes in state but not in pull)
  │
  ├─ 3. For each matched book:
  │       Batch ≤50 changes, ≤20 books per exchange request
  │       POST /koreader/plugin/annotations/exchange
  │       Send exchange-ack (empty applied list for one-way)
  │
  ├─ 4. Push progress (existing bulk path)
  │
  ├─ 5. Push status (Phase 9, if enabled)
  │
  └─ 6. Save state (existing)
```

---

## 8. Risks and Unknowns

| #   | Risk / Unknown                                                                                                                                                                                                                                                                                     | Evidence / Mitigation                                                                                                                                                                                                                                                        |
| --- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | **Per-book note pull amplifies request count.** 350 books × 1 request each = 350 requests per cycle. With 15-min polling, that's 1,400 req/hr. Readest's API may rate-limit.                                                                                                                       | Mitigation: stagger notes pull across polls (e.g., pull notes only every 4th poll cycle, or only for books whose progress changed). The `page` field on notes gives a cheap heuristic: if progress hasn't moved, notes probably haven't changed.                             |
| 2   | **Readest web deletion → tombstone → bridge → BookOrbit deletion.** If Readest web deletes a highlight, the note's `deleted_at` is set. The bridge must distinguish "note was deleted" (push tombstone) from "note wasn't in this delta pull (since filter)" (don't push).                         | Mitigation: the `since` filter on `pullChanges` returns all notes updated after the timestamp, including tombstones. Notes UPDATED before `since` are simply not returned — they stay in the bridge's state. Notes that appear with `deleted_at` set are tombstones to push. |
| 3   | **Color loss.** Readest web has no color picker (highlights are always yellow by default). All Readest-native highlights are `yellow`. Colors only become non-yellow when a KOReader device pushes them. Since the bridge will be Readest-only, all colors will be `yellow` — no loss in practice. | Verified: `KO_TO_READEST_COLOR` map only kicks in when the KOReader plugin pushes its own annotations. A bridge that only reads Readest sees only `yellow` unless a KOReader device also syncs.                                                                              |
| 4   | **Duplicate prevention across restarts.** The `deviceCreatedAt` must be derived deterministically from Readest's `createdAt` to prevent re-creating the same annotation on every poll.                                                                                                             | Deterministic derivation: `deviceCreatedAt = formatDeviceDatetime(createdAt_ms_epoch)`. Since we're deriving from a Readest-provided ms-epoch value, the result is stable.                                                                                                   |
| 5   | **BookOrbit ack-side corrections.** The server may send corrected xpointers in `toApply` when a pushed xpointer doesn't resolve. The bridge can't verify these (no live crengine).                                                                                                                 | Mitigation: ignore corrections. The server marks them `status: 'pending'` if the client doesn't ack with `corrected: true`. Other devices (a real KOReader) would fix them later.                                                                                            |
| 6   | **The exchange-ack is mandatory.** Even for one-way, the bridge must call `exchange-ack` or the server will re-queue the same push-downs forever.                                                                                                                                                  | Mitigation: always send `{ applied: [], deleted: [] }`, ack count is zero, no edits/deletes applied locally.                                                                                                                                                                 |
| 7   | **Readest notes may not have a `page` field.** `page` is populated by the KOReader plugin but might be null for web-created notes (no KOReader page concept in Readest web).                                                                                                                       | Mitigation: treat `pageno` as optional (nullable in DTO). The server stores it as `extras.pageno` if provided.                                                                                                                                                               |
| 8   | **Rate limiting on `/sync` POST.** Unknown. The KOReader plugin doesn't seem to have aggressive rate limiting, but the bridge could push large payloads.                                                                                                                                           | Mitigation: respect the `900KB` body-size limit per request (already enforced in the client). Split large batches.                                                                                                                                                           |

---

## 9. Dependency Graph

```
internal/readest/notes.go         — new: per-book notes pull client
internal/readest/models.go        — extend: NoteRow struct
internal/bookorbit/annotations.go — new: ExchangeAnnotations + ExchangeAck
internal/bookorbit/models.go      — extend: ExchangeBookDto, ExchangeAckBookDto, etc.
internal/sync/annotations.go     — new: annotation sync orchestration step
internal/sync/state/state.go      — extend: AnnotationState, NoteSnapshot
internal/config/types.go          — add: SyncAnnotations bool
internal/config/env.go           — add: BRIDGE_SYNC_ANNOTATIONS env var
internal/sync/engine.go          — extend: call annotations step if enabled
```

---

## 10. Recommended Phasing

Given the Phase 9 (status sync) design already established:

| Phase | What                                          | Effort                                                                                      | Value                         |
| ----- | --------------------------------------------- | ------------------------------------------------------------------------------------------- | ----------------------------- |
| 9     | Status sync (design done, ready to implement) | Small                                                                                       | High                          |
| 10    | Annotation sync — pull-only                   | **Medium** — pull notes from Readest, store in state, push to BookOrbit exchange as one-way | Medium                        |
| 10a   | Annotation sync — deletion detection          | **Medium-hard** — add per-book full note tracking, tombstone generation                     | Low (most users never delete) |
| 10b   | Annotation sync — ack protocol compliance     | **Small** — always ack with empty applied list                                              | Medium (protocol correctness) |
| 10c   | Local annotation cache (for keys-complete)    | **Hard** — persistent per-book note snapshot state                                          | Medium                        |

**Phase 10 can be decomposed:** start with the simplest feature (pull + push, no deletions, no ack), ship it behind a config flag, then incrementally add deletion detection and proper ack handling. The exchange protocol is designed for exactly this kind of incremental capability.

---

## 11. Bottom Line

**Syncing annotations is feasible, but it's a separate phase, not an afterthought.** The bridge can absolutely do it — the APIs are documented in the plugin sources, the DTOs are in the server code, and the position format is compatible.

**But it is not free.** The per-book nature of Readest's notes API, the identity/dedup problem, the state-tracking requirements for deletion detection, and the two-phase exchange protocol make this the single largest feature the bridge would carry. It deserves its own phase (Phase 10), its own ADR, and its own live-validation pass.

The recommended sequence is:

1. **Ship Phase 9 (status)** — three mappings, tiny touch surface, immediate user value
2. **Couple it with the annotation-pull-side scaffolding** — the bridge needs the per-book notes pull client anyway
3. **Ship Phase 10 (annotations)** once the notes pull is stable, with deletion detection as a follow-up

Each phase builds on the last: progress → status → annotations. All one-way, all Readest-authoritative, all additive to the existing engine loop.

---

_Prepared from: `reference/readest.koplugin/readest_syncannotations.lua`, `reference/readest.koplugin/spec/syncannotations_spec.lua`, `reference/readest.koplugin/readest-sync-api.json`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_annotations.lua`, `reference/koreader-plugin/bookorbit.koplugin/bookorbit_api.lua`, `reference/bookorbit/server/src/modules/koreader/koreader-annotation-exchange.service.ts`, `reference/bookorbit/server/src/modules/koreader/dto/koreader-exchange.dto.ts`, `reference/bookorbit/server/src/modules/koreader/dto/koreader-plugin.dto.ts`, `reference/bookorbit/server/src/modules/annotation/annotation-sync.service.ts`, `reference/bookorbit/server/src/modules/annotation/annotation-style-map.ts`, `reference/bookorbit/server/src/modules/position-converter/position-converter.service.ts`, and the project's existing [status-sync design documentation](docs/phases/phase-09-status-sync/design.md)._
