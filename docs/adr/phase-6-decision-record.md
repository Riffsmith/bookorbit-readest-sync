# Phase 6 — Decision Record

**Status:** VERIFIED. Independent verification pass confirms the implementation in `internal/sync/engine.go` faithfully matches the approved design (`docs/phase-6-design.md`). All nine flagged decisions (A–I) are implemented exactly as approved. The build and the full test suite pass (`go vet ./...`, `gofmt -l .`, `go test ./...`, `go test -race ./...`).

**Post-verification follow-ups (both resolved):** Two live-server invalidations were uncovered by the first end-to-end run and resolved in this record — Addendum 1 (`MatchCandidate.Source` enum) and Addendum 2 (the "hash absent from match-check response" handling). Both are fixed; `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...`, and `go test -race ./...` all still pass.

Phase 6 is complete; the project is ready to proceed to Phase 7 (`cmd/bridge` packaging + signal-hardened lifecycle) per `docs/implementation-roadmap.md`.

---

Phase 6 is implemented per the approved design (docs/phase-6-design.md).

## What changed

**internal/sync/engine.go** — full implementation replacing the foundation-phase
stub:

- RunOnce: pulls since the persisted watermark, classifies every row (dummy /
  deleted / normal), resolves unknown hashes via batched MatchCheck, pushes
  changed percentages via batched BulkProgress (or per-item UpdateProgress once
  bulk is discovered unsupported), and calls state.Store.Save() exactly once
  at the end regardless of outcome.
- Watermark: computeWatermark implements the approved retreat rule — advance to
  the max WatermarkMs across all non-dummy rows, or retreat to
  min(failed rows) - 1 if any batch ultimately failed, floored at the
  pre-existing watermark.
- Deleted rows: DeleteMatch + ClearUnmatched, no BookOrbit contact.
- MatchCandidate: Source="file", LastOpen=updated_at in Unix seconds,
  MetadataAmbiguous=false. (Originally approved as Source="readest"; see the
  Addendum below for the live-server invalidation and replacement.)
- Bulk fallback: an in-memory-only bulkUnsupported bool, flipped on the first
  bookorbit.ErrUnsupportedEndpoint from BulkProgress and never persisted or
  reset; every subsequent push (this RunOnce and every later one on the same
  Engine) goes through UpdateProgress instead.
- Unmatched recheck: purely time-based against config.Bridge.UnmatchedCooldown;
  no "new activity" comparison, matching the shipped state/config schema.
- Auth failures (readest.ErrUnauthorized on pull, bookorbit.ErrUnauthorized on
  match-check/push): abort the remainder of RunOnce immediately, preserve
  whatever state mutations already committed this pass, still call Save(),
  and return the wrapped error — no rollback.
- Retry/backoff: one small withRetry helper plus two package-level classifiers
  (classifyReadestErr, classifyBookOrbitErr) mapping each client's sentinels to
  outcomeSuccess/Retry/Fatal/Skip. Only outcomeRetry backs off (exponential,
  capped, governed by the already-shipped config.Bridge.Retry* fields);
  outcomeFatal/outcomeSkip never retry. context.Canceled/DeadlineExceeded are
  classified fatal so shutdown propagates immediately rather than waiting out
  a doomed retry loop.
- Full-library recheck (libraryVersion/needsFullRecheck) is out of scope, as
  approved — the existing UnmatchedCooldown is the only recheck mechanism.
- sync.ErrNotImplemented removed (no method returns it).

**cmd/bridge/main.go / cmd/bridge/device.go** — module path updated; the
dead `errors.Is(runErr, sync.ErrNotImplemented)` checks that existed in the
foundation-phase `run()` (for both the `--once` and daemon branches) are gone,
leaving only the `errors.Is(runErr, context.Canceled)` daemon-exit check.

**go.mod and every internal/cmd import** — module path changed from the
`github.com/user/...` placeholder to `github.com/Riffsmith/bookorbit-readest-sync`.

**internal/sync/engine_test.go** — rewritten with hand-written fakes for
readest.SyncClient and bookorbit.API (both plain interfaces, no test-double
machinery needed), covering: empty/dummy pulls, fresh-match-always-pushes
(including the pct==0 edge case), unchanged-vs-changed percentage, deleted-row
reset, unmatched cooldown (both within and past), match-check batch failure
retreating the watermark, auth failure aborting without mutating state, the
bulk→UpdateProgress fallback (including that it stays fallen back across a
second RunOnce and never re-probes BulkProgress), a pure computeWatermark
table test, Run respecting context cancellation mid poll-sleep, and Run
surviving a RunOnce failure to continue looping.

