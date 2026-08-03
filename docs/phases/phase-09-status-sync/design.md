# Phase 9 — Status Sync: Authoritative Design Record

> **Superseded in one respect by [Phase 10](../phase-10-status-sync-decoupling/README.md).**
> This document's §4 row-classification gate — the "rows carrying `progress: null` fall out of
> the match-check queue before the status step ever sees them" assumption — was found defective
> against a live server on 2026-08-02. The row-classification loop was refactored in Phase 10 so a
> row with a decisive `reading_status` is status-eligible on its own merits. Everything else in
> this design (the mapping table, Channel B body shape, 404 classification, opt-in flag,
> warn-once discipline, status-failure decoupling from progress watermark) remains authoritative.

**Status:** implemented and verified. This document is the authoritative,
as-built record of the Phase 9 one-way Readest → BookOrbit reading-status
sync. It describes the design the shipped client implements; the
file-by-file account of how it was built and verified lives in
[./decision-record.md](./decision-record.md).

**Scope:** one-way Readest → BookOrbit reading-status sync
(`finished`/`abandoned` push; `unread`/`reading` no-op), extending the
progress bridge shipped in Phases 0–6. Two-way sync, Channel A
(`book-states`), and `on_hold` are out of scope (see §4).

**Provenance.** `docs/future-status-sync.md` did the reference-source
investigation. `docs/status-sync-design-investigation.md` §6 answered
Decisions A–F against the live BookOrbit server source (cross-referenced in
`docs/reverse-engineering-report.md` §9). Those six decisions are closed and
carried into this record as settled facts (§1). Decisions G and H
(`on_hold` deferral and the 404 sentinel mechanism) were resolved at
implementation approval and are recorded in §2 in their final form.

---

## 1. Settled decisions (A–F), as implemented

Each is anchored to BookOrbit server source and reflected verbatim in the
shipped code:

