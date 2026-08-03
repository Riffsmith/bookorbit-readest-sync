# Live-Validation Status

This is the permanent tracking artifact for every live-server behavior flagged
across Phases 3–10 as needing verification against a real Readest account
and/or a real BookOrbit server, rather than something that can be confirmed
from the reference plugin source or a mocked-HTTP unit test alone.

It exists per [Phase 8](./phases/phase-08-tests-validation/README.md) Decision E: the project's testing convention
(`stubDoer`, hand-written fakes, no live integration-test harness — see
[Phase 7](./phases/phase-07-cli-packaging/design.md) §8.4 and
[Phase 8](./phases/phase-08-tests-validation/design.md) §4/§5) relies on
documented manual verification for anything that requires a real third-party
service. This file is where that knowledge lives permanently, instead of only
inside prose ADR addenda and [`live-test-reports.md`](./live-test-reports.md).

**Status legend**

- ✅ **Resolved** — confirmed by a specific live test, cited below.
- 🟡 **Accepted residual risk** — deliberately not chased; rationale given.
- ⬜ **Open** — still needs a live run before being considered checked off.

---

## Resolved (confirmed by [`live-test-reports.md`](./live-test-reports.md))

| Item | Opened in phase | Resolved by | Notes |
|---|---|---|---|
| Progress tuple semantics for EPUBs (`cur/total` vs. an opaque internal unit) | [Phase 4](./phases/phase-04-readest-client/README.md) §12 item 1 | Test 2 | Pushed ratio (0.08851) matched Readest's own displayed percentage (9%) and BookOrbit's dashboard (9%). `cur/total` is the correct semantic. |
| Whether BookOrbit accepts an empty `progress` string in `bulkProgress` | [Phase 5](./phases/phase-05-bookorbit-client/README.md) §16 item 1 | Test 2, Test 4 | Every live push sent `progress: ""`; both succeeded and displayed correctly. **Formerly listed in the README as "unverified"; now confirmed accepted** ([Phase 8](./phases/phase-08-tests-validation/decision-record.md) Decision D). |
| Absent-from-both-lists match-check handling, fresh-hash case | [Phase 6](./phases/phase-06-engine/README.md) ADR Addendum 2 (discovered live, fixed same phase) | Test 3 | The fix (SetUnmatched + DeleteMatch, watermark advances, `debug` not `warn`) was exercised for a *fresh* hash for the first time; Test 1 only proved the already-cooled-down case. |
| Absent-from-both-lists handling, already-cooled-down case | [Phase 6](./phases/phase-06-engine/README.md) ADR Addendum 2 | Test 1 | Idempotent re-run stayed near-silent; zero `WARN sync: hash absent` lines. |
| Match-check success path (`Source="file"` accepted by the live enum validator) | [Phase 6](./phases/phase-06-engine/README.md) ADR Addendum 1 (discovered live, fixed same phase) | Test 4 | Fresh `MatchRecord`, correct pushed percentage, correct BookOrbit dashboard display. |
| Deleted-row active state reset (Decision B) | [Phase 6](./phases/phase-06-engine/design.md) §5.3, §13 Decision B | Test 5 | `DeleteMatch`+`ClearUnmatched` fired with no BookOrbit contact; watermark still advanced; a second incidentally-deleted book was also handled correctly in the same pass. |
| Delete → re-add "brand new" property (no stale `LastPushedPct` bleed-through) | [Phase 6](./phases/phase-06-engine/design.md) §13 Decision B | Test 7 | A hash reappearing after deletion pushed the current Readest percentage fresh, not the pre-deletion value. Also surfaced that Readest can restore cloud-side reading position on re-import even after a local library deletion. |
| Daemon-mode entry, `mode:"daemon"` tag correctness, cancellation propagation via SIGINT | [Phase 7](./phases/phase-07-cli-packaging/design.md) §4.2 (claimed, not live-tested) | Test 6 | Confirmed against the real binary and a real OS signal; prompt exit, clean "shutdown requested; exiting" log line, state preserved. |
| Idempotent re-run / converged-state behavior | [Phase 6](./phases/phase-06-engine/design.md) §5.4 watermark-advance invariant | Test 1 | Immediate second `--once` run was near-instant and near-silent. |
| Positive match-check path end-to-end (book in both libraries) | [Phase 6](./phases/phase-06-engine/design.md) §6.2/§6.3 | Test 4 | Fresh match, correct push, correct dashboard display, no double-push on re-run. |
| Status sync correctly handles null-progress decisive-status rows (Phase 10 fix) | [Phase 10](./phases/phase-10-status-sync-decoupling/README.md) design; operator live report 2026-08-02 | Unit tests `TestRunOnceNullProgressFinishedBookStatusPushes`, `TestRunOnceNullProgressThenOpensBookPushesProgressToo`, `TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch` | Phase 10 decoupled status from progress so a book downloaded then marked "finished" without ever being opened (`progress: null, reading_status: "finished"`) reaches MatchCheck through the unified status-eligibility path and is status-pushed on the very first poll. Live confirmation run is the operator delete-state-file + `BRIDGE_SYNC_STATUS=true BRIDGE_LOG_LEVEL=debug ./bin/bridge --once` sequence in the [Phase 10 ADR](./phases/phase-10-status-sync-decoupling/decision-record.md) — tracked as a "resolved-on-confirmation" item pending that run. |

