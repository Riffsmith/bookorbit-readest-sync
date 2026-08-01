# Phase 10 — Decision Record

**Status:** Implemented per the approved design
(`docs/phase-10-status-sync-decoupling-design.md`). Scope was two surgical
edits to `internal/sync/engine.go` plus three regression-guard tests in
`internal/sync/engine_test.go`. No package boundary changed; no state schema
change; no new endpoint; no new sentinel error. `go build ./...`,
`go vet ./...`, `gofmt -l .`, `go test ./...`, and `go test -race ./...` all
pass, including every newly added test.

Phase 10 is complete. Live re-verification (a `since=0` re-pull on the
operator's account, after deleting the persisted state file, to confirm the
three finished-but-never-opened books now reach BookOrbit as `read`) is a
manual item, tracked in `docs/live-validation-status.md`; it does not block
this phase.

---

## The bug this phase fixes

Operator live-server report, 2026-08-02:

> I download a book from BookOrbit's OPDS which I have never read in
> BookOrbit. I set its status as "Mark as finished" (finished) without even
> opening or reading any pages. [...]
> `curl 'https://web.readest.com/api/sync?type=books&since=0'` returns the
> book with `reading_status: "finished"`. [...]
> `BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config configs/bridge.yaml`
> does not write this book into `bridge-state.json` [...]
> [But] if I go into Readest and read that book — even just turn a single
> page and let it sync — then I get that book in the bridge-state.json.

