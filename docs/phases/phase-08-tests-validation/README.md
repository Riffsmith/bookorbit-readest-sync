# Phase 8 — Test Strategy & Production-Readiness Validation

**Status:** Complete. **No production code changed.** Phase 8 was test- and documentation-only by design (§8 explicit non-goals).
**Design:** [design.md](./design.md) — *(original filename `phase-8-desing.md` had a typo; renamed at reorganization time)*
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`internal/token/token_test.go`](../../../internal/token/token_test.go) (new), [`internal/util/util_test.go`](../../../internal/util/util_test.go) (`TestLowerNormal` added), [`internal/logger/logger_test.go`](../../../internal/logger/logger_test.go) (`TestLevelName` added), [`internal/config/config_test.go`](../../../internal/config/config_test.go) (two `EnvSupabaseAnonKey` cases added), [`docs/live-validation-status.md`](../../live-validation-status.md) (new, the permanent tracking artifact).
**Depends on:** [Phase 7 — CLI + Packaging](../phase-07-cli-packaging/README.md).
**Unblocked by this phase:** [Phase 9 — Status Sync](../phase-09-status-sync/README.md)

## What this phase shipped

- **Four small unit-test batteries** (Decision A, gaps G1–G4) for `internal/token` (was a `[no test files]` package), `internal/util.LowerNormal`, `internal/logger.LevelName`, and `config.Load`'s `EnvSupabaseAnonKey` base64/raw-passthrough branches — all only exercising already-exported functions, no new testability seam.
- **Decision E — `docs/live-validation-status.md`** as the project's permanent tracking artifact for every live-validation item opened across Phases 3–7. Cross-references each of the 7 live tests in [`../live-test-reports.md`](../../live-test-reports.md) and groups them by Resolved/Open/Accepted-residual-risk/Deferred with reasoning preserved inline.
- **README updates**: empty-`progress`-string item moved from "unverified" to "confirmed accepted" (Tests 2 & 4 are its live evidence); status banner → "Phase 8 complete."

## What shipped DIFFERENTLY from the design

None. The Phase 8 ADR is the only ADR in the project with **zero deviations and zero addenda** — Phase 8 was the audit/test phase, not a feature phase, and every Decision (A–E) was implemented as proposed.

## Live-validation items this phase opened

Two items are **open and scheduled** (not accepted residual risk): L1 (SIGTERM as a distinct signal) and L4 (live Supabase token refresh over a >30 min window). Both are tracked with next-step instructions in [`../../live-validation-status.md`](../../live-validation-status.md).

Five items are **accepted residual risk** (Decision C, carried forward): L2 (bulk→singular-PUT fallback against an older BookOrbit), L3 (429 rate-limiting), L6 (large `since=0` pull size/pagination), L7 (Readest hosted-API edge behaviors), L8 (`BulkProgress.Unmatched` non-empty response).

One item is **deferred** (Decision B): G5 (anon-key base64-decode-failure branch in `config.finalize()`) — guards a compile-time constant, cannot be corrupted at runtime without editing the source.

## Tests

See "Implemented in" above — the Phase 8 additions are themselves the tests. `go vet ./...`, `gofmt -l .`, `go test ./...`, and `go test -race ./...` all green; `internal/token` went from `[no test files]` to a passing suite.