## Deviations from the design

None. All nine flagged decisions (A–I) were approved as proposed and
implemented as specified.

## Verification pass

A verification pass (`go vet ./...` clean, `gofmt -l .` clean, `go test ./...`
and `go test -race ./...` green) confirms the shipped engine matches the
approved design. The pass surfaced three incomplete items, all corrected with
no engine-behavior change (i.e., the engine logic was already faithful; what
was missing was the agreed housekeeping and the §12 testing-strategy coverage):

1. **Module-path housekeeping was only partially applied (Decision I).** The
   Phase 6 files (`internal/sync/engine.go`, `internal/sync/engine_test.go`)
   had been rewritten to the `github.com/Riffsmith/...` path, but `go.mod`
   still declared `module github.com/user/bookorbit-readest-sync` and every
   other `.go` file still imported the old placeholder — so the module did
   not build. The full repo-wide find-and-replace was applied: `go.mod` and
   every import (production and test, across `cmd/bridge`,
   `internal/readest`, `internal/bookorbit`, `internal/config`,
   `internal/sync/state`, `internal/util`, `internal/util/httpclient`,
   `internal/token`) now use `github.com/Riffsmith/bookorbit-readest-sync`.
   `rg "github\.com/user" --glob '*.go' --glob 'go.{mod,sum}'` returns no
   matches.

2. **Stale foundation-phase comments contradicted shipped behavior (design §0
   required sweeping any such leftovers before Phase 6 landed).** Three
   comments still described the engine as a "stub" that "performs no network
   sync" and "reports that the engine is not yet implemented," despite Phase 6
   having shipped the real pull→match→push loop. These were corrected in
   `cmd/bridge/main.go` and `internal/readest/doc.go` to describe the engine
   as built, with no behavior change.

