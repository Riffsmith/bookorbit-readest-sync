# Phase 4 — Readest Sync Client

**Status:** Complete and verified (84.8% coverage on `internal/readest`).
**Design:** [design.md](./design.md)
**Decision record (ADR):** [decision-record.md](./decision-record.md) — **note: this ADR is unusually short and lacks the conventional section structure**; what it records is reproduced below.
**Implemented in:** [`internal/readest/client.go`](../../../internal/readest/client.go), [`internal/readest/models.go`](../../../internal/readest/models.go), [`internal/readest/client_test.go`](../../../internal/readest/client_test.go), [`internal/readest/models_test.go`](../../../internal/readest/models_test.go)
**Depends on:** [Phase 3 — Readest Auth](../phase-03-auth/README.md)
**Unblocked by this phase:** [Phase 6 — Sync Engine](../phase-06-engine/README.md)

## What this phase shipped

The Readest-side sync client: `PullBooks(ctx, since int64) ([]BookRow, error)` against `GET /sync?type=books&since=<ms>`, Bearer-token auth via the Phase 3 `Authenticator`, one 401/403 → force-refresh-and-retry attempt, and the full row/timestamp model (`BookRow` + `ProgressTuple` decode + `IsDummy`/`IsDeleted`/`WatermarkMs`/`Percentage`). The client is **faithful** — returns every row in the requested window without filtering — and leaves selection to the engine (Phase 6).

## What shipped DIFFERENTLY from the design

No substantive deviations recorded in the ADR. (The Phase 4 ADR is a 5-line file with no `# Phase 4 — Decision Record` header — preserved as-is per the project's "ADRs are append-only" convention, with the conventional header added at reorganization time. See [decision-record.md](./decision-record.md).)

## Live-validation items this phase opened

- L6 — pagination/response-size behavior on a 1000+ book `since=0` pull (current operator library is ~100 books; this is monitored, not actively tested).
- L7 — Readest hosted-API edge behaviors (redirects, 401-vs-403 distinction, revoked-token shape, `synced_at` presence guarantees) — passive production monitoring.

Both tracked in [`../../live-validation-status.md`](../../live-validation-status.md).

## Tests

[`internal/readest/client_test.go`](../../../internal/readest/client_test.go): 69 scripted-fake cases (success, 401/403 retry, malformed, dummy/deleted row passthrough, per-call timeout, `-race`). [`internal/readest/models_test.go`](../../../internal/readest/models_test.go): tuple-decoding and `WatermarkMs`/`Percentage`/`IsDummy`/`IsDeleted` predicates.