---

## Open (scheduled, per [Phase 8](./phases/phase-08-tests-validation/decision-record.md) Decision D)

| ID | Item | Opened in phase | Why still open | Priority |
|---|---|---|---|---|
| **L1** | SIGTERM as a distinct signal, independent of SIGINT | [Phase 7](./phases/phase-07-cli-packaging/decision-record.md) | Test 6's SIGTERM attempt was interrupted by a manual Ctrl+C before `kill -TERM` fired; only SIGINT was actually delivered (twice). `signal.NotifyContext` registers both identically in code and downstream cancellation handling is signal-agnostic, so the residual risk is narrow — but it was left incomplete against the original test plan. | High — cheap to redo correctly (send `kill -TERM $PID` from a separate terminal/script rather than a blocking `wait` in the sending shell). |
| **L4** | Supabase token refresh mid-run (50%-TTL proactive refresh or the 60-second guard) firing against the real Supabase project | [Phase 3](./phases/phase-03-auth/decision-record.md) | All seven live runs were short (Test 6's longest daemon session ran ~110s before shutdown); Supabase access tokens are typically ~1h TTL, so no run has crossed the 50% threshold live. This is the single largest unexercised live path relative to how central it is to `internal/readest.Auth` (Phase 3's most complex logic, already unit-tested exhaustively with a fake clock). | High — needs a >30 minute observation window (daemon mode or repeated `--once` calls with a shortened `poll_interval`), no code change required. |

## Accepted residual risk (per [Phase 8](./phases/phase-08-tests-validation/decision-record.md) Decision C — not blocking, documented rather than chased)