3. **Test coverage gaps against design §12.** The shipped
   `internal/sync/engine_test.go` covered the bulk of §12 but was missing
   cases 7, 12, 14, 17, 20, 21, and 22, and the partial coverage on cases 3,
   4, 5, and 8 did not assert the design's invariants. The missing and partial
   cases were added/strengthened (test-only; no engine change because the
   engine logic was already correct):

    - Case 3 (`TestRunOnceFreshMatchAlwaysPushes`) now also asserts the
      `MatchCandidate` field values the engine builds (Source="file",
      MetadataAmbiguous=false, LastOpen = updated_at in Unix seconds) — the
      bridge-specific values approved in decision E. (Source was originally
      "readest"; see the Addendum below.)
   - Cases 4 & 5 newly assert that the watermark still advances on a
     seen-but-unchanged row, and that `LastPushedAt` is refreshed after a
     changed push.
   - Case 7 (`TestRunOnceUnusableProgressSkippedButWatermarkAdvances`) covers
     a row with a zero-total progress tuple being skipped while the watermark
     still advances.
   - Case 8 (`TestRunOnceUnmatchedPastCooldownRechecks`) now also asserts no
     push, the new `Unmatched` cooldown timestamp is recorded, the watermark
     advances, and the next `RunOnce` (now within cooldown) does not recheck.
   - Case 12 (`TestRunOnceBulkProgressFailureHoldsPctAndRetreatsWatermark`)
     symmetric to the existing match-check failure test: a retryable
     BulkProgress failure holds `LastPushedPct` and retreats the watermark.
   - Case 14 (`TestRunOnceReadestAuthFailureAbortsBeforeProcessing`) covers a
     Readest pull auth failure aborting RunOnce before any processing with no
     state mutation.
   - Case 17 (`TestWithRetry*`) is a pure-function suite for `withRetry`:
     attempt count (initial + RetryMaxAttempts), delay growth and cap,
     outcomeFatal/Skip never retrying, and cancellation mid-backoff
     aborting immediately.
   - Case 20 (`TestRunOnceSavesExactlyOnceOn{Success,BatchFailure,BookOrbitAuthAbort}`)
     asserts `state.Store.Save()` is called exactly once per RunOnce
     regardless of outcome (Decision C), via a counting decorator.
   - Case 13's deeper half (`TestRunOnceBookOrbitAuthAbortPreservesPriorChunks`)
     covers a successful prior chunk's state being preserved when a later
     chunk hits a BookOrbit auth failure, plus Save still running (Decision H).
   - Case 21 (`TestRunOncePercentageToleranceBoundary`) documents the actual
     `<=` boundary on the 5-decimal-place grid the rounding imposes: deltas
     under 0.001 are skipped, exactly 0.001 is pushed (FP error makes 0.001
     evaluate to > 0.001, mirroring the reference plugin's own double math),
     and 0.0011 is pushed.
   - Case 22 (`TestRunOnceHashReappearsAfterDeletionIsBrandNew`) covers a hash
     that reappears after deletion being treated as brand-new, with no stale
     `LastPushedPct` bleed-through.

### Things deliberately left unchanged during verification

- `internal/readest/auth.go` retains an unused `readest.ErrNotImplemented`
  leftover from the foundation phase. Phase 6 design §3 only *documents the
  history* of Phase 4's removal; it does not require Phase 6 to touch the
  already-accepted Phase 4 package. Removing it is an optional cleanup outside
  Phase 6's scope, so it was left in place.

---

## Addendum — live-server invalidation of Decision E (`MatchCandidate.Source`)

**Date discovered:** 2026-07-30  
**Source of evidence:** First end-to-end live run of the bridge against a real
BookOrbit server.

### What was originally decided

Decision E (docs/phase-6-design.md §6.2, approved in this record above)
chose `MatchCandidate.Source = "readest"` as the value the bridge sends for
every candidate it submits to `POST /koreader/plugin/match-check`. The
rationale was that the plugin uses `"current_file"`/`"statistics"`/`"file"`
depending on which live KOReader subsystem produced the candidate, none of
which apply to a headless bridge (docs/phase-6-design.md §6.2 and §14 row
"Source"), and a bridge-specific value would make server-side diagnostics
more readable. The design explicitly anticipated that a different value might
be preferable (§14 decision-E approval note: "Alternative values welcome if a
different `Source` string is preferred for BookOrbit-side diagnostics") but
the chosen value was never validated against the live server's behavior —
exactly the gap Phase 5's design (docs/phase-5-design-temp.md §16
live-validation item) and the reverse-engineering report had flagged.

### What the live server demonstrated

The first live run of:

```
BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config configs/bridge.yaml
```

authenticated to Readest, pulled the books table, hashed the rows, and
dispatched the first `match-check` batch. BookOrbit rejected the request
with HTTP 400 and the body:

```json
{"statusCode":400,"message":[
  "books.0.source must be one of the following values: current_file, file, statistics",
  "books.1.source must be one of the following values: current_file, file, statistics",
  ...
]}
```

This proves:

1. BookOrbit's server validates `MatchCandidate.Source` against a fixed
   server-side enum of exactly three values: `current_file`, `file`,
   `statistics`. No other strings are accepted.
2. The original design assumption — that `"readest"` could be used as a
   bridge-specific value for diagnostics — is **incorrect**. The server
   rejects any non-enum value at the request-validation layer, before the
   match logic can run. The 400 is returned for *every* candidate in the
   batch (each `books.N.source` is listed in the validation error array),
   confirming the rejection is a hard schema-check, not a soft warning.

The wire-shape assumption (Phase 5 ADR §4.2c, verified — books-as-array,
device-wrapped, with `lastOpen`/`source`/`metadataAmbiguous` per candidate)
was *correct*: the server parsed the body and walked into the per-item
`source` field to validate it. Only the *value* of `source` was wrong.

### What replaces it and why

`MatchCandidate.Source` is now constant `"file"`.

Evidence that `"file"` is the correct choice among the three enum values the
server accepts:

1. **Direct precedent in the reference plugin.**
   `reference/koreader-plugin/bookorbit.koplugin/bookorbit_catalog_download.lua:335`
   constructs a match-check candidate with `source = "file"` for a
   just-downloaded catalog file whose only resolvable identity is a partial
   MD5 hash plus the metadata derived from the catalog. That candidate has
   no live reader context (not `"current_file"`), no KOReader stats database
   row (not `"statistics"`), and structurally is exactly what the bridge
   submits: "hash + title + authors + lastOpen, match it please." A
   Readest-backed bridge candidate is the same shape.

2. **Negative fit of the alternatives.**
   - `"current_file"` is reserved by the plugin
     (`bookorbit_book_sync.lua:242`, `main.lua:786`) for the one book
     currently open in KOReader's `self.ui.document`. The bridge runs
     headless with zero open documents, so labelling any candidate as
     "currently open" is the strongest *false* claim about bridge state.
   - `"statistics"` is used by the plugin
     (`bookorbit_sweep.lua:200`) only when a candidate is sourced from
     `statistics.sqlite3` (KOReader's reading-stats database). The bridge
     has no such database; sending `"statistics"` would falsely claim a
     stats-DB provenance the bridge does not possess.
   - `"file"` is the value the plugin itself uses when a book is identified
     by a hash + an on-disk file with sidecar metadata
     (`bookorbit_sweep.lua:224` — overriding the default `"statistics"` when
     a file is also present) and, more importantly, when only the hash +
     metadata is available (`bookorbit_catalog_download.lua:335`). The
     bridge's situation is the latter case.

3. **The plugin never branches on `source` after submission.** A grep of
   the match-check response handling
   (`bookorbit_sweep.lua:323-326`, `bookorbit_catalog_download.lua:343-346`,
   `bookorbit_book_sync.lua:251-258`, `main.lua:796-803`) confirms the
   server returns `{hash, bookFileId, bookId}` per match and *never echoes
   `source` back*, and no plugin code consumes `source` from a response.
   So `source` is purely server-side input metadata (likely used for
   audit / matching heuristics on the BookOrbit side) and not a value the
   caller needs to round-trip on. The impact of the choice is limited to
   what the server records about the candidate's origin.

### Acknowledged imperfect literal fit

`"file"` does name a thing the bridge does not possess — the bridge has no
on-disk file at all for any candidate; it has only the Readest sync row.
This is recorded here so the trade-off is not silently re-litigated. The
choosing principle is *least-misleading claim among the available enum
values*: the bridge never has a live document (`"current_file"`) and never
has a statistics-DB row (`"statistics"`); the plugin's own
`bookorbit_catalog_download.lua:335` treats `"file"` as the value for a
"hash-resolvable metadata candidate without live-reader or stats-DB
context," which is exactly the bridge's situation. `"file"` is therefore
the BookOrbit-enum value whose semantic least exaggerates a headless
bridge's actual state.

### Other MatchCandidate fields — separate review, no change

The live-server evidence **only** disproves the `Source="readest"` value.
Two other `MatchCandidate` fields were Phase 6 bridge-specific assumptions
that had also never been validated:

- **`MetadataAmbiguous`** (bridge sends `false`): the server treats this
  as a boolean and validates only its presence/type, not an enum. `false`
  is structurally correct and the live-server 400 did not name it. No
  change. Live confirmation is implied by the next successful match-check
  request.

- **`LastOpen`** (bridge sends `updated_at` as Unix seconds): the plugin
  also sends Unix seconds for the same wire field, so the unit, name, and
  shape are consistent. The live-server 400 did not name it. No change.
  Live validation will be confirmed by the next successful match-check.

Per the brief — "identify but do not change unless evidence shows it is
also incorrect" — these are flagged here for completeness but left
untouched. The single live-server 400 named only `source`; only `source`
was changed.

### Scope of the code change

The fix is exactly one line in `internal/sync/engine.go` (the
`Source: "readest"` literal becomes `Source: "file"`), plus the matching
fixtures in `internal/sync/engine_test.go` and
`internal/bookorbit/models_test.go` and the doc-comment on
`MatchCandidate.Source` in `internal/bookorbit/models.go`. No engine logic
changes; no client wire-shape changes; no other `MatchCandidate` field
changes.

### Why this isn't a re-approval of the rest of Phase 6

This addendum invalidates a single bridge-specific value. It does not
revisit the rest of Decision E (`LastOpen` = `updated_at` in Unix seconds;
`MetadataAmbiguous` = `false`), and it does not revisit any other Phase 6
decision (A–D, F–I) or any Phase 5 client-design decision. Those remain
approved and (where source-verifiable) verified, exactly as recorded
elsewhere in this document.

---

## Addendum 2 — live-server invalidation of §6.3 "hash absent from match-check response" handling

**Status:** **Resolved.** Investigated, diagnosed, and — after the
Phase 6 follow-up described below — fully implemented. The fix matches
both the reference plugin (`bookorbit_sweep.lua:328-332`) and the
verified live BookOrbit server behavior. See "Resolution" at the end
of this addendum for the full implementation record.

**Date discovered:** 2026-07-30
**Source of evidence:** Same first end-to-end live run as Addendum 1,
immediately after the `Source="readest"` → `"file"` fix was applied. The
run succeeded in matching and pushing the one book that had been read to
3% (verified visually in the BookOrbit dashboard) but emitted ~98
`WARN sync: hash absent from match-check response` lines, one per
Readest-owned book that BookOrbit's library did not know about.

### What was originally decided

`docs/phase-6-design.md` §6.3 (Match-check result handling), bullet 3:

> Any hash that was in the request but appears in **neither** list (defensive
> — should not happen per the documented contract, but the client normalizes
> nil to empty slices rather than guaranteeing coverage): treated as a
> batch-level failure for that hash for the purposes of watermark retreat
> (§5.4), logged at `warn`.

Translated to shipped behavior at `internal/sync/engine.go:242-247`:

```go
for _, h := range chunk {
    if !seen[h] {
        e.log.Warn("sync: hash absent from match-check response", "hash", h)
        failedWatermarks = append(failedWatermarks, rowByHash[h].WatermarkMs())
    }
}
```

Because `failedWatermarks` is consumed by `computeWatermark`
(`engine.go:289-303`) as input to the retreat rule, every "absent" hash
causes the persisted watermark to retreat to `min(failed) - 1` (floored at
the pre-existing watermark).

### What the live server demonstrated

Across the ~99-book match-check batch in this run, the BookOrbit server
returned exactly the one book that was a real library match in
`resp.Matches`. **Zero** hashes were returned in `resp.Unmatched`. Every
other book — ~98 of them, all of them books the user has in Readest but
BookOrbit's library had never seen — appeared in **neither** list. The
bridge then logged `WARN` for each one and appended each row's
`WatermarkMs` to `failedWatermarks`.

Three compounding operational consequences were observed or reasoned from
the engine code:

1. **Watermark stall and re-pull storm.** Because every absent hash is
   treated as a batch-level failure, `computeWatermark` retreats to
   `min(failedWatermarks) - 1`. The "absent" set is stable across polls
   (the same ~98 Readest-only books will keep not matching, since the
   operator has not uploaded them to BookOrbit), so the watermark cannot
   advance past their shared minimum `updated_at`. Every subsequent poll
   re-pulls the same set from Readest and re-submits the same ~98 hashes
   to `match-check`, generating ~98 `WARN` lines per `poll_interval`
   (default 15 min) in perpetuity. A daemon run, absent a fix, would
   emit ~9,500 such warnings per day per install.

2. **Divergence from the reference plugin's `match-check` handling.** The
   plugin's structurally equivalent code
   (`reference/koreader-plugin/bookorbit.koplugin/bookorbit_sweep.lua:322-332`,
   function `stepMatchNext`) handles the same case with the opposite
   semantics:
   ```lua
   local matched = {}
   for _, match in ipairs(body.matches or {}) do
       matched[match.hash] = true
       ctx.state:setMatched(match.hash, match.bookFileId, match.bookId, ...)
   end
   for _, md5 in ipairs(batch) do
       if not matched[md5] then
           ctx.state:setUnmatched(md5)   -- absent from matches == unmatched
       end
   end
   ```
   The plugin **only consults `body.matches`** for this decision; it never
   reads `body.unmatched` in the match phase. `body.unmatched` is consumed
   only later (`sweep.lua:631,678,738`) for the *bulk-progress* response,
   to invalidate books that were previously matched but which BookOrbit
   can no longer resolve. The plugin treats "absent from `matches`" as the
   authoritative "unmatched" signal in the *match-check* phase, exactly
   the opposite of the bridge's "treat as failure and retreat the
   watermark" behavior.

3. **State cache never converges.** The bridge never calls
   `st.SetUnmatched(hash, now)` for any of the ~98 absent books. So they
   are not subject to the `UnmatchedCooldown` (24h default) recheck
   gating — the engine has no way to know they are *known-unmatched*
   rather than *never-checked*. They will be submitted again on the very
   next poll (per §6.1's "fresh hash" trigger: any hash without a
   `MatchRecord` and without a recent `UnmatchedAt` is treated as fresh
   and must be checked). A daemon left running will therefore re-do
   this work forever, never populating the unmatched cooldown that the
   Phase 6 design's recheck path is keyed off.

### Why the original design was wrong

The §6.3 line-205 design note described the absent-from-both case as
"defensive — should not happen per the documented contract." That
characterization was inherited from `docs/phase-5-design-temp.md` §4.2c
which itself was an inference from the plugin's behavior, *not* a
verification against a live BookOrbit server. The two were conflated:

- The **wire response shape** ("`unmatched` is the field the server uses
  to report unmatched hashes") was verified against the plugin's
  consumptions and is correct.
- The **server's operating behavior** ("the server always reports every
  un-matched request hash in `unmatched`") was never verified against a
  live server, and the plugin's source never claimed it — `stepMatchNext`
  precisely works around the opposite reality by treating *any* hash not
  in `body.matches` as unmatched, regardless of `body.unmatched`'s
  contents.

The live server's behavior now confirms the plugin's defensive pattern
was load-bearing, not paranoid: BookOrbit's server typically returns
zero hashes in `unmatched` for hashes it has no record of at all, and
reserves `unmatched` for a narrower subset (likely: hashes recently
considered but explicitly not-matched, or hashes the server *did* know
about but has since lost). The plugin's defensive pattern handles both
contracts; the bridge's "treat absent as failure" only works under the
contract that turns out not to hold.

### What the correct behavior is and why

The engine's match-check result handler should treat any hash absent from
both `resp.Matches` and `resp.Unmatched` as **unmatched** — exactly what
the reference plugin does — by calling `st.SetUnmatched(hash, now)` and
`st.DeleteMatch(hash)`, *without* appending the row's watermark to
`failedWatermarks` and *without* logging a warning. Concretely, in
`internal/sync/engine.go`, the post-loop at lines 242-247 should be
restructured to:

- For each hash in `chunk` not in `seen`:
  - `e.st.SetUnmatched(h, now.Unix())`
  - `e.st.DeleteMatch(h)`
  - *Not* append to `failedWatermarks`
  - *Optionally* log at `debug` (not `warn`) for visibility, but the
    situation is normal, not exceptional.

Supporting evidence for this being the correct fix:

1. **Direct plugin precedent.** `bookorbit_sweep.lua:328-332` -
   authoritative; the plugin ships this exact pattern.
2. **Restores the `UnmatchedCooldown` invariant.** Once a hash is in
   `state.Store`'s unmatched set, §6.1's recheck-gating kicks in (a hash
   in cooldown is not re-submitted to `match-check` until the cooldown
   expires, default 24h). The storm stops: ~98 hashes settle into
   unmatched-cache once, then are re-checked at most once per day.
3. **Watermark advances normally.** Removing the absent hashes from
   `failedWatermarks` means `computeWatermark` advances to the row
   max `WatermarkMs` as designed (Decision A), rather than retreating
   to the absent-set's `min - 1` forever. Polls become near-instant
   "nothing changed" passes after the first one, exactly as Phase 6
   intended (§12 design case 5: "Already-matched, percentage unchanged
   ... watermark still advances").

### Scope of the fix (planned; not yet applied)

- `internal/sync/engine.go:242-247` (match-check result handler):
  behavior change as described above.
- `internal/sync/engine_test.go`: the existing test fixtures that
  populate `resp.Unmatched` for every "not found" case (cases 3, 8
  and likely others) must be re-examined; cases that intentionally
  exercise "server reports unmatched via `body.unmatched`" should
  still pass, but cases that were implicitly relying on the wrong
  "absent == failure" semantics will need new expectations (the absent
  hash should now land in `state.Store.Unmatched`, not in
  `failedWatermarks`). New explicit cases for the absent-from-both
  path must be added — these are the regression guard for this bug
  going forward.
- `docs/phase-6-design.md` §6.3 bullet 3: rewrite from "defensive -
  should not happen per the documented contract" to reflect the
  live-server reality ("the server omits unknown hashes from both
  lists in the common case; treat absence as unmatched, per
  `bookorbit_sweep.lua:328-332`").

### Why this blocks Phase 7 (recorded here for traceability)

`docs/phase-7-design.md:7` opens with the premise *"Phase 6 is treated
as complete and correct. Nothing here revisits Phase 6's
retry/watermark/fallback decisions."* Phase 6 is no longer complete:
the match-check result handler has the behavioral defect documented
above. Two specific Phase 7 sections are directly affected and would
be authored over a wrong foundation if the bug is not fixed first:

- **Phase 7 §8.2 item 9** proposes new `cmd/bridge` unit tests. These
  will sit on top of `engine_test.go`, which has *no* test for the
  "absent from both lists" path (verified: `rg "hash absent|absent
  from match-check" internal/sync/engine_test.go` returns no matches).
  Any new tests written now would inherit the untested/incorrect
  contract and pass while the daemon emits 9,500 warnings/day.
- **Phase 7 §7.3**'s error-classification table claims
  `Restart=on-failure` is essentially a safety net that "almost never
  fires" because the daemon absorbs transient failures itself. This
  reasoning holds *only* when the engine's state converges; under the
  current "absent == failure" bug the daemon's persisted state never
  converges (watermark keeps retreating) and §7.3's operator-facing
  classification is being written over a non-convergent engine.

The fix is in Phase 6 scope (engine logic + engine tests + Phase 6
design §6.3), not Phase 7. Per the current instruction ("Don't touch
any code"), this addendum records the finding and the planned fix
without applying it; a corresponding blocker banner has been added to
`docs/phase-7-design.md` so the Phase 7 design does not silently rest
on the wrong foundation.

---

### Resolution (2026-07-30)

**Status:** **Implemented.** The planned fix in "Scope of the fix" above
has been applied exactly as specified, with one minor implementation
detail (the merged-loop form) called out below. The Phase 7 blocker in
`docs/phase-7-design.md` has been lifted accordingly.
`go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...`, and
`go test -race ./...` all pass.

#### Code

`internal/sync/engine.go` — the match-check result handler was restructured
to route the absent-from-both case through the same `SetUnmatched` +
`DeleteMatch` handling the explicit `resp.Unmatched` list already used,
removing the old `Warn` + append-to-`failedWatermarks` path entirely:

```go
// Per docs/adr/phase-6-decision-record.md Addendum 2: the live
// BookOrbit server does not echo unknown hashes in
// resp.Unmatched — it omits them from both lists. Any hash in
// the request that didn't appear in resp.Matches is therefore
// unmatched, exactly as the reference plugin treats it
// (bookorbit_sweep.lua:328-332: "if not matched[md5] then
// setUnmatched(md5)"). Hashes the server does return in
// resp.Unmatched get the same treatment; the two cases are
// semantically indistinguishable, so they share one loop that
// populates seen first, then marks any remaining chunk hash
// unmatched.
for _, h := range resp.Unmatched {
    seen[h] = true
    e.st.SetUnmatched(h, now.Unix())
    e.st.DeleteMatch(h)
}
for _, h := range chunk {
    if !seen[h] {
        seen[h] = true
        e.st.SetUnmatched(h, now.Unix())
        e.st.DeleteMatch(h)
        e.log.Debug("sync: hash unmatched by bookorbit", "hash", h)
    }
}
```

**Minor implementation divergence from the planned wording (worth
recording, **not** a behavior change):** the "Scope of the fix" prose
said "for each hash in chunk not in `seen`" should `SetUnmatched` +
`DeleteMatch`; the merged form keeps **two loops** (one over
`resp.Unmatched`, one over `chunk`) so that hashes the server *does*
echo in `resp.Unmatched` get processed exactly once in their own loop
and skip the chunk loop via the `seen` guard, and hashes the server
omits get processed exactly once by the chunk loop. A single combined
loop would have been subtly incorrect: it would require either skipping
`resp.Unmatched`-listed hashes in the chunk loop (same outcome, more
reads) or letting `resp.Unmatched`-listed hashes be processed only by
the chunk loop (works but loses the live-server-informed attribution,
and `state.Store` callers would be unable to distinguish the two paths
if a future debug log wanted to). The dual-loop form is the cleanest
expression of "both paths funnel into the same state mutation, but each
hash lands in exactly one branch."

The `e.log.Debug(...)` log line appears only in the **absent-from-both**
branch, not the `resp.Unmatched` branch — by design. The
explicit-unmatched path is the server's normal "I checked and have no
match" signal and was already silent pre-fix; logging it at `debug`
would add no diagnostic value. The absent path is the path the original
contract mischaracterized, so it carries the one new `debug` line for
the rare case an operator wants to confirm "yes, BookOrbit had nothing
to say about this hash." The pre-fix `warn`-level line is gone — this
is the operational win, eliminating the ~98/per-poll warning storm
(~9,500/day for the test account).

**No other engine.go change.** The genuine-failure path (line 217,
reachable only when `err != nil` from `e.matchCheck`), the
auth-failure abort, the bulk-progress push phase, and the watermark
computation all remain exactly as approved. `computeWatermark`'s
retreat rule is unchanged; its input `failedWatermarks` now contains
only the rows whose batch genuinely failed (transport error after
retry-exhaustion), which is precisely what Decision A intended.

#### Tests

`internal/sync/engine_test.go` — one new test added, no existing tests
modified:

- **`TestRunOnceAbsentFromMatchResponseIsUnmatchedNotFailure`** — the
  regression guard Addendum 2 required. Exercises the absent-from-both
  path with a fresh hash (no prior `MatchRecord`, no prior
  `UnmatchedAt`) against a `fakeBookOrbit` whose `matchResp` is the
  zero value (`Matches` and `Unmatched` both empty — the live-server
  behavior the original contract mischaracterized). Asserts:
  1. `matchCalls == 1` (fresh hash reaches match-check).
  2. `bulkCalls == 0` (no push for an unmatched hash).
  3. `UnmatchedAt("h1")` exists with a non-zero timestamp (hash
     settled into the cooldown recheck gate).
  4. `Match("h1")` returns `ErrNotFound` (defensive `DeleteMatch`
     leaves no stale match record).
  5. `Watermark()` equals exactly `row.WatermarkMs()` — *not*
     `WatermarkMs() - 1` as the pre-fix bug would have produced. This
     1ms-level assertion is the precise behavioral discriminator
     between the correct "absent == unmatched → watermark advances"
     contract and the wrong "absent == failure → watermark retreats"
     contract; it's deliberately tight so a regression silently
     re-introducing the old code would fail this test by exactly 1ms.
  6. A second `RunOnce` (now within the freshly established 24h
     cooldown) makes zero match-check calls — confirming the per-poll
     re-pull/re-match storm is gone.

  This test was verified to *fail* against the pre-fix engine code
  (reverted temporarily during implementation, then restored): under
  the old engine, the absent hash never enters `UnmatchedAt` (the old
  `Warn + failedWatermarks` path didn't call `SetUnmatched`), so
  assertion (3) fails first. This confirms the test isn't vacuously
  green — it actually exercises the regression.

**No existing tests modified.** All pre-existing tests pass unchanged
because, as the audit found, none of them actually exercised the
absent-from-both path: `TestRunOnceFreshMatchAlwaysPushes` populated
`Matches`, `TestRunOnceUnmatchedPastCooldownRechecks` populated
`Unmatched`, and every other match-check-touching test pre-populated a
`MatchRecord` so the row skipped the match-check phase entirely. The
wrong contract — exactly as Addendum 2 noted — was untested; the new
test closes that gap.

#### Documentation

- `docs/phase-6-design.md` §6.3 — the original "defensive — should
  not happen" bullet 3 was rewritten to merge the two cases (explicit
  `resp.Unmatched` and omitted-from-both) into a single bullet with
  the verified behavior, citing `bookorbit_sweep.lua:328-332`,
  `SetUnmatched`/`DeleteMatch`, watermark-advance (not retreat), the
  `UnmatchedCooldown` settlement, and the `debug` (not `warn`) log
  level. The §12 case 8 description was updated to cover both the
  explicit-list and absent-from-both shapes, naming the two tests that
  guard them.
- This addendum's "Scope of the fix" wording is left intact above as a
  record of what was planned; this Resolution section records what was
  done. The two are consistent except for the minor dual-loop
  implementation detail called out above, which is documented
  in-context inside `engine.go` itself.
- `docs/phase-7-design.md` — the BLOCKED banner placed under line 7 is
  replaced with a RESOLVED note pointing back to this section, so the
  Phase 7 design now rests on the corrected foundation.

#### Verification (recorded for next-phase author)

```
$ go build ./...        # clean
$ go vet ./...          # clean
$ gofmt -l .            # no files reported
$ go test ./...         # all packages green
$ go test -race ./...   # all packages green
```

Live re-run by an operator is the only remaining non-automated
verification step (the prior test run was the one that surfaced the
bug); the next `BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config
configs/bridge.yaml` invocation is expected to produce one `INFO
bridge starting` line followed by a small number of `DEBUG sync: hash
unmatched by bookorbit` lines for every Readest-only book on the first
poll, then near-silent second and subsequent polls (watermark advanced,
all previously-absent hashes now in cooldown). Zero `WARN` lines should
mention "hash absent" or "match-check response."
