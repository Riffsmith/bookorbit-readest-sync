# Context — Pre-Implementation Investigation

These are the docs produced before any production code was written. Together
they establish **why** the bridge is buildable, **what** the spec is, and
**where** the load-bearing reference evidence lives.

| Doc | Role |
|---|---|
| [`planning.md`](./planning.md) | "Readest → BookOrbit Standalone Sync Bridge: Feasibility Investigation." The original architecture-vs-language tradeoff (Go over Rust/Node/Python), the proposed bridge flow, the candidate API surface treated as a spec, and the open questions answered before Phase 0 began. Note: the reverse-engineering report §3 lists 12 corrections to this doc — where the planning doc and the plugin source disagree, the plugin wins, and the corrected shapes live in `reverse-engineering-report.md` and the per-phase ADRs that flagged live-server invalidations. |
| [`reference-map.md`](./reference-map.md) | Every reference plugin source file mapped to "In scope / Background pattern / Out of scope" for the bridge, plus the table of behavioral constants to port (Supabase URL, anon key, refresh threshold, batch sizes, body-size limits, etc.). The "What NOT to port" section is the load-bearing exclusion list every phase design cross-references. |

For the project's reading order from project genesis through current state of
the art, start at [`../README.md`](../README.md).