| #   | Decision                                                            | Answer (as implemented)                                                                                                                                                                                                                                                                                                                                                                              | Source anchor                                                                                                                  |
| --- | ------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| A   | Does `unread` need local bookkeeping even though it's a no-op push? | Yes — `MatchRecord` carries **two** field pairs, not one: `LastSeenStatus`/`LastSeenStatusAt` (last Readest value observed) and `LastPushedStatus`/`LastPushedStatusAt` (last BookOrbit token written). Without the "seen" pair, an `unread → finished` transition has nothing correct to diff against.                                                                                              | `internal/sync/state/state.go` (`MatchRecord`)                                                                                 |
| B   | Request/response shape for Channel B?                               | `PUT /koreader/plugin/catalog/books/{bookId}/read-status`, body **exactly** `{"status": "<token>"}` — no device wrapper. Success is `200` with a body of `{"readStatus": "<token>"}` that echoes the request; the persisted DB status may differ for the `reading → rereading` projection (`reading-attempt.service.ts:114`), which is unreachable here because Readest `reading` is always skipped. | `koreader-catalog-query.dto.ts:245-248`, `main.ts:64-70` (`forbidNonWhitelisted: true`), `koreader-catalog.service.ts:275-279` |
| C   | How to classify 404/405?                                            | 404 = **stale cached `bookId`** (book deleted from the user's library, or access revoked) — not "endpoint unsupported." Drop this book from the status-sync set: `DeleteMatch` + `SetUnmatched`, **no** watermark retreat. 405 is unreachable (Channel B is a first-class route, no legacy-server fallback exists).                                                                                  | `book.service.ts:472-481` (404 source), `koreader-catalog.controller.ts:24,81-84` (first-class route)                          |
| D   | Unrecognized Readest status value?                                  | Log `WARN` once per distinct value (in-memory, process-lifetime dedup set `Engine.warnedStatusValues`), then skip. Do not update `LastSeenStatus`/`LastPushedStatus` for an unrecognized value.                                                                                                                                                                                                      | N/A — Readest-side schema-drift concern, not server-verifiable                                                                 |
| E   | Does a status-push failure retreat the progress watermark?          | **No — decoupled.** Status and progress are different tables/write paths server-side (`reading_progress` vs. `user_book_status`), so a status failure never touches the progress watermark or `LastPushedPct`. A status failure just skips advancing `LastPushedStatus` for that book this poll; it's picked up again next poll since the mapped value still differs from `LastPushedStatus`.        | `koreader.repository.ts:536-562` vs. `reading-attempt.repository.ts:85-111`                                                    |
| F   | Config-gated, default on or off?                                    | Default **off**, opt-in. A status write changes what the operator's BookOrbit catalog displays, unlike progress (purely informational), so silent opt-in is the wrong default.                                                                                                                                                                                                                       | `BRIDGE_SYNC_STATUS` / `bridge.sync_status`, default `false`                                                                   |

---

## 2. The two open items resolved at approval (final form)

### Decision G — `on_hold` excluded from v1

**(G-1) Ship now, `on_hold` deferred.** Readest's web "Mark on hold" action
emits no `on_hold` enum value in `readingstatus.lua` / `librarystore.lua` /
`library-design.md`. The bridge maps only the source-verified tokens
(`finished`, `abandoned`) to a push and treats every other value — including
any future `on_hold` token — as the unrecognized-value path (Decision D):
warn-once, skip, no state corruption. Shipping therefore cannot mis-sync an
on-hold book; it can only leave its BookOrbit status unchanged.

The live probe that would settle what Readest's on-hold button writes to
`reading_status` (a manual `--once` plus `GET /sync?type=books&since=0`
against a real account, per `docs/future-status-sync.md` §5) requires no bridge
code and remains an open manual/live-validation item tracked in
`docs/live-validation-status.md`. It does not gate this feature.

### Decision H — 404 sentinel mechanism

**(Mech-α) A new exported sentinel `bookorbit.ErrBookGone`.** A 404 on Channel
B means "stale cached bookId" (semantically different from the 404 on the
progress endpoints, which means "endpoint unsupported"). `SetReadStatus`
intercepts `resp.StatusCode == http.StatusNotFound` first and returns
`ErrBookGone`, then delegates every other status (400/401/403/429/5xx/network)
to the unchanged shared `classifyErrorResponse`. The engine branches on
`errors.Is(err, bookorbit.ErrBookGone)` to drop the book from the sync set.
This is a single endpoint-specific branch layered on the shared classifier,
not a rewrite of it — the same pattern `ErrUnsupportedEndpoint` already
establishes (Phase 5 §10) for giving the engine something to `errors.Is`
against.

### Config field shape

`Bridge.SyncStatus bool` / yaml `bridge.sync_status` / env
`BRIDGE_SYNC_STATUS` / default `false`. It follows the project's existing
`config.Bridge` conventions, has no cross-field invariant for `Validate()`,
and is the first boolean config field — which is why `config/env.go` gained a
new `setBool` helper.

---

## 3. Implementation shape (as built)

Each package received one focused, additive change, following the layering the
project has used since Phase 4: models → client/data → state → engine →
config. No package boundary changed (`internal/readest` and
`internal/bookorbit` still never import each other; only `internal/sync`
imports both).

### 3.1 `internal/readest` — two new `BookRow` fields

`BookRow` gained `ReadingStatus string` (`json:"reading_status"`) and
`ReadingStatusUpdatedAt string` (`json:"reading_status_updated_at"`), stored
raw as ISO strings (not pre-converted to ms), matching the convention of the
existing `UpdatedAt`/`DeletedAt`/`SyncedAt`/`CreatedAt` fields. No new methods
were added: `mapReadingStatus` operates on the raw string, and because status
pushes are decoupled from the progress watermark (Decision E), the
reading-status timestamp is not used in watermark math. Both fields already
arrive on the existing `GET /sync?type=books` response the engine consumes
(`syncbooks.lua:168-169`), so `readest.Client.PullBooks` needed no change.

### 3.2 `internal/bookorbit` — one new client method, one new sentinel

`models.go` gained `SetReadStatusRequest{Status string}` and
`SetReadStatusResponse{ReadStatus string}`. `client.go` gained the sentinel
`ErrBookGone`, the `API` interface method `SetReadStatus(ctx, bookID int64,
status string) error`, and `Client.SetReadStatus` — a `PUT` to
`/koreader/plugin/catalog/books/{bookID}/read-status` that reuses the existing
`newRequest`/`do`/`withTimeout`/`encodeAndCheckSize` plumbing, intercepts 404 →
`ErrBookGone` before the shared classifier, and decodes the success body
through `readBoundedBody`. The request carries no device wrapper (Decision B).

### 3.3 `internal/sync/state` — four new `MatchRecord` fields

`MatchRecord` gained `LastSeenStatus`/`LastSeenStatusAt` and
`LastPushedStatus`/`LastPushedStatusAt`, slotted next to the existing
`LastPushedAt`/`LastPushedPct` with no restructuring and no interface change
(`Store.SetMatch`/`Match` already operate on the whole value). The fields
round-trip through both `MemStore` and `FileStore` (the latter via the
existing atomic `0600` JSON write).

### 3.4 `internal/sync` — pure mapper + engine step, gated by config

- `mapReadingStatus(readestStatus) (token string, push bool)`: `finished` →
  `("read", true)`; `abandoned` → `("abandoned", true)`; `unread` →
  `("", false)`; `""` / `reading` → `("", false)`; anything else (including a
  future `on_hold` token, Decision G) → `("", false)`.
- `classifyStatusValue` splits a non-push value into "record the seen
  bookkeeping" (known: `""` / `reading` / `unread`) versus "warn once, record
  nothing" (unrecognized). The warn-once dedup set lives on `Engine` as
  `warnedStatusValues`, initialized in `NewEngine`.
- `RunOnce` inserts the status-push phase after the push-progress phase, gated
  by `if e.cfg.Bridge.SyncStatus`. The step rides on whatever match the
  match-check phase already resolved this poll or a prior one; it never
  triggers its own match-check. Per row it skips dummy / deleted /
  unusable-progress / unmatched rows, then:
  - non-push recognized value → record the seen pair, no call;
  - unrecognized value → warn once, no call, no bookkeeping;
  - push value equal to `LastPushedStatus` → fold the seen bookkeeping
    forward, no call;
  - push value differing from `LastPushedStatus` → `SetReadStatus` via
    `pushReadStatus`, and on success update all four status fields.

  **Phase-10 note (supersedes the "unusable-progress" skip above, and the
  "never triggers its own match-check" framing).** The 2026-08-02 operator
  live report surfaced that this design's "unusable-progress" skip silently
  dropped exactly the rows that matter for status sync — a book downloaded
  then marked `finished` without ever being opened renders on the wire as
  `progress: null, reading_status: "finished"`. Phase 10
  (`docs/adr/phase-10-decision-record.md`, design at
  `docs/phase-10-status-sync-decoupling-design.md`) decoupled the status
  step from the progress classifier: the row-classification loop now routes
  decisive-status rows into a unified match-check queue independent of
  whether they carry a usable progress tuple, and `pushStatuses` no longer
  re-imposes the progress gate. Every other Phase 9 decision (the mapper,
  the Channel B shape, the 404 classification, the opt-in flag, the
  warn-once discipline) is preserved unchanged; only the eligibility
  predicate was wrong.
- `classifyBookOrbitStatusErr` is the status-specific verdict classifier:
  `ErrUnauthorized` → fatal (aborts `RunOnce`); `ErrBookGone`/`ErrBadRequest`
  → skip; rate-limit/server/network → retry; context cancellation → fatal.
  `ErrBookGone` is additionally branched on directly in `pushStatus` to drop
  the book (`DeleteMatch` + `SetUnmatched`, no watermark retreat), so the
  classifier's skip verdict for it is defense-in-depth.
- The Phase 6 progress-write `SetMatch` calls (`pushChunk`, `pushSingle`)
  preserve the status bookkeeping on the existing record rather than
  rebuilding a bare `MatchRecord`, so a progress push cannot reset the status
  baseline (see `docs/adr/phase-9-decision-record.md`, "Deviations found in
  testing").

### 3.5 `internal/config` — the opt-in toggle

`BridgeConfig.SyncStatus bool` (`yaml:"sync_status"`); `Default()` sets it to
`false`; `env.go` gained the `setBool` helper and the `EnvSyncStatus`
constant applied in `applyEnv`; `load.go` `applyValues` gained the
`bridge.sync_status` case via `strconv.ParseBool`; `Validate()` intentionally
has no invariant to check; `configs/bridge.example.yaml` documents the key
under the `bridge:` section (commented out, default `false`).

---

## 4. Explicit non-goals (unchanged)

- Two-way status sync (BookOrbit → Readest) — no source-of-truth signal exists
  for a reverse write; out of scope per `docs/problem-statement-prompt.md:24`.
- Channel A (`POST /koreader/plugin/book-states`) — wrong enum
  (`reading|complete|abandoned`, not BookOrbit-native tokens); Channel B is
  the only endpoint the status step touches.
- `POST /koreader/plugin/sweeps` (sweep-complete notification) — excluded in
  Phase 5 design §6; irrelevant to a headless bridge.
- Full-library recheck / `libraryVersion` gating for status — the existing
  `UnmatchedCooldown` mechanism is the only recheck gate.
- `want_to_read`/`rereading`/`skimmed` mappings — no Readest-side
  source-of-truth value exists to drive a push.
- `on_hold` — deferred (Decision G), not implementable from source evidence.

---

## 5. Test inventory (implemented)

All of the following are real, passing tests.

**`internal/bookorbit`**

- 404 on `SetReadStatus` → `ErrBookGone`, not `ErrUnsupportedEndpoint` (the
  <<<<<<< HEAD
  pinning test for Mech-α).
- Request body is exactly `{"status":"..."}`, no device-wrapper keys.
- Response decodes `{"readStatus":"..."}`.
- Full status matrix (400/401/403/404/429/5xx/network/cancellation/malformed).

**`internal/readest`**

- `reading_status` / `reading_status_updated_at` present → decodes; the
  timestamp parses via `util.ISOToMs`.
- Both absent/`null` → decode to `""` without error.

**`internal/sync/state`**

- Four new fields round-trip through both `MemStore` and `FileStore`.
- `FileStore` atomicity/permissions guarantees hold with the larger struct.

**`internal/sync` (engine)**

- `mapReadingStatus` table test (finished/abandoned/unread/reading/empty/
  unrecognized).
- Gate: `SyncStatus=false` → zero `SetReadStatus` calls ever.
- Fresh decisive push (finished/abandoned).
- Unchanged status → no re-push.
- Non-decisive (`reading`, `""`) → no-op, no call.
- `unread` → no-op push, but `LastSeenStatus` still updates.
- `unread → finished` transition pushes on the next poll.
- Unmatched book → status step skipped, no panic on a zero `BookID`.
- `ErrBookGone` → drop-from-sync-set, progress watermark unaffected.
- Retryable failure, exhausted → `LastPushedStatus` not advanced, progress
  watermark/advance for the same book's progress still advances.
- `ErrUnauthorized` → abort `RunOnce`, prior mutations preserved, `Save()`
  still runs once.
- Unrecognized value → warn once, silent thereafter, no bookkeeping update.
- Mixed multi-book batch composes correctly (push + skip + drop + no-op).
- `Save()` still exactly once per `RunOnce` with the new step present.
- Status bookkeeping is preserved across a progress push (progress-write
  `SetMatch` is field-preserving).

**`internal/config`**

- `bridge.sync_status` YAML key parses; invalid value is a hard error.
- `BRIDGE_SYNC_STATUS` env override parses and wins over the file.
- Default is `false` when neither is set; an invalid env value is ignored.

**Manual/live (post-implementation, tracked in `docs/live-validation-status.md`)**

- Real "Mark as finished" → BookOrbit shows `read`, idempotent on re-run.
- Real "Mark as Unread" → zero `SetReadStatus` calls, BookOrbit untouched.
- Real "Mark as abandoned" → token-identical round-trip.
- The `on_hold` probe (§2, Decision G) — informational only, does not gate
  this phase.

---

## 6. Verification

Recorded in full in `docs/adr/phase-9-decision-record.md`. The shipped working
tree passes:

```
$ go build ./...        # clean
$ go vet ./...          # clean
$ gofmt -l .            # no files reported
$ go test ./...         # all packages green
$ go test -race ./...   # all packages green
```
