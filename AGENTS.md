# bookorbit-readest-sync

## Project

- Headless Go daemon/CLI bridging reading progress from Readest (Supabase-backed cloud EPUB reader, source of truth) into BookOrbit (self-hosted kosync-compatible library server, write-only).
- One-way sync only: `Readest -> Readest Sync API -> bridge -> BookOrbit`. No status/highlight/annotation sync, no bidirectional writes.
- Module path: `github.com/Riffsmith/bookorbit-readest-sync`. Go 1.26+, no CGO, single static binary.
- No web server, no database. Persistence is one JSON state file (`internal/sync/state`), written atomically with `0600` permissions.

**Local dev setup:**

1. `cp configs/bridge.example.yaml configs/bridge.yaml` - edit with real credentials, or export `BRIDGE_*` env vars instead (preferred for secrets)
2. `make build` - static binary to `bin/bridge`
3. `BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config configs/bridge.yaml` - single pass with verbose logging
4. `make test` / `make test -race` before every commit

## Development Process (non-negotiable)

This project is built phase-by-phase, design-first, approval-gated. Follow this even for small changes:

1. **Investigate** - read the relevant reference plugin source under `reference/` (KOReader Lua plugins) and the existing Go code before proposing anything. Reference plugins are executable specifications, never copied verbatim.
2. **Design doc first** - for anything beyond a trivial fix, write a short design note (behavior verification table, edge cases, testing strategy) before touching code. End it with an explicit **decisions requiring approval** list.
3. **Wait for sign-off** - do not implement until the person has approved the listed decisions. Do not silently choose an alternative and proceed.
4. **Implement, then verify for real** - run `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...`, `go test -race ./...` in a sandbox. Never claim a phase is done without having actually run these.
5. **Stop after the milestone.** Do not start the next phase unprompted.
6. Prior phase design docs and ADRs are authoritative history - do not revisit or "improve" an already-accepted decision unless a genuine architectural issue surfaces. If one does, flag it explicitly rather than quietly diverging.

