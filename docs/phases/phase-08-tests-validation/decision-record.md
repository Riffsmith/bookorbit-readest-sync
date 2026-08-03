# Phase 8 — Decision Record

**Status:** Implemented exactly per the approved design
(`docs/phase-8-design.md`). Scope was intentionally tight and test/
documentation-only, per the design's own §8 "Explicit non-goals" and the
task instructions: no production behavior changed, no architecture
revisited, no new testing framework introduced. `go build ./...`,
`go vet ./...`, `gofmt -l .`, `go test ./...`, and `go test -race ./...` all
pass, including every newly added test.

Phase 8 is complete. The two remaining items are live-server checks (L1,
L4), not code — see `docs/live-validation-status.md`.

---

## Decisions implemented

| # | Decision | Status |
|---|---|---|
| A | Add the four small unit tests (G1–G4): `internal/token/token_test.go`, a direct `LowerNormal` test, a direct `LevelName` test, and the two `EnvSupabaseAnonKey` env-branch cases | **Implemented** |
| B | Treat G5 (the anon-key base64-decode-failure branch in `config.finalize()`) as optional/deferred | **Deferred, as approved** — no code added |
| C | Accept L2, L3, L6, L7, L8 as documented residual risk rather than blocking "production ready" on them | **Documented** in `docs/live-validation-status.md` |
| D | Schedule L1 (SIGTERM) and L4 (live token refresh) as the two remaining live tests; correct the README's empty-`progress`-string item from "unverified" to "confirmed accepted" | **Documented** — README updated, both items tracked as open in `docs/live-validation-status.md` |
| E | Formalize a permanent live-validation tracking artifact | **Implemented** — `docs/live-validation-status.md` created |

No decision was reopened or reconsidered. §3 and §4 of the design (the
table-driven-utility-test confirmation and the mocked-HTTP-testing-strategy
confirmation) required no action — both were audits confirming existing
practice, not proposals.

---

## What changed

### Production code

**None.** No file under `internal/config`, `internal/logger`, `internal/util`,
`internal/token`, `internal/readest`, `internal/bookorbit`, `internal/sync`,
`internal/sync/state`, or `cmd/bridge` had its behavior changed. This matches
the design's own §8 non-goals and Decision A's framing ("additive test-only
work; nothing here revisits a Phase 3–7 decision"). No testability seam was
requested or added — Decision A's four tests all exercise already-exported,
already-testable functions (`token.Token`'s methods, `util.LowerNormal`,
`logger.LevelName`, and `config.Load`'s existing `EnvSupabaseAnonKey` handling
via the standard `t.Setenv` + `Load` pattern already used throughout
`config_test.go`).

### Test files added

- **`internal/token/token_test.go`** (new, Gap G1). Package-local table tests
  for `Token.ShouldRefresh`, `Token.ExpiresWithin`, and `Token.IsZero`,
  mirroring the boundary cases already covered indirectly via
  `internal/readest/models_test.go: TestTokenFreshnessRules` and throughout
  `internal/readest/auth_test.go`, but now owned by the package itself so a
  regression in the freshness rules is caught here first regardless of what
  `internal/readest` or any future `token.Token` consumer does.

- **`internal/util/util_test.go`** (modified, Gap G2). Added `TestLowerNormal`:
  a direct table test (mixed case, leading/trailing whitespace including tabs
  and newlines, an already-normalized value, an empty string, and an
  already-lowercase MD5-shaped string) covering `LowerNormal`'s own contract
  independent of `config.AuthKey`'s composition of it (previously exercised
  only via `internal/config/config_test.go: TestAuthKeyDerivation`'s
  userkey-normalization assertion).

- **`internal/logger/logger_test.go`** (modified, Gap G3). Added
  `TestLevelName`: a direct table test over every recognized level name
  (including case variants and the "warning" alias) plus the unknown-falls-
  back-to-"INFO" case, pinning the exported `LevelName` function's own
  contract independent of `New`'s level-gating behavior (which exercises
  `parseLevel` internally but never calls `LevelName`).

