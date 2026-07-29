# Phase 6 — Decision Record

**Status:** VERIFIED. Independent verification pass confirms the implementation in `internal/sync/engine.go` faithfully matches the approved design (`docs/phase-6-design.md`). All nine flagged decisions (A–I) are implemented exactly as approved. The build and the full test suite pass (`go vet ./...`, `gofmt -l .`, `go test ./...`, `go test -race ./...`).

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
- MatchCandidate: Source="readest", LastOpen=updated_at in Unix seconds,
  MetadataAmbiguous=false, exactly as approved.
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
     `MatchCandidate` field values the engine builds (Source="readest",
     MetadataAmbiguous=false, LastOpen = updated_at in Unix seconds) — the
     bridge-specific values approved in decision E.
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