Reference-vs-bridge-specific behavior must always be labeled: cite the exact Lua file/line a behavior is ported from, and separately call out any deliberate bridge-specific deviation with its rationale (e.g. `internal/readest/auth.go`'s `reauth` fallback, which the Lua plugin does not have).

## Architecture / Package Boundaries

Each package owns exactly one external system or concern. Nothing crosses a boundary except through the interface that boundary exists for.

- `internal/config` - env, yaml, defaults, validation. No I/O outside `Load`.
- `internal/util`, `internal/util/httpclient` - pure helpers (time conversion, percent calc, URL normalize, MD5, batching) and the `Doer` transport seam. Zero network access, zero third-party deps.
- `internal/token` - the shared, provider-agnostic `Token` type and its pure freshness rules only. Exists specifically to break the `readest <-> state` import cycle - do not add auth/HTTP/persistence logic here.
- `internal/logger` - `log/slog` setup only.
- `internal/readest` - Supabase auth + `GET /sync?type=books` client. Knows nothing about BookOrbit.
- `internal/bookorbit` - match-check / bulk-progress / update-progress client. Knows nothing about Readest. Static auth (`x-auth-user`/`x-auth-key`), no token lifecycle.
- `internal/sync/state` - the only persistence. Passive key-value surface (`Store` interface); it does not decide *when* to write, only *how*.
- `internal/sync` (engine) - the **only** package allowed to import both `readest` and `bookorbit`. Owns all orchestration: watermark math, batching, retry/backoff, match/unmatched cache coupling, bulk-to-singular-PUT fallback policy.
- `cmd/bridge` - CLI wiring only (flags, signal handling, dependency construction). No business logic.

**Never** import `bookorbit` from `readest` or vice versa. **Never** let a client (`readest.Client`, `bookorbit.Client`) read or write `state.Store` directly - only the engine touches state.

## Client Design Rules

- **Clients are faithful, the engine is selective.** `readest.Client.PullBooks` returns dummy/deleted rows unfiltered; filtering, watermark advance, and match-check decisions all live in the engine. Never move filtering logic into a client "for convenience."
- **Clients never retry transient failures.** They classify a failure into a sentinel error (`ErrSyncServer`, `ErrRateLimited`, `ErrNetwork`, ...) and return immediately. All backoff/retry policy lives in `internal/sync/engine.go`'s `withRetry` + the two `classify*Err` functions, driven by `config.Bridge.Retry*`.
- **Auth is the one exception.** `readest.Client` performs exactly one re-auth-and-retry after a downstream 401/403, because that recovery policy is already owned by `readest.Auth`. This is documented as a deliberate, scoped exception, not a precedent for adding more client-side retries.
- **Distinct sentinel errors per package/concern.** Auth failures and sync failures get separate sentinels (`readest.ErrNetwork` vs `readest.ErrSyncNetwork`) so `errors.Is` in the engine is unambiguous. Follow this pattern for any new client method; do not reuse a sentinel across unrelated failure domains.
- Every sentinel error wraps status code + a bounded body snippet via `%w` (`maxErrorBodyBytes = 512`). Never log or wrap a raw, unbounded response body.
- `context.Canceled` / `context.DeadlineExceeded` must always propagate untouched, never get remapped to a generic network error - this is required for graceful shutdown to work.
- Constructors take an injected `httpclient.Doer`, `*slog.Logger` (nil -> `slog.Default()`), and where relevant an injectable clock (`now func() time.Time`, defaulting to `time.Now`, settable directly on the struct in tests - not a constructor param).

## Engine Rules

- `RunOnce` computes a **ceiling watermark** from all non-dummy rows, but only advances to it if nothing failed; on any genuine batch failure it retreats to `min(failed rows' WatermarkMs) - 1`, floored at the pre-existing watermark. Never let a failed row silently fall out of the next pull window.
- `state.Store.Save()` is called exactly once per `RunOnce`, at the end, regardless of outcome. Do not add additional `Save()` calls mid-pass.
- A hash absent from **both** `resp.Matches` and `resp.Unmatched` is unmatched, not a failure (`SetUnmatched` + `DeleteMatch`, watermark still advances). This was a real live-server regression (Addendum 2) - do not reintroduce "treat absent as failure."
- Auth failures (`readest.ErrUnauthorized`, `bookorbit.ErrUnauthorized`) abort the rest of `RunOnce` immediately but never roll back state already committed by earlier batches in the same pass.
- The bulk-to-singular-PUT fallback flag (`bulkUnsupported`) is in-memory only, never persisted - rediscovering it once per process restart is intentional, not an oversight.
- Deleted rows (`row.IsDeleted()`) actively reset local state (`DeleteMatch` + `ClearUnmatched`), not just get skipped - this is a deliberate bridge-specific addition beyond the reference plugin's "just ignore."

## Testing Conventions

- **No mocking framework, ever.** The only HTTP-mocking pattern in this codebase is a hand-rolled `stubDoer` (scriptable `[]stubStep{status, body, err, delay}`, records every request) implementing `httpclient.Doer`. Reuse this exact pattern for any new client; do not introduce `httpmock`, `gock`, VCR cassettes, or similar.
- Fakes for interfaces (`Authenticator`, `API`, `SyncClient`) are plain hand-written structs. No generated mocks.
- Table-driven tests for every pure function (`util.Percent`, `util.ISOToMs`, `util.NormalizeBookOrbitURL`, watermark math, etc.).
- `-race` is mandatory for anything touching `Auth.AccessToken`, `state.Store`, or the engine - these have documented single-refresh/single-writer guarantees that only a race test can actually prove.
- One narrow, justified exception to "always use stubDoer": a real `httptest.Server` is acceptable for a single end-to-end wiring test in `cmd/bridge` that proves the full construction chain (config -> state -> clients -> engine -> error propagation) against real `net/http`/JSON - not a pattern to spread further.
- No live integration-test harness, no CI live-server gate. Live-server behavior is verified manually and recorded permanently in `docs/live-validation-status.md` (status: Resolved / Accepted residual risk / Open) plus dated entries in `live-test-reports.md`. Any new live-validation item must be added to `docs/live-validation-status.md`, not left only in a chat transcript.

## Configuration

- All config flows through `internal/config`: YAML file -> env var override -> built-in default, in that precedence, merged in `Load()` and validated before use.
- Every new config field needs: a struct field with a `yaml` tag, a default in `defaults.go`, an env var constant + wiring in `env.go`, a case in `load.go`'s `applyValues`, and a validation rule in `validate.go` if it has constraints. Unknown YAML keys are a hard error - do not silently ignore typos.
- Never read an env var directly from a package other than `internal/config`.

## Error Handling & Logging

- Every exported error is a package-level `var Err... = errors.New(...)` sentinel, matched with `errors.Is`, never a raw string comparison.
- `log/slog` only. Never log a credential, token, or full request/response body. Truncate error bodies to `maxErrorBodyBytes`.
- Match the existing `debug`/`warn` discipline: routine-but-notable events (e.g. a hash settling into the unmatched cooldown) are `debug`; failures the operator should notice are `warn`; only genuinely fatal startup conditions are `error`.
- Deliberate deviations from reference-plugin behavior must be logged loudly at the point they happen (see `internal/readest/auth.go`'s `reauth`), not buried silently.

## Git / Docs

- Any newly discovered live-server behavior that invalidates an earlier assumption gets an **Addendum** appended to the relevant ADR (see Phase 6 ADR Addendum 1/2) - do not quietly rewrite the original decision text.
- Never add a `Co-authored-by` trailer to any commit.
- No em dashes in code, comments, commit messages, or docs. Use a hyphen, colon, or rewrite the sentence.

**General**

- No god methods or classes. If it's hard to name, it's doing too much.
- Communicate through exported services or shared interfaces only.
- Don't over-engineer. Introduce patterns only when the complexity REALLY justifies it.
- Split code when it improves naming, testability, or ownership.

## Code Style

- Never add unnecessary comments. Only add a comment when the logic is genuinely non-obvious or when it explains a tricky decision that cannot be inferred from the code itself. Do not describe what the code does - only explain why if the reason is not self-evident.
- Always run ESLint before committing (`cd server && npx eslint .` and `cd client && npx eslint .` as applicable). Fix any errors before committing.
- Never use em dashes anywhere: UI text, strings, comments, PR descriptions, commit messages, or any other written output. Use a regular hyphen, colon, or rewrite the sentence.

- Comments explain *why*, not *what* - especially for bridge-specific deviations from the reference plugin, where the comment must say what the plugin does differently and why the bridge diverges. Do not add comments that restate the code.
- Match existing patterns exactly before introducing a new one: injectable clock via a settable struct field, sentinel error + `%w` wrapping, `stubDoer`-style test doubles, `withTimeout`/per-call `context.WithTimeout` honoring a `timeout <= 0` disables-deadline convention.
- No god functions. `Engine.RunOnce` is already at the edge of acceptable size because it's the one orchestration point - do not add unrelated concerns to it; extract a helper instead.
- Avoid new third-party dependencies. The whole project deliberately uses only the standard library plus nothing else beyond what's already in `go.mod`.

## Verification

Before calling any change complete, run and report the actual output of:

```sh
go build ./...
go vet ./...
gofmt -l .
go test ./...
go test -race ./...
```

If a check was skipped, say so explicitly and why. For anything touching config, also confirm `configs/bridge.example.yaml` still documents every field/env var accurately.
