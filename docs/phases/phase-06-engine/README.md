# Phase 6 — Sync Engine

**Status:** Complete and verified. Phase 6 is the load-bearing phase of the bridge — the `pull → classify → match-check → push → save` orchestration that ties Phases 3–5 together.
**Design:** [design.md](./design.md)
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`internal/sync/engine.go`](../../../internal/sync/engine.go), [`internal/sync/engine_test.go`](../../../internal/sync/engine_test.go), [`cmd/bridge/main.go`](../../../cmd/bridge/main.go), `go.mod` (module-path move to `github.com/Riffsmith/bookorbit-readest-sync`).
**Depends on:** [Phase 3 Auth](../phase-03-auth/README.md), [Phase 4 Readest Client](../phase-04-readest-client/README.md), [Phase 5 BookOrbit Client](../phase-05-bookorbit-client/README.md), Phase 2 state store.
**Unblocked by this phase:** [Phase 7 — CLI + Packaging](../phase-07-cli-packaging/README.md)

## What this phase shipped

The engine: `RunOnce` runs `EnsureTokenFresh → PullBooks → classify rows (dummy/deleted/normal) → batched MatchCheck (500/batch) → batched BulkProgress (100/batch, fallback to per-item UpdateProgress on `ErrUnsupportedEndpoint`) → advance/retreat watermark → Save() once`. `Run` wraps `RunOnce` in a poll loop with graceful `context.Canceled` exit. Retry policy is owned exclusively by the engine via `withRetry` + two outcome classifiers (`classifyReadestErr`/`classifyBookOrbitErr`); clients never retry on their own.

## What shipped DIFFERENTLY from the design

> ⚠️ The two addenda below **rewrite §6.2 and §6.3 of [design.md](./design.md)**. The design doc's text is preserved unchanged (per the project's "design docs are historical" convention); the corrected contract lives in the ADR.

- **[Addendum 1 — live-server invalidation of Decision E (`MatchCandidate.Source`)](./decision-record.md#addendum-1--live-server-invalidation-of-decision-e-matchcandidatesource).** The design chose `Source="readest"` for diagnostics; the live BookOrbit server 400-rejects any value outside the enum `{current_file, file, statistics}`. After live-server evidence, `Source="file"` was shipped (direct precedent in `bookorbit_catalog_download.lua:335`, and the least-misleading enum value for a headless bridge with no live document and no statistics-DB row).

- **[Addendum 2 — live-server invalidation of §6.3 "hash absent from match-check response" handling](./decision-record.md#addendum-2--live-server-invalidation-of-63-hash-absent-from-match-check-response-handling).** The design said a hash absent from both `Matches` and `Unmatched` "should not happen — treat as batch failure, retreat watermark." Live BookOrbit behavior: the server *omits* unknown hashes from both lists; the plugin's own `bookorbit_sweep.lua:328-332` treats this as the matched-against unmatched signal. The shipped engine now calls `SetUnmatched`+`DeleteMatch` for both absent and explicit-unmatched paths, the **watermark advances** (not retreats), and a `debug` (not `warn`) line fires for the absent case. This eliminated a ~9,500-per-day warning storm the old contract would have produced. Regression guard: [`TestRunOnceAbsentFromMatchResponseIsUnmatchedNotFailure`](../../../internal/sync/engine_test.go) (asserts the exact 1ms watermark-advance boundary).

- **Phase 7 follow-up banner** on the design was lifted once the fix shipped (was previously a "RESOLVED" note pointing back to this ADR's Resolution section).

## Live-validation items this phase opened

Both Phase 6 addenda are themselves the product of live-validation runs (Tests 1–5 in [`../../live-test-reports.md`](../../live-test-reports.md)). Two remaining items L1 (SIGTERM signal handling) and L4 (live token refresh over >30 min) are tracked for Phase 8 ([`../../live-validation-status.md`](../../live-validation-status.md)).

## Tests

[`internal/sync/engine_test.go`](../../../internal/sync/engine_test.go): the full Phase 6 design §12 22-case matrix plus the Addendum 2 regression test, all driven by hand-written `fakeBookOrbit`/`scriptedBookOrbit` satisfying the existing interfaces (no mocking framework). Pure-function `computeWatermark` table test. `withRetry`'s own behavior (attempt count, exponential cap, fatal/skip never retry, cancellation mid-backoff). Two concurrency tests via `-race`. Phase 6 verification pass: `go vet`, `gofmt`, `go test`, `go test -race` all clean.
