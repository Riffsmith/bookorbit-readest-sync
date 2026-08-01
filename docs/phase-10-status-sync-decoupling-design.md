# Phase 10 — Status Sync Decoupling From Progress: Authoritative Design Record

**Status:** implemented per `docs/adr/phase-10-decision-record.md`. This
document is the historical investigative record (kept untouched per the
project's "design docs are historical; ADRs are as-built" convention, the
same pattern followed by every prior phase). What was actually built lives
in the ADR.

**Scope:** one surgical surgical fix to the shipped Phase 9 status-sync
feature. The Phase 9 status step is decoupled from the Phase 6 progress
classifier — a null-progress row carrying a decisive `reading_status`
(now observable on the wire for any book downloaded then marked
finished/abandoned without being opened) is now eligible for match-check
through the status channel and is status-pushed on the very first poll the
bridge sees it. Two surgical edits to `internal/sync/engine.go`; three
regression-guard tests appended to `internal/sync/engine_test.go`. No Phase
9 mapping table, Channel B body, 404 classification, opt-in flag, or
warn-once discipline is revisited.

**Provenance.** The bug was reported against a live account on 2026-08-02 by
the operator, with raw evidence (`/tmp/readest-pull-notes.json` and
`bridge-state.json` of the affected run). Investigation surfaced a two-layer
bug in the shipped Phase 9 engine, both inherited from the Phase 6 progress
classifier. Decisions I–IV are settled here; the implementation follows in
`docs/adr/phase-10-decision-record.md`.

---

## 1. The bug

### 1.1 Operator's report

A book downloaded from BookOrbit's OPDS into the Readest library, then marked
"finished" in Readest's UI without ever having been opened, did not propagate
to BookOrbit after a `BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config
configs/bridge.yaml` run. The book did not appear in `bridge-state.json` (no
`MatchRecord`, no `UnmatchedAt` entry). The raw Readest pull did contain the
book with `reading_status: "finished"` and a server-stamped `synced_at`.

Marking progress on the same book (turning even one page in Readest) made the
bridge pick it up on the very next run — the status appeared in
`bridge-state.json` after that.

### 1.2 Live-wire evidence (from `/tmp/readest-pull-notes.json`)

The operator's pull contains 355 books. Three of them have `progress: null`
(24 books have `progress: null` overall — never opened in Readest — and 3
of those 24 carry a non-empty `reading_status` value, all `"finished"`):

| Title | `book_hash` | `progress` | `reading_status` | `updated_at` | `synced_at` |
|---|---|---|---|---|---|
| Magic Academy's Bastard Instructor | `1f70ec53...` | `null` | `finished` | 08:44:39 | 08:44:42 |
| Extra's Descent | `6999fa76...` | `null` | `finished` | 08:44:33 | 08:44:35 |
| Lord of Mysteries | `ba29246f...` | `null` | `finished` | 2026-07-27 (status), 08:42:36 (row) | 08:43:31 |

For all three, `synced_at` is present and newer than `updated_at`, confirming
the Readest server considers them changed rows and emits them on every `since=0`
pull. The bug is entirely on the bridge side.

### 1.3 Root cause, two layers

**Layer 1** — `internal/sync/engine.go` RunOnce row-classification loop:

```go
pct, ok := row.Percentage()
if !ok {
    continue    // ← null-progress rows skipped here
}
// ...match-check / push enqueue logic follows...
```