- **`internal/config/config_test.go`** (modified, Gap G4). Added
  `TestEnvSupabaseAnonKeyBase64JWTShapedDecodes` and
  `TestEnvSupabaseAnonKeyRawPassthroughWhenNotBase64`, exercising both
  branches of `applyEnv`'s previously-untested `EnvSupabaseAnonKey`
  conditional (`internal/config/env.go`): a base64-encoded value whose
  decoded form has the `"eyJ"` JWT-shaped prefix is decoded and stored; any
  other value (here, a string containing characters outside the standard
  base64 alphabet) is used verbatim. Both tests follow the existing
  `setRequiredEnv` + `Load(missing path)` + `errors.Is(err,
  ErrNoConfigFile)` pattern already used by `TestLoadMissingFileUsesDefaults`.

No existing test in any of these four files was modified or removed; all
additions are net-new test functions.

### Documentation added

- **`docs/live-validation-status.md`** (new, Decision E). The permanent
  tracking artifact: every live-validation item flagged across Phases 3–8's
  design docs, cross-referenced against the seven tests already run
  (`live-test-reports.md`), grouped into Resolved / Open (scheduled) /
  Accepted residual risk / Deferred, with the reasoning for each residual-
  risk acceptance preserved inline so a future contributor doesn't have to
  re-derive it. Includes an explicit "how to update this file" section for
  when L1/L4 are eventually run.

- **`docs/adr/phase-8-decision-record.md`** (this file, new).

### Documentation modified

- **`README.md`** (Decision D):
  - Status banner updated from "Phase 7 complete" to "Phase 8 complete,"
    summarizing the seven passed live tests and the two still-open live
    checks, pointing at `docs/live-validation-status.md`.
  - "Known live-server unknowns to watch for" renamed to "Known live-server
    behavior" and rewritten: the empty-`progress`-string item is moved from
    "unverified, watch for `ErrBadRequest`" to "confirmed accepted," citing
    Tests 2 and 4 as the live evidence (per the design's explicit instruction
    that "Test 2 already proves this"). The unsupported-endpoint status code,
    429 handling, and large-pull-size items are reframed as accepted residual
    risk with a pointer to the new tracking file rather than left as vague
    unknowns. The unchanged-`updated_at`-handling and no-redundant-push items
    are kept (no live evidence contradicts them) with the no-redundant-push
    claim now cited against Test 1/Test 2's live confirmation.
  - Roadmap section updated from "Phases 0–7 complete... Remaining: Phase 8"
    to "Phases 0–8 complete... two live checks remain," pointing at
    `docs/live-validation-status.md`.