| ID | Item | Opened in phase | Why it's accepted rather than pursued |
|---|---|---|---|
| **L2** | Bulk-progress-unsupported fallback (`ErrUnsupportedEndpoint` → `UpdateProgress`) against a real older BookOrbit server | [Phase 5](./phases/phase-05-bookorbit-client/design.md) §16 item 3 | The operator's live server supports the bulk endpoint directly; the fallback path is unit-tested (`TestRunOnceBulkUnsupportedFallsBackAndStaysfallenBack`) and design-reasoned but has never fired live. Requires an actually older BookOrbit deployment, which may not be available. Real risk is scoped to operators on old BookOrbit versions. |
| **L3** | 429 (rate limiting) from either Readest or BookOrbit | [Phase 3](./phases/phase-03-auth/design.md) §15 item 2 | Never observed in any of the seven runs (single, well-spaced `--once` invocations). Unlikely to trigger under the documented default 15-minute poll interval for a single-instance deployment. Defensive code path only. |
| **L6** | Response size / pagination behavior of a very large `since=0` full pull | [Phase 4](./phases/phase-04-readest-client/design.md) | The current library (~100 rows) is far from any plausible size limit. Only relevant if the bridge is recommended to an operator with a 1000+ book library. |
| **L7** | `301` redirects, 401-vs-403 distinction, revoked-token response shape, `synced_at` presence on every row — properties of Readest's hosted, unversioned, third-party API | Multiple: [Phase 4](./phases/phase-04-readest-client/design.md) §12 items 3, 5, 7, 8 | The project doesn't control this service and has no supported way to safely provoke these conditions on demand. Treated as passive production monitoring (watch `warn`-level log volume) rather than an active test target, consistent with [`reverse-engineering-report.md`](./reverse-engineering-report.md) §5 risk #7. |
| **L8** | `BulkProgress` response actually containing a non-empty `unmatched` list | [Phase 5](./phases/phase-05-bookorbit-client/README.md) | Every live bulk-progress push so far succeeded with an implicitly empty `unmatched`. The engine's handling of this response field is unit-tested (mirrors `bookorbit_sweep.lua:stepProgressNext`) but would require a book to be removed from BookOrbit's library between match and push to trigger live — an edge condition, not a normal operating scenario. |

## Deferred, low priority (not in a phase's unit-test-gap or live-validation scope)

| Item | Opened in phase | Note |
|---|---|---|
| **G5** — `config.finalize()`'s anon-key base64-decode-failure branch | [Phase 8](./phases/phase-08-tests-validation/design.md) §2 | Explicitly deferred by Phase 8 Decision B. Guards a compile-time constant that cannot be corrupted at runtime without editing the source; a test would need an artificial seam purely to exercise dead-in-practice code. |
| **L5** — multi-chunk match-check/bulk-progress against real batch boundaries | [Phase 8](./phases/phase-08-tests-validation/design.md) §6 | The operator's real library stayed under 100 books in every live test, so every real request has been a single chunk. Chunking itself and the engine's per-chunk loop are both unit-tested with a scripted multi-chunk fake. Only matters for operators with >100 unmatched or >100 changed-percentage books in one poll window. |

---

## The only true open live-probe item (not part of Phases 3–10's scope — flagged across Phases 9/10)

The `on_hold` mapping for status-sync: no `on_hold` token exists anywhere in
the Readest plugin source, schema, tests, or design docs. Resolution is gated
on the operator's live Readest-web-UI probe described in detail in
[`investigations/status-sync.md`](./investigations/status-sync.md)
[Part I §5](./investigations/status-sync.md#5-interaction-with-phase-8) /
[Part II §4 Decision B](./investigations/status-sync.md#part-iii--decisions-answered-after-bookorbit-server-source-review-status-sync-design-investigationmd-6-verbatim) /
[Part III §6.8](./investigations/status-sync.md#68-decisions-not-made-here-flagged-as-live-probe-items-that-genuinely-remain).
Until that probe lands, the shipped status-sync (Phases 9 and 10) does not map
`on_hold` at all — any Readest `reading_status` that isn't `finished`,
`abandoned`, `unread`, or `nil` falls into the warn-once + skip path.

## How to update this file

When L1 or L4 (or any future live-validation item) is run:

1. Move its row from "Open" to "Resolved," citing whichever live test (new
   entry in `live-test-reports.md`, numbered sequentially) confirmed it.
2. If a run surfaces a genuine regression or a previously-unknown live-server
   behavior (as Tests 3/4 did for the [Phase 6 ADR](./phases/phase-06-engine/decision-record.md) Addenda), record it here and cross-
   reference the relevant ADR.
3. Residual-risk items move out of "Accepted residual risk" only if a live
   run actually becomes feasible (e.g., an old BookOrbit server becomes
   available for L2) — being listed here is not a permanent waiver, just a
   documented, current decision not to block on them.

This file does not require any new tooling or CI integration to maintain — it
is a plain-text companion to [`live-test-reports.md`](./live-test-reports.md), updated by hand after
each live-validation session, per [Phase 8](./phases/phase-08-tests-validation/decision-record.md) Decision E.