A null-progress row is skipped at this gate before the row ever reaches
match-check. So it never enters `toMatch`, never gets a `MatchRecord`, and —
critically — never enters `Unmatched` either (since `DeleteMatch`/`SetUnmatched`
are only called in the match-check response handler). Worse: the row's
`WatermarkMs` is computed ABOVE this skip (Phase 6's invariant), so the
watermark advances past the row's `synced_at`; subsequent `since=<watermark>`
pulls never re-return it.

**Layer 2** — `internal/sync/engine.go` `pushStatuses`:

```go
if _, ok := row.Percentage(); !ok {
    continue    // ← null-progress rows skipped again
}
```

The Phase 9 status step inherited the same progress gate, even though
`mapReadingStatus` reads only `row.ReadingStatus` and never `row.Progress`.
A matched book whose cached `LastPushedStatus` differed from a new decisive
`ReadingStatus` would be skipped from status sync the moment its progress
tuple became unusable.

**Why removing Layer 2 alone was not sufficient.** Even if only the Layer 2
gate were removed, the Layer 1 gate would still drop null-progress rows
before match-check. The operator's exact scenario — finished without ever
opening — requires the status step to be self-sufficient at: (a) finding
new matches for null-progress rows, and (b) pushing their status once
matched.

---

## 2. Decisions

Decisions I–IV were settled before implementation; an ADR records the
as-built account. They are repeated here in final form.

### Decision I — Scope of the fix

**Fully decouple status from progress.** The status step no longer gates on
`row.Percentage()` succeeding. The row-classification phase routes decisive-
status rows into the unified match-check queue independent of whether the
row also has a usable progress tuple.

The bug was in *who is allowed to call `mapReadingStatus`*, not in what
`mapReadingStatus` returns. The Phase 9 mapper, sentinels, retry classifier,
and `ErrBookGone` handling are untouched.

### Decision II — Match-check queue shape

**Unified.** One `MatchCheck` pass per RunOnce, fed by the union of progress-
eligible and status-eligible rows (deduped by hash). Phase 6's existing
`MATCH_BATCH = 500` batching plumbing is reused unchanged; no second
MatchCheck call is added for the status channel.

A `progressEligible || statusEligible` boolean OR replaces the historical
"row was implicitly added to `toMatch` when match-check was needed" side
effect. A row can never enter `toMatch` twice for the same poll.

The alternative — two independent MatchCheck passes, one per channel —
would double HTTP cost when both paths run on the same poll, risk divergence
on match resolution if the two paths ever disagreed, and need new batching
plumbing. The unified queue is a strict simplification over that alternative.

### Decision III — Null-progress decisive-status rows, first poll

**Push status, skip progress.** A `progress: null, reading_status: "finished"`
row:

- enters match-check via the `statusEligible` predicate,
- gets a `MatchRecord` with `LastPushedStatus == ""` (zero value, from the
  fresh struct literal the match-check phase writes),
- the status step reads the cached record back, sees
  `"read" != ""`, and pushes `set-read-status(bookID, "read")`,
- the progress path correctly does nothing (`Percentage() !ok` → no `toPush`
  append; the existing match-check handler in the match-check phase also
  guards `if pct, ok2 := row.Percentage(); ok2 { toPush.append(...) }`,
  so a null-progress row that turned out to be a match also correctly
  produces no progress push),
- on subsequent polls the unchanged-status skip fires (`LastPushedStatus ==
  "read" == mapReadingStatus("finished")`), so no further status calls; if
  the user later opens the book, the progress path runs normally against
  the cached record (`LastPushedPct == 0`, any real progress is detected as
  a change), and Phase 9's existing `pushChunk`/`pushSingle` preserve
  `LastSeenStatus`/`LastPushedStatus` when writing the progress fields.

The alternative — "match only, defer status push to a later poll where the
user must have made some progress" — does not resolve the operator's
reported scenario at all: the user marked the book finished without ever
opening it, so progress would remain `null` indefinitely and the status
would never be pushed.

### Decision IV — Phase 9 decision preservation

All of Phase 9's Decisions A–H remain intact:

| Phase 9 Decision | Phase 10 status |
|---|---|
| A — `unread` records seen bookkeeping without pushing | **Preserved.** The `LastSeenStatus` write is unaffected by Phase 10's eligibility refactor. |
| B — Channel B body is `{"status": token}` exactly, no device wrapper | **Preserved.** Phase 10 touches only eligibility, not request shape. |
| C — 404 → `bookorbit.ErrBookGone` → `DeleteMatch` + `SetUnmatched`, no watermark retreat | **Preserved.** The match-check response handler's `SetUnmatched`/`DeleteMatch` for absent-from-both is unchanged; `pushStatus`'s `ErrBookGone` branch is unchanged; Decision E decoupling holds. |
| D — Unrecognized Readest status logged `WARN` once-per-value, then skipped | **Preserved.** The per-process `warnedStatusValues` set is unchanged; Phase 10 adds no new unrecognized values. |
| E — Status failure decoupled from progress watermark | **Preserved.** Null-progress decisive-status rows still never touch `failedWatermarks` when their status push fails. |
| F — `BRIDGE_SYNC_STATUS` opt-in, default `false` | **Preserved.** Phase 10's status-eligibility predicate is gated on `e.cfg.Bridge.SyncStatus` exactly as the Phase 9 status step already was. |
| G — `on_hold` shipped unresolved (live probe item) | **Preserved.** Phase 10 changes nothing about which tokens `mapReadingStatus` recognizes. |
| H — 404 sentinel mechanism is `bookorbit.ErrBookGone` | **Preserved.** Phase 10 touches no sentinels. |

---

## 3. Implementation shape

### 3.1 Refactor RunOnce row-classification loop

The single-pronged Phase 6 flow (dummy/deleted skip → `Percent ok?` filter
→ match-cache lookup → unchanged-pct skip OR `toMatch`/`toPush` append) is
replaced with a two-pronged classifier:

```
for each row:
    if row.IsDummy(): continue                                  (existing)
    wm := row.WatermarkMs(); if wm > maxWatermark: maxWatermark = wm  (existing)
    if row.IsDeleted(): DeleteMatch/ClearUnmatched; continue    (existing)

    progressEligible := false
    statusEligible := false

    pct, hasPct := row.Percentage()
    if hasPct:
        rec, merr := st.Match(row.BookHash)
        switch merr:
        case nil:
            if rec.LastPushedAt != 0 && |pct - rec.LastPushedPct| <= tolerance:
                // unchanged pct: don't enqueue progress push, but row may
                // still be status-eligible below.
            else:
                toPush.append(pushItem{...})                    (existing)
        case ErrNotFound:
            at, cooldown := st.UnmatchedAt(row.BookHash)
            if not cooldown:
                progressEligible = true                          (existing behavior; new explicit bool)
        default:
            log.Warn(...)                                         (existing)

    if e.cfg.Bridge.SyncStatus:
        if _, push := mapReadingStatus(row.ReadingStatus); push:
            if _, merr := st.Match(row.BookHash); merr == ErrNotFound:
                at, cooldown := st.UnmatchedAt(row.BookHash)
                if not cooldown:
                    // Dedup: don't add the same row a second time.
                    if not progressEligible:
                        statusEligible = true

    if progressEligible || statusEligible:
        toMatch.append(row)
```

Notes:

- The dedup is the boolean OR. A single row may push *itself* into
  `toMatch` for either reason, never twice for the same hash in the same
  pass (Phase 6 already keyed `rowByHash[h]` on hash, so duplicate rows
  are not a concern).
- The cooldown gate is shared across both channels: a row in cooldown is
  not put back into the match-check queue even if it carries a decisive
  status. The cooldown is keyed on `book_hash`, which is shared.
- The non-SyncStatus poll behavior is preserved bit-for-bit:
  `e.cfg.Bridge.SyncStatus == false` ⇒ the entire status-eligibility block
  is skipped ⇒ the bridge behaves identically to Phase 9 with status off.
- `progressEligible = true` replaces the previous implicit "row was added
  to `toMatch`" side effect. This refactor is the only structural change
  to `RunOnce`'s main loop; the match-check phase itself is unchanged.

### 3.2 Match-check phase

Structurally unchanged. The match-check response handler still calls
`SetMatch` with a fresh `MatchRecord{BookFileID, BookID}` (zero-valued status
fields by construction), so a null-progress decisive-status row that the
match-check phase newly matched will have `LastPushedStatus == ""` when the
status step reads it back via `Match(hash)` — and `"read" != ""` will
correctly fire the status push. No additional `newlyMatched` set is needed;
the existing zero-value semantics already Do The Right Thing.

### 3.3 Drop the progress gate from `pushStatuses`

Delete the line:

```go
if _, ok := row.Percentage(); !ok {
    continue
}
```

Rewrite the function's doc-comment to record the Phase 10 decoupling. No
other logic change.

### 3.4 `pushStatus`, `mapReadingStatus`, `recordSeenStatus`,
`classifyStatusValue`, `classifyBookOrbitStatusErr`, `pushReadStatus`

Unchanged. Phase 9's status-path semantics, retry classification, and failure
handling are intact.

---

## 4. Out of scope (explicitly not touched)

- **Watermark retreat on the absent-from-both path** — Phase 6 Addendum 2's
  resolution stands.
- **Bulk→UpdateProgress fallback** — Phase 6 Decision F stands.
- **`on_hold` mapping** — still dependent on the live probe (Phase 9
  Decision G-1).
- **Annotation sync** — `docs/annotation-sync-feasibility.md` remains in
  feasibility-investigation state; Phase 10 closes one of its preconditions
  (a book the bridge currently drops from status sync for want of progress
  would also have been dropped from annotation sync), but Phase 10 does not
  implement annotation sync.
- **Match-check body shape / device wrapper / Source enum** — Phase 5
  Design plus Phase 6 Addendum 1 stand unchanged. The candidates the engine
  builds for status-only rows still use `Source: "file"`,
  `MetadataAmbiguous: false`, `LastOpen: <updated_at in seconds>` exactly
  as built by the shipped code.

---

## 5. Test strategy

Three new tests, all in `internal/sync/engine_test.go`, all using the
project's existing hand-written fakes (no new test infrastructure):

### 5.1 `TestRunOnceNullProgressFinishedBookStatusPushes` (the bug regression guard)

Exercises a row with `progress: null, reading_status: "finished"` against a
fake BookOrbit that returns the row as a match. Asserts:

1. `matchCalls == 1` (null-progress decisive rows now reach MatchCheck via
   the `statusEligible` path; pre-Phase-10 this was 0).
2. `bulkCalls == 0` and `updateCalls == 0` (no progress tuple, no progress
   push).
3. `statusCalls` has exactly one `(bookID=11, token="read")` entry.
4. `MatchRecord` carries `LastSeenStatus="finished"`,
   `LastPushedStatus="read"`, and `LastPushedPct == 0` / `LastPushedAt == 0`.
5. A second `RunOnce` with the same rows makes zero further calls —
   `matchCalls` stays at 1 (match cached, no recheck),
   `bulkCalls`/`updateCalls` stay at 0, `statusCalls` stays at length 1 (the
   unchanged-skip fires).

### 5.2 `TestRunOnceNullProgressThenOpensBookPushesProgressToo` (decoupling "future-tense" guard)

Two-phase scenario with two `RunOnce` invocations against the same engine:

- Run 1: `progress: null, reading_status: "finished"` → status pushed, no
  progress pushed. `matches` cache now has the row with `LastPushedStatus =
  "read"`, `LastPushedPct = 0`.
- Run 2: same hash, `progress: [3, 5167]` (reader turned pages),
  `reading_status: "finished"` unchanged.

Asserts:

1. zero `matchCalls` across Run 2 (already cached).
2. zero `statusCalls` across Run 2 (unchanged-skip via
   `LastPushedStatus == "read"`).
3. one `bulkCalls` with one item (`pct = 3/5167` differs from `0` by more
   than `percentTolerance = 0.001`, so the progress path enqueues).
4. **The Phase 9 invariant preserved by Phase 10**: after Run 2,
   `MatchRecord.LastPushedStatus == "read"` (progress push did not reset
   status bookkeeping), `LastSeenStatus == "finished"` (same), and
   `LastPushedPct > 0` (progress push did advance the progress bookkeeping).

### 5.3 `TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch` (Decision II regression guard)

Three rows in one poll:

- Row A: `progress: [5, 100]`, no `reading_status` (progress-eligible only).
- Row B: `progress: null, reading_status: "finished"` (status-eligible only).
- Row C: `progress: [7, 200], reading_status: "finished"` (both eligible;
  must be deduped to a single MatchCheck entry).

Asserts:

1. exactly one `MatchCheck` call (`matchCalls == 1`).
2. the request's `Hashes` slice contains all three unique hashes (`hA`,
   `hB`, `hC`) and no duplicates.