No other documentation file was touched. `docs/phase-8-design.md` (the
design investigation itself) is left as-is per the project's established
convention (design docs are historical records; the ADR is where the
"as-implemented" account lives — the same pattern followed by every prior
phase's ADR relative to its design doc).

---

## Deviations from the approved design

**None.** The implementation matches `docs/phase-8-design.md` exactly:

- Decision A's four tests (G1–G4) were added with no scope creep — no
  additional gaps beyond G1–G4 were introduced, and G5 was left untouched
  per Decision B.
- No production file was modified, per Decision A's own framing and the
  task instruction that no testability seam was pre-approved for Phase 8.
- No new mocking/HTTP-fake library was introduced (Decision confirmed in
  design §4/§8) — Decision A's tests use only `t.Setenv` and the standard
  library, consistent with every other table-driven test in this codebase.
- No new integration-test harness or CI live-server gate was introduced
  (design §4/§5/§7-E/§8) — `docs/live-validation-status.md` is a plain-text,
  hand-maintained artifact with no tooling dependency, exactly as Decision E
  specified.
- The residual-risk items (L2, L3, L6, L7, L8) were documented, not chased,
  per Decision C — no fault-injecting proxy, synthetic huge library, or old
  BookOrbit deployment was manufactured to force them.

---

## Verification performed

Full reconstruction of the module (every file in the repository as of Phase
7's ADR, plus this phase's additions) was built and tested end-to-end in an
isolated environment:

```
$ go build ./...
(clean, no output)

$ go vet ./...
(clean, no output)

$ gofmt -l .
(clean, no files reported)

$ go test ./...
ok  	github.com/Riffsmith/bookorbit-readest-sync/cmd/bridge
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/bookorbit
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/config
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/logger
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/readest
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/sync
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/token
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/util
?   	github.com/Riffsmith/bookorbit-readest-sync/internal/util/httpclient	[no test files]

$ go test -race ./...
ok  	github.com/Riffsmith/bookorbit-readest-sync/cmd/bridge
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/bookorbit
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/config
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/logger
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/readest
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/sync
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/token
ok  	github.com/Riffsmith/bookorbit-readest-sync/internal/util
?   	github.com/Riffsmith/bookorbit-readest-sync/internal/util/httpclient	[no test files]
```

`internal/token` went from `[no test files]` (confirmed as the exact gap G1
described) to a passing suite. The four new tests were also run individually
with `-v` to confirm every subtest name and pass status:

- `TestShouldRefresh`, `TestExpiresWithin`, `TestIsZero` — all subtests pass
  (`internal/token`).
- `TestLowerNormal` — passes (`internal/util`).
- `TestLevelName` — passes (`internal/logger`).
- `TestEnvSupabaseAnonKeyBase64JWTShapedDecodes`,
  `TestEnvSupabaseAnonKeyRawPassthroughWhenNotBase64` — both pass
  (`internal/config`).

No existing test regressed. No production behavior changed beyond the
approved Phase 8 scope (there was none to change — Phase 8 is test/docs
only). Documentation (`README.md`, `docs/live-validation-status.md`) is
consistent with the implementation: every claim in the README's updated
"Known live-server behavior" section is backed by a specific test cited in
`live-test-reports.md` and cross-referenced in `docs/live-validation-status.md`.

**Toolchain note:** `go.mod` declares `go 1.26.5`. Verification was performed
in a sandboxed reconstruction of the repository using the `go1.22.2` toolchain
available in that environment; the `go` directive was temporarily lowered to
`go 1.22` for that local build/test pass only and restored to `go 1.26.5`
before this record was written. No language or standard-library feature used
by Phase 8's additions is version-sensitive (`t.Setenv`, `encoding/base64`,
table-driven tests, `errors.Is` — all long-stable stdlib surface), so this is
a toolchain-availability artifact of the verification environment, not a
compatibility concern for the real `go 1.26.5` toolchain the project targets.

---

## Remaining accepted residual risks (carried forward, exactly as approved)

Per Decision C, the following are documented residual risk, not blockers,
with full reasoning in `docs/live-validation-status.md`:

- **L2** — bulk→singular-PUT fallback (`ErrUnsupportedEndpoint` →
  `UpdateProgress`) against a real older BookOrbit server: unit-tested, never
  triggered live; requires infrastructure (an old BookOrbit version) the
  current operator doesn't have.
- **L3** — 429 rate-limiting from either service: unit-tested, never observed
  live; unlikely under the documented default poll cadence.
- **L6** — response size/pagination behavior of a very large `since=0` pull:
  only relevant for a 1000+ book library, far beyond the current operator's
  ~100-book library.
- **L7** — Readest hosted-API edge behaviors (redirects, 401-vs-403
  semantics, revoked-token response shape, `synced_at` presence guarantees):
  properties of a third-party service this project doesn't control and has
  no safe way to provoke on demand; treated as passive production monitoring.
- **L8** — `BulkProgress` response actually containing a non-empty
  `unmatched` list: unit-tested, never observed live; would require a book
  to be removed from BookOrbit's library between match and push.
- **G5** (deferred per Decision B, not a live-validation item) — the
  anon-key base64-decode-failure branch in `config.finalize()`: guards a
  compile-time constant that can't be corrupted at runtime without editing
  the source.

Two items remain **open and scheduled** (not accepted risk — see Decision D):
**L1** (SIGTERM as a distinct signal) and **L4** (live Supabase token refresh
over a >30 minute window). Both require only a live run, no code change, and
are tracked with next-step instructions in `docs/live-validation-status.md`.
