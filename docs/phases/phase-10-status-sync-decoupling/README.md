# Phase 10 — Status Sync Decoupling From Progress

**Status:** Complete. A surgical two-edit fix to the Phase 9 engine, prompted by a live-server operator report. (Phase 10 is the **third live-server invalidation** in the project — see [decision-record.md §"Why this is the third live-server invalidation"](./decision-record.md#why-this-is-the-third-live-server-invalidation-and-what-that-pattern-means) for the project-level pattern note.)
**Design:** [design.md](./design.md) — **historical investigative record** (the design's own header; kept untouched per the project convention).
**Decision record (ADR):** [decision-record.md](./decision-record.md) — the as-built record.
**Implemented in:** [`internal/sync/engine.go`](../../../internal/sync/engine.go) (two surgical edits), [`internal/sync/engine_test.go`](../../../internal/sync/engine_test.go) (three regression-guard tests appended).
**Depends on:** [Phase 9 — Status Sync](../phase-09-status-sync/README.md).
**Unblocked by this phase:** nothing currently scoped.

## What this phase shipped

Two surgical edits to `internal/sync/engine.go`:

1. **`RunOnce`'s row-classification loop** is refactored so a row carrying a *decisive* `reading_status` and *null* `progress` is eligible for MatchCheck through the status channel on its own merits, not gated through the progress classifier first (which had been dropping null-progress decisive-status rows before MatchCheck ever saw them — the bug).
2. **The `pushStatuses` step's progress gate is dropped**, so the status push runs for status-eligible rows regardless of whether progress is also usable for that book.

Three regression-guard tests appended (no existing test modified): the bug regression, the "future-tense" decoupling guard (a row that then earns progress via a later Readest pull still pushes progress too), and the Decision II regression guard (mixed progress + status rows use a single unified MatchCheck batch).

## The bug this phase fixes

> Operator live-server report, 2026-08-02: *"I download a book from BookOrbit's OPDS which I have never read in BookOrbit. I set its status as 'Mark as finished' (finished) without even opening or reading any pages. [...] `BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config configs/bridge.yaml` does not write this book into `bridge-state.json`. [...] [But] if I go into Readest and read that book — even just turn a single page and let it sync — then I get that book in the bridge-state.json."*

Investigation surfaced a **two-layer bug** inherited from the Phase 6 progress classifier, both predating Phase 9. Full operator report and root-cause: [decision-record.md §"The bug this phase fixes"](./decision-record.md#the-bug-this-phase-fixes). Design treatment: [design.md §1](./design.md#1-the-bug).

## What shipped DIFFERENTLY from the design

- **Decision III implementation detail** (a minor implementation divergence from the design's prose): the Phase 9 engine's `MATCH_CHECK_BATCH = 500` cap was preserved, but a mismatched-status-eligibility check was added later in the row-classification loop rather than as a new pre-MatchCheck filtering stage. The behavior matches the approved Decision III; only the code shape differs. Recorded in [decision-record.md "What changed, file by file" → `internal/sync/engine.go`](./decision-record.md#internal-sync-enginego).

No Phase 9 mapping, Channel B body, 404 classification, opt-in flag, or warn-once discipline is revisited (Decision IV, [design.md §2](./design.md#decision-iv--phase-9-decision-preservation)).

## Live-validation items this phase opened

- A `since=0` re-pull on the operator's account (deleting the persisted state file first) to confirm the three finished-but-never-opened books from the operator's report now reach BookOrbit as `read` on the first poll. Tracked as a "resolved-on-confirmation" item pending that run, in [`../../live-validation-status.md`](../../live-validation-status.md) row "Status sync correctly handles null-progress decisive-status rows (Phase 10 fix)".

## Tests

[`internal/sync/engine_test.go`](../../../internal/sync/engine_test.go): three new tests appended — `TestRunOnceNullProgressFinishedBookStatusPushes`, `TestRunOnceNullProgressThenOpensBookPushesProgressToo`, `TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch`. No existing test modified. `go build`, `go vet`, `gofmt -l`, `go test ./...`, `go test -race ./...` all green.