3. exactly one `BulkProgress` call containing exactly A and C (B has no
   progress to push).
4. exactly two `SetReadStatus` calls for B and C (A has no decisive status).

A regression that introduced a separate status-channel MatchCheck pass
would fail assertions (1) and (2).

### 5.4 No changes to existing tests

All Phase 9 status-tests (`TestRunOnceStatusGateOffNeverPushes`,
`TestRunOnceStatusFreshFinishedPushes`, `TestRunOnceStatusAbandonedPassthrough`,
`TestRunOnceStatusUnchangedSkips`, `TestRunOnceStatusNonDecisiveNeverPushes`,
`TestRunOnceStatusUnreadRecordsSeenButDoesNotPush`,
`TestRunOnceStatusUnreadThenFinishedTransitions`,
`TestRunOnceStatusUnmatchedBookSkipped`,
`TestRunOnceStatusBookGoneDropsFromSyncSet`, `TestMapReadingStatus`,
`TestRunOnceStatusUnrecognizedValueWarnsOnce`, mixed-batch /
`Save`-called-exactly-once / mixed-progress-+-status-failure scenarios)
continue to pass unchanged. None of them had been exercising the
null-progress-decisive-status path — exactly the gap the three new tests
close.

---

## 6. Live re-verification path

The bridge's persisted watermark on the operator's live state file has
already advanced past the three affected books' `synced_at` values, so a
routine `--once` on the existing state file will not re-pull them. To
confirm the fix end-to-end against the live account:

1. Stop the bridge if running in daemon mode.
2. Back up `bridge-state.json` (it contains auth tokens).
3. Delete `bridge-state.json`. The bridge will regenerate it on next run.
4. Run `BRIDGE_SYNC_STATUS=true BRIDGE_LOG_LEVEL=debug ./bin/bridge --once
   --config configs/bridge.yaml`.
5. Inspect the new `bridge-state.json`: the three affected hashes should
   each have `LastPushedStatus: "read"` and `LastSeenStatus: "finished"`.
6. Inspect the BookOrbit catalog (web UI): the three books should display
   as `read`.

This is tracked as a `resolved-on-confirmation` item in
`docs/live-validation-status.md`. The three unit tests pin the behavioral
contract; they fail against the pre-Phase-10 engine, so a regression
silently re-introducing the old code would fail at least
`TestRunOnceNullProgressFinishedBookStatusPushes` immediately.

---

## 7. Risk register

- **MatchCheck batch size for very-large libraries.** A library-wide `since=0`
  pull on an account with hundreds of unread books would blow past
  `MATCH_BATCH = 500` faster than before, since previously-skipped
  null-progress rows now queue. The existing `util.BatchFunc` plumbing
  handles batching correctly, but operators with hundreds of unread books
  should be aware that the very first run with `BRIDGE_SYNC_STATUS=true` now
  has MatchCheck ticking through every decisive-status book in the library.
  Behavior, not bug. Documented in the README.
- **Bridge-state migration for existing installs.** Phase 10 does not change
  `MatchRecord`'s schema or the `Data` struct's JSON shape. No migration is
  required. The three books in the operator's account will resolve on the
  next poll only if their `synced_at` is newer than the persisted watermark;
  the operator's case (watermark advanced past them) needs the
  delete-state-file step from §6 to confirm live. New books status-marked
  from now on will resolve on the very next poll without any state-file
  reset.
- **Cooldown interaction between progress and status.** The cooldown gate is
  keyed on `book_hash` and shared across both channels: if a null-progress
  decisive-status row gets matched and then later dropped (Phase 6 Addendum
  2 / `bookorbit.ErrBookGone`), the cooldown blocks re-matching on both
  progress and status. This is the intended behavior and matches Phase 9's
  Decision E decoupling: a book gone from BookOrbit's library is gone for
  both channels.

---

*Prepared from `internal/sync/engine.go` (Phase 9 release as of
2026-08-02), the operator's `/tmp/readest-pull-notes.json` from the same
date, `docs/phase-9-status-sync-design.md`,
`docs/status-sync-design-investigation.md`, `docs/adr/phase-9-decision-record.md`,
and `docs/adr/phase-6-decision-record.md` Addenda 1 and 2.*