The reported behavior was the load-bearing evidence: a status-only change on
a book the user had downloaded from BookOrbit's OPDS but never opened did not
propagate through the bridge, while the same book *did* propagate after the
user made any progress at all. Inspection of `/tmp/readest-pull-notes.json`
(the operator's raw Readest pull) confirmed three rows in the account with
`progress: null` and `reading_status: "finished"` — books downloaded then
immediately marked finished. All three carried a server-stamped `synced_at`
newer than the matching client `updated_at`, confirming the Readest server
considered them changed rows and would emit them on any `since=0` pull.

The bug was entirely on the bridge side. Two layers suppressed the
decisive-status rows:

### Layer 1 — the row-classification phase (historical Phase 6 path)

`internal/sync/engine.go` `RunOnce`'s main loop filtered every null-progress
row out of match-check BEFORE the row ever reached the status step:

```go
pct, ok := row.Percentage()
if !ok {
    continue                                  // ← null-progress rows skipped here
}
rec, merr := e.st.Match(row.BookHash)
switch {
case errors.Is(merr, state.ErrNotFound):
    …
    toMatch = append(toMatch, row)
…
}
```

A row skipped here never enters `toMatch`, never reaches MatchCheck, never
gets a `MatchRecord` in the state store. Worse: the row's `WatermarkMs` was
still computed ABOVE the skip (Phase 6's invariant), so the watermark
advanced past the row's `synced_at`, and subsequent `since=<watermark>` pulls
never re-returned it. The row became permanently invisible after the first
poll cycle.

### Layer 2 — the status-push step (Phase 9 path)

The Phase 9 status step inherited the same filter:

```go
func (e *Engine) pushStatuses(...) (fatal bool) {
    for _, row := range rows {
        if row.IsDummy() || row.IsDeleted() { continue }
        if _, ok := row.Percentage(); !ok {
            continue                          // ← null-progress rows skipped again
        }
        rec, merr := e.st.Match(row.BookHash)
        …
    }
}
```

This gated status push on the row having a usable progress tuple, even though
mapReadingStatus reads only `row.ReadingStatus` and never `row.Progress`. A
matched book whose cached `MatchRecord.LastPushedStatus` differed from a new
decisive `ReadingStatus` value would have been skipped from status sync the
moment its progress tuple became unusable. The shipped Phase 9 design
inherited this gate from the progress path without flagging it;
`docs/status-sync-design-investigation.md:114` ("status push depends on
match-check having already resolved a `BookID`") silently assumed match-check
ran for that row, which Layer 1 silently violated for null-progress rows.

### Why removing Layer 2 alone was not sufficient

Even if only Layer 2 were removed, the Layer 1 skip would still drop
null-progress rows before match-check. The decisive case the operator
reported — "user finishes a book they've never opened" — requires the status
step to be self-sufficient at BOTH finding new matches for null-progress
rows AND pushing their status. A partial Layer-2-only fix would have left
exactly that case broken.

---

## Decisions implemented

All decisions are settled by `docs/phase-10-status-sync-decoupling-design.md`
which was approved before implementation. They are recorded here in final
form; the design doc's investigative prose is left as historical record.

| # | Decision | Implementation |
|---|----------|----------------|
| I | Scope of the fix | **Fully decouple status from progress.** The status step no longer gates on `row.Percentage()` succeeding; the row-classification phase routes decisive-status rows into the unified match-check queue independent of whether the row also has a usable progress tuple |
| II | Match-check queue shape | **Unified** — one MatchCheck pass per RunOnce, fed by the union of progress-eligible rows and status-eligible rows (deduped by hash). Phase 6's existing `MATCH_BATCH=500` batching plumbing is reused unchanged; no second MatchCheck call is added for the status channel |
| III | Null-progress decisive-status rows, first poll | **Push status, skip progress.** The row enters match-check via the `statusEligible` predicate, gets matched, and the status step pushes the mapped token. The progress path correctly does nothing (no `toPush` append) because `row.Percentage()` returns `false`. If the user later opens the book, the next poll's progress path runs normally against the cached `MatchRecord` (LastPushedPct starts at 0, so any real progress is detected as a change) |
| IV | Phase 9 decision preservation | All of A–H remain intact: the two-pair `MatchRecord` schema (`LastSeenStatus`/`At` + `LastPushedStatus`/`At`), the Channel B `{"status": token}` body shape, the 404 → `bookorbit.ErrBookGone` + `DeleteMatch` + `SetUnmatched` rule (no watermark retreat), the `BRIDGE_SYNC_STATUS` opt-in default-off, the warn-once-per-unrecognized-value dedup set, the Decoupling-E invariant (status failure never touches `LastPushedPct`/`At` or `failedWatermarks`). `mapReadingStatus` is unchanged; the bug was always in *who is allowed to call it*, not in what it returns |

---

## What changed, file by file

### `internal/sync/engine.go`

**`RunOnce` row-classification loop** (`for _, row := range rows` block):

The single-pronged Phase 6 flow — dummy/deleted skip → `Percent ok?` check
(2-layer bug, Layer 1) → match-cache lookup → either unchanged-pct skip or
`toMatch`/`toPush` append — was replaced with a two-pronged classifier.
Each row is independently evaluated for two predicates:

- **`progressEligible bool`** — true iff the row has a usable progress tuple,
  no current match, and is not in unmatched cooldown. (Identical trigger to
  the historical Phase 6 path; behavior preserved bit-for-bit for
  progress-bearing rows.)
- **`statusEligible bool`** — true iff `Bridge.SyncStatus` is enabled,
  `mapReadingStatus(row.ReadingStatus)` returns push=true (i.e. the row has a
  decisive `finished` or `abandoned` status), no current match, and not in
  unmatched cooldown. (Null-progress rows are eligible here; this is the
  fix for Layer 1.)

A row qualifies for `toMatch` if either predicate is true. The two predicates
are deduped by hash: `if !progressEligible { statusEligible = true }` — a row
can never enter `toMatch` twice for the same poll. Phase 6's batching loop in
the match-check phase is unchanged; it already iterated `toMatch` by unique
hash.

The unchanged-pct skip (`rec.LastPushedAt != 0 && math.Abs(pct - rec.LastPushedPct) <= percentTolerance`) is preserved exactly: it controls whether `toPush` is appended, but the same row may still be `statusEligible` and route into `toMatch` if needed (e.g. a book whose percentage hasn't moved but whose reading_status just changed to `finished`).

The unmatched-cooldown `continue` from Phase 6 was retained as-is for BOTH
predicates: a row in cooldown is not put back into the match-check queue,
even if it carries a decisive status. The cooldown gate is shared across
both channels because it's keyed on `book_hash`, not on which channel asked.

The watermark computation above the skip (Phase 6 invariant: `wm :=
row.WatermarkMs()`, `if wm > maxWatermark { maxWatermark = wm }`) is
preserved unchanged.

**`pushStatuses` doc-comment and body:**

The `if _, ok := row.Percentage(); !ok { continue }` line was deleted (Layer 2
fix). The function now iterates: skip dummy/deleted; `Match(hash)` → on
`ErrNotFound` skip (unmatched); otherwise call `pushStatus`. The function's
doc-comment was rewritten to record the Phase 10 decoupling: a row is
status-eligible purely on the basis of carrying a decisive `reading_status`,
not on whether its progress tuple is computable. The previous doc-comment
line ("a row whose percentage could not be computed also has no business
asserting a status") was removed — it was the design-level error that let
the bug sit for an entire Phase 9 release.

**Match-check phase, push-progress phase, finish, watermark math:**
unchanged. Phase 6 Addendum 2's absent-from-both handling,
Decision F's bulk→UpdateProgress fallback, and Decision C's single
`state.Store.Save()` per RunOnce all stand unmodified.

**`mapReadingStatus`, `pushStatus`, `recordSeenStatus`,
`classifyStatusValue`, `classifyBookOrbitStatusErr`, `pushReadStatus`:**
unchanged. Phase 9's status-path semantics, retry classification, and
failure handling are intact.

### `internal/sync/engine_test.go`

**`mkNoProgressRow` helper** (new). Pairs with `mkStatusRow`: the existing
helper builds rows with `progress: [10, 100]` (a usable tuple); the new one
builds rows with `progress: null` (the wire form the operator observed on
the bug). Sets `SyncedAt = UpdatedAt` so `WatermarkMs()` reflects the row.

**`TestRunOnceNullProgressFinishedBookStatusPushes`** (new). The regression
guard for the operator's exact reported scenario. Exercises a `progress:
null, reading_status: "finished"` row against a fake BookOrbit that reports
the row as matched. Asserts:

1. `matchCalls == 1` (Layer 1 fix: null-progress decisive rows now reach
   MatchCheck via the `statusEligible` path).
2. `bulkCalls == 0` and `updateCalls == 0` (no progress tuple, no progress
   push — the progress path correctly does nothing).
3. `statusCalls` has exactly one `(bookID=11, token="read")` entry.
4. The `MatchRecord` carries `LastSeenStatus="finished"`,
   `LastPushedStatus="read"`, and `LastPushedPct == 0` / `LastPushedAt == 0`
   (no progress was ever pushed for this book; the status bookkeeping set by
   the status step is preserved by the existing `pushChunk`/`pushSingle`
   behavior of updating only the progress fields).
5. A second `RunOnce` with the same rows makes zero further calls of any
   kind — `matchCalls` stays at 1 (`lengthy: match cached, no recheck`),
   `statusCalls` stays at length 1 (the unchanged-skip fires because
   `LastPushedStatus == "read"`), `bulkCalls`/`updateCalls` stay at 0.

**`TestRunOnceNullProgressThenOpensBookPushesProgressToo`** (new). Tests the
decoupling "future-tense" direction: a book status-pushed on a null-progress
poll, then later opened by the reader, must have progress pushed and status
unchanged-skipped — and the status bookkeeping from Run 1 must NOT be reset
by Run 2's progress push (the Phase 9 invariant preserved by Phase 10):

- Run 1: `progress: null`, `reading_status: "finished"` → status pushed,
  no progress pushed, `LastPushedPct == 0` and `LastPushedStatus == "read"`.
- Run 2: `progress: [3, 5167]` (reader turned three pages), `reading_status:
  "finished"` unchanged.
- Asserts zero `matchCalls` (already cached), zero `statusCalls` (unchanged
  skip via `LastPushedStatus == "read"`), one `bulkCalls` containing one
  item (`pct = 3/5167 ≈ 0.00058` differs from `0` by more than
  `percentTolerance = 0.001`, so the progress path enqueues the push).
- Most importantly: the `MatchRecord` after Run 2 still has
  `LastPushedStatus == "read"` and `LastSeenStatus == "finished"` (the
  progress push did not reset status bookkeeping) AND `LastPushedPct > 0`
  (the progress push did advance the progress bookkeeping).

**`TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch`** (new).
Decision-II regression guard. Three rows in one poll:

- Row A: `progress: [5, 100]`, no `reading_status` (progress-eligible only).
- Row B: `progress: null`, `reading_status: "finished"` (status-eligible only).
- Row C: `progress: [7, 200]`, `reading_status: "finished"` (both eligible;
  must be deduped to a single MatchCheck entry).

Asserts exactly one `MatchCheck` call carrying exactly three unique hashes
(`hA`, `hB`, `hC`); exactly one `BulkProgress` call containing exactly A and
C (B has no progress to push); exactly two `SetReadStatus` calls for B and C
(A has no decisive status). This is the test that most directly pins the
Decision-II unified-queue invariant; a regression that introduced a separate
status MatchCheck pass would fail it.

### Documentation

- `docs/adr/phase-10-decision-record.md` (this file, new).
- `docs/live-validation-status.md` — added a "Resolved" line pinning that
  status sync correctly handles null-progress decisive-status rows (Phase 10
  fix); cites the bug report and the regression-test name.
- `docs/phase-9-status-sync-design.md` — added a pointer to this Phase 10
  ADR next to the original Phase 9 design's mischaracterization of
  null-progress eligibility, without rewriting the Phase 9 prose (per the
  project's convention of preserving design docs as historical records).
- `docs/status-sync-design-investigation.md` — added a note in §3.5 next to
  the "Unmatched book → status step skipped" row pointing at the Phase 10
  ADR; the premise that match-check runs for every status-eligible row was
  never guaranteed until Phase 10 made it so.
- `README.md` — "Status sync (optional)" subsection's language was tightened
  to reflect that a finished-but-never-opened book now propagates correctly.

---

## Deviations from the design

**One minor implementation detail, the `newlyMatched` map skipped.** The
Phase 10 design proposed a `newlyMatched map[string]bool` in the match-check
phase to signal "this hash was freshly matched this poll, so the status step
will see a zero-value `LastPushedStatus` and push the decisive token."
Inspection during implementation showed this set is unnecessary: the
existing `SetMatch(m.Hash, state.MatchRecord{BookFileID: m.BookFileID,
BookID: m.BookID})` call (engine.go, match-check phase) already writes a
`MatchRecord` whose status fields are zero-valued by construction (zeroed on
the fresh struct literal). When the status step reads the record back via
`Match(hash)`, it sees `LastPushedStatus == ""`, the mapped `"read"` differs,
and the push fires. No additional state plumbing is needed.

This is a minor implementation divergence from the planned wording (worth
recording, not a behavior change). It is recorded here so a future
maintainer doesn't re-introduce the unused set under "completeness."

---

## Verification performed

```
$ go build ./...        # clean
$ go vet ./...          # clean
$ gofmt -l .            # no files reported
$ go test ./...         # all packages green, including the three new Phase 10 tests
$ go test -race ./...   # all packages green
```

The three new tests were also run individually with `-v` to confirm every
subtest name and pass status:

- `TestRunOnceNullProgressFinishedBookStatusPushes` — pass.
- `TestRunOnceNullProgressThenOpensBookPushesProgressToo` — pass.
- `TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch` — pass.

No existing test regressed. The Phase 9 status-test matrix
(`TestRunOnceStatusGateOffNeverPushes`, `TestRunOnceStatusFreshFinishedPushes`,
`TestRunOnceStatusAbandonedPassthrough`, `TestRunOnceStatusUnchangedSkips`,
`TestRunOnceStatusNonDecisiveNeverPushes`,
`TestRunOnceStatusUnreadRecordsSeenButDoesNotPush`,
`TestRunOnceStatusUnreadThenFinishedTransitions`,
`TestRunOnceStatusUnmatchedBookSkipped`,
`TestRunOnceStatusBookGoneDropsFromSyncSet`, `TestMapReadingStatus`,
`TestRunOnceStatusUnrecognizedValueWarnsOnce`, and the mixed-batch /
`Save`-called-exactly-once / mixed-progress-+-status-failure scenarios)
continues to pass unchanged. None of them had been exercising the
null-progress-decisive-status path — exactly the gap the three new tests
close.

---

## Live re-verification steps

The bridge's persisted watermark on the operator's live state file has
already advanced past the three affected books' `synced_at` values, so a
routine `--once` on the existing state file will not re-pull them. To
confirm the fix end-to-end against the live account, the following manual
sequence is required (and is tracked as a resolved-on-confirmation item in
`docs/live-validation-status.md`):

1. Stop the bridge if running in daemon mode.
2. Back up the current `bridge-state.json` (it contains valid auth tokens;
   don't delete blindly — copy to `bridge-state.json.bak`).
3. Delete `bridge-state.json`. The bridge will regenerate it on next run.
4. Run `BRIDGE_SYNC_STATUS=true BRIDGE_LOG_LEVEL=debug ./bin/bridge --once
   --config configs/bridge.yaml`.
5. Inspect the new `bridge-state.json`: the three hashes
   `1f70ec53db4782bb5a51c3b4f0af5fc8`,
   `6999fa76a4cbd24f9f198e27519deddf`, and
   `ba29246ff67da1189c927d3782185cca` should now each have a `MatchRecord`
   with `LastPushedStatus: "read"` and `LastSeenStatus: "finished"`.
6. Inspect the BookOrbit catalog (web UI): the three books should display
   as `read`.

Before this confirmation run lands, the three new unit tests pin the
behavior; they fail against the pre-Phase-10 engine if applied as-is, so a
regression silently re-introducing the old code would fail at least
`TestRunOnceNullProgressFinishedBookStatusPushes` immediately.

---

## Acknowledged risks

- **MatchCheck batch size for very-large libraries.** A library-wide
  `since=0` pull on an account with hundreds of unread books would blow
  past `MATCH_BATCH = 500` faster than before, since previously-skipped
  null-progress rows now queue for match-check. The existing
  `util.BatchFunc` plumbing handles this correctly (batches of 500), but
  operators with hundreds of unread books should be aware that the very
  first run with `BRIDGE_SYNC_STATUS=true` now has MatchCheck ticking
  through every decisive-status book in the library — behavior, not bug.
- **Bridge-state migrations for existing installs.** Phase 10 does not change
  the `MatchRecord` schema, the `Data` struct, or the state-file format. No
  migration is required; existing state files continue to load and benefit
  from the fix on the next poll. The three books in the operator's account
  will resolve on the next poll only if their `synced_at` is newer than the
  persisted watermark — which the operator's case (watermark advanced past
  them) does NOT satisfy. Hence the "delete state file" step in the live
  re-verification recipe. New books status-marked from now on will resolve
  on the very next poll without any state-file reset.

---

## Why this is the third live-server invalidation, and what that pattern means

This is the third such invalidation in the project's history, after:

- Phase 6 Addendum 1 (2026-07-30): `Source="readest"` in the match-check
  candidate was rejected by the live server with HTTP 400; only the three
  enum values `current_file`, `file`, `statistics` are accepted. Server-side
  validation had not been verified against the plugin's source.
- Phase 6 Addendum 2 (2026-07-30): the assumption that the live BookOrbit
  server echoes every unknown hash in `resp.Unmatched` was wrong; the server
  omits them from both lists. The plugin's defensive fallback (treat absence
  as unmatched) was load-bearing, not paranoid.

Phase 10 is the same shape: a Phase 9 design assumption — that status sync
is correctly modeled as "a per-poll walk over rows the progress path already
classified as eligible" — was inherited from the progress path without an
explicit check that *status-eligible rows have any reason to also be
progress-eligible*. They don't, and the user's real-world scenario
(download OPDS → mark finished without opening) is exactly the case where
they don't. Each of these invalidations came from the same root: an inherited
inference from an upstream design (the Lua plugin's reading, the prior
phase's assumptions) was treated as settled when it was actually unverified
behavior, and only live behavior surfaced the gap.

The Phase 10 ADR is recorded in the same style as the Addenda:
describe-what-was-decided, describe-what-the-live-server-demonstrated,
describe-what-replaces-it-and-why, name the §scope-of-fix, name the test that
guards the regression going forward. The pattern is now established enough
that a future Phase 11 (likely annotations, see
`docs/annotation-sync-feasibility.md`) should anticipate paying the same
live-validation tax the same way.
