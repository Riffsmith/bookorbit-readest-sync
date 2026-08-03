# Phase 9 — Status Sync (one-way, Readest → BookOrbit)

**Status:** Complete and verified. The first phase of the project that added a feature beyond the original progress-only MVP scope.
**Design:** [design.md](./design.md) — **authoritative as-built record** (the design's own header).
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`internal/readest/models.go`](../../../internal/readest/models.go), [`internal/bookorbit/{client,models}.go`](../../../internal/bookorbit/client.go), [`internal/sync/state/state.go`](../../../internal/sync/state/state.go), [`internal/sync/engine.go`](../../../internal/sync/engine.go), [`internal/config/`](../../../internal/config/types.go) (`Bridge.SyncStatus`), test files for each.
**Investigation provenance:** [`../../investigations/status-sync.md`](../../investigations/status-sync.md) Parts I–III (originally authored as two separate docs — see the consolidated file's header).
**Depends on:** [Phase 8 — Tests + Validation](../phase-08-tests-validation/README.md).
**Unblocked by this phase:** [Phase 10 — Status Sync Decoupling](../phase-10-status-sync-decoupling/README.md) — Phase 10 fixed a two-layer bug Phase 9 shipped with.

## What this phase shipped

One-way, Readest → BookOrbit reading-status sync, opt-in via `bridge.sync_status: true` (default `false`. Decisions A–F (settled in [`../../investigations/status-sync.md`](../../investigations/status-sync.md) Part III §6 against the live BookOrbit server source) plus Decisions G (`on_hold` excluded from v1 — G-1 chosen) and H (`bookorbit.ErrBookGone` sentinel, Mech-α) resolved at implementation time. The mapping table is just two push tokens (`finished → read`, `abandoned → abandoned`); `unread`/`nil`/`reading` are non-decided no-ops (the latter is the load-bearing safety precedent from `readingstatus.lua:5-9`). Channel B only (`PUT /koreader/plugin/catalog/books/{bookId}/read-status`, body `{"status": "<token>"}`, no device wrapper). The status step is **decoupled** from the progress watermark (Decision E — a status push failure never retreats the progress watermark, never touches `LastPushedPct`/`LastPushedAt`).

## What shipped DIFFERENTLY from the design

- **Phase 6 progress-write path had to be made field-preserving.** [decision-record.md "Deviations found in testing"](./decision-record.md) — `pushChunk`/`pushSingle` were constructing a fresh `MatchRecord` literal on every progress push, which would zero the newly-added `LastSeenStatus*` / `LastPushedStatus*` fields. Caught by `TestRunOnceStatusUnchangedSkips` failing on the first test run; fixed by keeping the existing `p.rec` and updating only the two progress fields. Not an architecture change; the existing progress-write path field-preserving now that `MatchRecord` carries more than progress.

Phase 9's ADR records no other deviations. (Phase 10 *then* found and fixed a two-layer bug in the shipped Phase 9 engine — see the next hub README.)

## Live-validation items this phase opened

- The `on_hold` probe (Readest web UI → `GET /sync?type=books&since=0` inspection of the `reading_status` field) remains the only true live-probe item blocking an `on_hold` mapping. See [Part I §5](../../investigations/status-sync.md#5-interaction-with-phase-8) / [Part III §6.8](../../investigations/status-sync.md#68-decisions-not-made-here-flagged-as-live-probe-items-that-genuinely-remain) of the status-sync investigation. Does not gate status sync for `finished`/`abandoned`.
- A live smoke run of `BRIDGE_SYNC_STATUS=true` against a real account. Tracked in [`../../live-validation-status.md`](../../live-validation-status.md).

## Tests

[`internal/sync/engine_test.go`](../../../internal/sync/engine_test.go) status matrix: gate-off, fresh `finished`/`abandoned` push, unchanged skip, non-decisive skip, `unread` records-seen-but-pushes-nothing, `unread→finished` transition, unmatched book skipped, `ErrBookGone` drops with progress watermark unaffected, retryable failure decoupled from progress, `ErrUnauthorized` abort preserves prior mutations + `Save()`, mixed multi-book batch, `Save()` exactly once with the status step present, unrecognized value warns once (verified via `slog.TextHandler` on an in-memory buffer). `TestMapReadingStatus` table test (all six cases). Plus `internal/bookorbit` and `internal/config` tests for the new method/field, and `internal/readest` tests for the two new `BookRow` fields.
