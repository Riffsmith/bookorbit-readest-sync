# Investigations — Cross-Cutting Feasibility Studies

These docs are **not** phase designs. They span phases, or end in "no
implementation yet," or treat a problem that doesn't fit the phase-numbering
unit at all. They live separately so a contributor skim-fileting by filename
can tell which is which.

| Folder / file | Status | Role |
|---|---|---|
| [`status-sync.md`](./status-sync.md) | **Implemented** (Phases 9 and 10) | Consolidated 3-part record: (I) reference-source feasibility from the Readest + BookOrbit plugin source, (II) test strategy + six open Decisions A–F, (III) the Decisions A–F answers against the live BookOrbit NestJS server source. The Phases 9 and 10 designs and their ADRs were built directly from Part III §6 of this file. |
| [`security-hardening/`](./security-hardening/) | **Implemented** (parallel track, not phase-numbered) | BookOrbit URL scheme + loopback validation + cleartext-credential transport guard. Lands the additive validation touch-points; no engine or wire-protocol behavior change. |
| [`dashboard-triggered-sync.md`](./dashboard-triggered-sync.md) | **Feasibility only, not implemented yet.** | Investigates whether the bridge can be triggered automatically when BookOrbit's dashboard is accessed, with the synced progress visible on first page load (no manual refresh required). The recommended Push-model variant is documented; implementation awaits operator approval. |
| [`annotation-sync.md`](./annotation-sync.md) | **Feasibility only, not implemented yet.** | Extends the shipped progress bridge to cover annotations/highlights. Rated "an order of magnitude harder than progress or status sync" due to position conversion (xpointer ↔ CFI ↔ pdf) and the BookOrbit annotation-exchange two-phase protocol. Recommendation: Phase 11+ when the primary features are confirmed stable. |

For the project's reading order from project genesis through current state of
the art, start at [`../README.md`](../README.md).
