# Project Documentation

**Reading order for newcomers:** start here, then follow the links in the order
below to get a complete mental model of the project in under 300 lines.

## 1. The one-sentence pitch

A standalone Go daemon that polls Readest's sync API on an interval, matches
books to your BookOrbit library by partial-MD5 hash, and pushes progress
percentages (+ optionally reading status) into BookOrbit. Readest is the sole
source of truth; BookOrbit is write-only. No KOReader runtime, no UI.

## 2. Read these first (genesis + constraints, ~80 lines combined)

1. [`problem-statement-prompt.md`](./problem-statement-prompt.md) — the original
   25-line objective ("eliminate the KOReader middleman; standalone bridge").
2. [`implementation-brief.md`](./implementation-brief.md) — the 56-line
   one-pager the operator signed that constrains every phase: "Go, polling
   daemon, Readest source of truth, BookOrbit write-only, no UI, no
   annotations, no bidirectional sync."

## 3. Then read the context folder (pre-implementation investigation)

[`context/`](./context/) — how we knew this was buildable and where the spec
lives:

- [`context/planning.md`](./context/planning.md) — the original feasibility
  investigation and architecture recommendation (Go over Rust/Node/Python, the
  proposed bridge flow, the open questions answered before Phase 0).
- [`context/reference-map.md`](./context/reference-map.md) — every reference
  plugin source file mapped to "In scope / Background pattern / Out of scope"
  for the bridge, plus the load-bearing constants to port (Supabase URL, anon
  key, refresh threshold, batch sizes, etc.).

## 4. Then skim the phases folder for **current state of the art**

[`phases/`](./phases/) — one folder per phase, each with a one-page hub
README plus its design doc and ADR. The hub README's "**What shipped
DIFFERENTLY from the design**" section is the most load-bearing thing in the
entire docs tree: it lists every live-server invalidation, addendum, and
deviation that rewrites a section of the design, with a deep link to the ADR
section that contains the corrected contract. **A reader of `design.md` alone
gets the pre-fix contract; the hub README is the only place guaranteed to flag
disagreements.**

Open them in phase order — each one names its dependency and what it unblocks:

[`phase-03-auth`](./phases/phase-03-auth/README.md) →
[`phase-04-readest-client`](./phases/phase-04-readest-client/README.md) →
[`phase-05-bookorbit-client`](./phases/phase-05-bookorbit-client/README.md) →
[`phase-06-engine`](./phases/phase-06-engine/README.md) →
[`phase-07-cli-packaging`](./phases/phase-07-cli-packaging/README.md) →
[`phase-08-tests-validation`](./phases/phase-08-tests-validation/README.md) →
[`phase-09-status-sync`](./phases/phase-09-status-sync/README.md) →
[`phase-10-status-sync-decoupling`](./phases/phase-10-status-sync-decoupling/README.md)

(Phase 0 skeleton and Phases 1–2 config/state-store had no design or ADR docs
written for them — see [`implementation-roadmap.md`](./implementation-roadmap.md)
for their scope and Milestone acceptance criteria.)

## 5. Then read the investigations folder for cross-cutting work

[`investigations/`](./investigations/) — feasibility studies that span phases
or end in "no implementation yet":

- [`investigations/status-sync.md`](./investigations/status-sync.md) — the
  consolidated 3-part record (reference-source feasibility + test strategy +
  Decisions A–F resolved against the live BookOrbit server source). This was
  the input Phases 9 and 10 were built from.
- [`investigations/security-hardening/`](./investigations/security-hardening/) —
  BookOrbit URL/loopback validation + cleartext-credential transport guard.
  Landed between phases as a parallel-track hardening pass.
- [`investigations/dashboard-triggered-sync.md`](./investigations/dashboard-triggered-sync.md) —
  feasibility only; not implemented yet. Investigates whether the bridge can
  be nudged to run one sync pass *before* BookOrbit's dashboard HTTP response
  is assembled so the synced progress is visible on first load (no manual
  refresh required).
- [`investigations/annotation-sync.md`](./investigations/annotation-sync.md) —
  feasibility only; not implemented yet. Extends the shipped bridge to cover
  annotations/highlights; rated "an order of magnitude harder than progress or
  status sync" due to position conversion (xpointer ↔ CFI ↔ pdf) and the
  annotation-exchange two-phase protocol.

## 6. Then the cross-cutting tracking + reference docs

- [`implementation-roadmap.md`](./implementation-roadmap.md) — the
  dependency graph (Mermaid) the project was actually built in, with milestone
  acceptance criteria. Phases 9 and 10 were inserted after the original
  roadmap was written; the phases folder's [`README.md`](./phases/README.md)
  re-renders the graph with all 10 phases.
- [`live-validation-status.md`](./live-validation-status.md) — the permanent
  tracking artifact for every live-validation item opened across Phases 3–10
  (resolved by which live test / open and scheduled / accepted residual risk /
  deferred), with reasoning preserved inline.
- [`live-test-reports.md`](./live-test-reports.md) — the operator's
  chat-style log of the 7 live tests run against real Readest + BookOrbit
  accounts. Companion to `live-validation-status.md`.
- [`reverse-engineering-report.md`](./reverse-engineering-report.md) — the
  authoritative "why the plugin is the spec" record for both plugin sources
  plus the BookOrbit NestJS server source. §1–§5 cover the plugin-side contract;
  §8/§9 are kept as authorial provenance stubs that point at the investigations
  folder where the same server-side findings live in detail.

## 7. House rules

[`internals.md`](./internals.md) — the one-page "how we document this project"
guide (design docs are historical; ADRs are as-built; one hub per phase; cross
references via hyperlink, not via prose provenance sentences).
