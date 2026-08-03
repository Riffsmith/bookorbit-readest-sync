# How We Document This Project

A one-page guide to the convention so future Phase 11+ authors and contributors
don't have to re-derive it from reading five prior ADRs.

## Doc types

| Type | Mutability | Where it lives | Role |
|---|---|---|---|
| **Hub README** | Living — updated when an ADR addendum lands | `docs/phases/phase-NN-<topic>/README.md` | One-page hub per phase following the §2.2 template: status, depends on, what shipped, ⚠️ what shipped DIFFERENTLY from the design, live-validation items, tests. The load-bearing thing in the docs tree. |
| **Design doc** | Frozen at approval; never edited except to add a header banner pointing to the hub README when an ADR addendum supersedes a section | `docs/phases/phase-NN-<topic>/design.md` | The proposed design at approval time — historical. A reader of the design alone is reading the *pre-fix* contract when there's an addendum; the hub README is the breadcrumb. |
| **ADR (decision-record.md)** | Append-only — addenda only, never edit a recorded decision | `docs/phases/phase-NN-<topic>/decision-record.md` | The as-implemented account: what changed, deviations, addenda (incl. live-server invalidations), verification pass. Any current contributor work references this, not the design. |
| **Investigation** | Living until implementation lands, then frozen | `docs/investigations/*.md` | Cross-cutting feasibility studies. Either spawn a phase (e.g. status-sync investigation → Phases 9, 10), land as a parallel-track hardening pass (security-hardening), or stay open (dashboard-triggered-sync, annotation-sync). |
| **Top-level tracking** | Living, updated after live runs | `docs/live-validation-status.md`, `docs/live-test-reports.md` | The permanent trace of which live-validation items are resolved-by-which-test vs. open/accepted-residual-risk, plus the operator's chat-style log of the 7 live tests. |

## Conventions

1. **One folder per phase** under `docs/phases/`. The phase is the primary
   unit of shipped work, not the doc-type.
2. **Every phase's hub README follows the §2.2 template** — see
   [`phases/README.md`](./phases/README.md) for the structure.
3. **Cross-cutting work lives in `docs/investigations/`**, never in
   `docs/phases/`. If a feasibility study spans multiple phases or doesn't fit
   the phase-numbering unit, it goes there.
4. **Hyperlink, don't restate.** If a fact appears in 3 docs, it belongs once
   and is linked from the other two. The reverse-engineering report §8/§9 are
   5-line pointer stubs for exactly this reason — the full server-side
   analysis lives in the investigations folder.
5. **Reference source citations stay byte-accurate.** A `bookorbit_sweep.lua:328-332`
   citation never gets reworded; the hyperlink conversion (`bookorbit_sweep.lua:328-332`
   → `[bookorbit_sweep.lua:328-332](../reference/.../bookorbit_sweep.lua#L328)`)
   is purely additive.
6. **The design/ADR separation is load-bearing.** When a live-server run
   invalidates a design's contract, the design is *not* rewritten — the new
   contract lives in an ADR addendum, and the design keeps a banner at its
   top pointing to the hub README's "What shipped DIFFERENTLY" section. This
   is so future readers can always tell what was originally approved vs. what
   actually shipped, which is what every prior ADR adds in its preamble
   "(Implemented exactly per the approved design (`docs/phase-N-design.md`))"
   and what `phase-08-decision-record.md` makes explicit at line 120-124:
   *"design docs are historical records; the ADR is where the as-implemented
   account lives — the same pattern followed by every prior phase."*

## What the reorg (2026-08-02) changed

- Created the `context/`, `phases/`, `investigations/` folder tree.
- Moved every `docs/phase-N-design.md` to `docs/phases/phase-NN-<topic>/design.md`
  with renames per the §2.7 plan (`phase-8-desing.md` typo dropped, `-temp`
  suffix dropped, zero-padded phase numbers for sort order).
- Moved every `docs/adr/phase-N-decision-record.md` to its phase folder as
  `decision-record.md`, co-located with its design.
- Merged `future-status-sync.md` + `status-sync-design-investigation.md` →
  `investigations/status-sync.md` (3 parts, verbatim).
- Slimmed `reverse-engineering-report.md` §8 and §9 to 5-line pointer stubs
  (full content lives in the dashboard-triggered-sync / status-sync
  investigations; no facts removed).
- Updated `live-validation-status.md` to a table with link-back columns to the
  originating phase ADR.
- Converted load-bearing reference-source citations (≥3 hits across docs) into
  markdown links.
- Authored this file, `docs/README.md`, and per-folder `README.md` files as
  the entry-point and indexes.

The reorg did not change any production code, any investigation content, any
ADR body, or any design body — only where the files live, what the cross-
references between them look like, and which new hub files were added.
