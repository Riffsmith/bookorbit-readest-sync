# Phase 7 Design Investigation — CLI, Packaging, Deployment, Operational Lifecycle

**Status:** design only, per instructions — no implementation code below. This document is the Phase 7 counterpart to `docs/phase-4-design.md` / `docs/phase-5-design-temp.md` / `docs/phase-6-design.md`: it describes what should be built, verifies claims against the reference plugins where applicable, and flags every bridge-specific decision for explicit approval before implementation begins.

**Inputs reviewed:** `docs/problem-statement-prompt.md`, `docs/planning.md`, `docs/implementation-brief.md`, `docs/reference-map.md`, `docs/implementation-roadmap.md`, `docs/reverse-engineering-report.md`, `docs/phase-5-design-temp.md`, `docs/adr/phase-6-decision-record.md`, `docs/phase-6-design.md`, plus the shipped code: `cmd/bridge/main.go`, `cmd/bridge/device.go`, `Makefile`, `README.md`, `configs/bridge.example.yaml`, `internal/config/*`, `internal/sync/engine.go`, `internal/sync/state/*`, `internal/readest/auth.go`, `internal/bookorbit/client.go`, `.gitignore`.

Phase 6 is treated as complete and correct. Nothing here revisits Phase 6's retry/watermark/fallback decisions (`docs/adr/phase-6-decision-record.md`) — this phase is strictly the executable shell around the already-verified engine.

> **Phase 6 follow-up — RESOLVED 2026-07-30.** This document
> previously carried a BLOCKED banner (recorded 2026-07-30 after the
> first live run) for two Phase 6 issues the run surfaced: the
> `MatchCandidate.Source` enum value (Addendum 1) and the "hash absent
> from match-check response" handler (Addendum 2). Both have now been
> fixed in `internal/sync/engine.go` per the Resolution record at the
> end of `docs/adr/phase-6-decision-record.md`. In particular:
>
> - Match-check result handling now treats a hash absent from both
>   `resp.Matches` and `resp.Unmatched` as **unmatched** (settled into
>   the `UnmatchedCooldown` recheck gate, watermark advances normally),
>   matching the reference plugin's `bookorbit_sweep.lua:328-332` and
>   the verified live BookOrbit server's omission-based behavior. The
>   pre-fix warning-storm and watermark-retreat are gone. A new
>   regression test, `TestRunOnceAbsentFromMatchResponseIsUnmatchedNotFailure`,
>   guards the corrected contract.
> - `docs/phase-6-design.md` §6.3 bullet 3 has been rewritten to remove
>   the stale "defensive — should not happen" wording and describe the
>   verified behavior with a code citation.
>
> The two specific Phase 7 sections the blocker previously flagged are
> now unblocked on their stated premises: §7.3's claim that the daemon's
> state converges (and `Restart=on-failure` is essentially a safety net)
> is now accurate; §8.2 item 9's call for `cmd/bridge` tests now sits
> on top of an `engine_test.go` that does cover the absent-from-both
> path. The rest of this Phase 7 design document is unchanged.

---

## 0. What already exists (verified against shipped code)

Contrary to a from-scratch Phase 7, a substantial amount of the roadmap's Phase 7 scope is **already implemented**. This matters: the job here is mostly *hardening, filling gaps, and adding packaging/ops artifacts*, not building the CLI from zero.

| Roadmap item | Status | Evidence |
|---|---|---|
| `--config` flag | **Done** | `cmd/bridge/main.go`: `configPath = fs.String("config", "configs/bridge.yaml", ...)` |
| `--once` | **Done** | `once = fs.Bool("once", false, ...)`; `if *once { return engine.RunOnce(ctx) }` |
| `--daemon` | **Done, but a no-op alias** | `daemon = fs.Bool("daemon", false, ...)`; only consumed by `modeName()` for the startup log line — daemon behavior is already the `else` branch regardless of this flag's value (see §3.2) |
| Signal handling | **Done** | `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` |
| Graceful shutdown on `context.Canceled` | **Done** | `if errors.Is(runErr, context.Canceled) { log.Info(...); return nil }` |
| State load/save lifecycle | **Done** | `st.Load()` at startup; `defer func() { st.Save() }()` at exit; `Engine.RunOnce` also calls `Save()` once per pass (Phase 6, `finish()`) |
| Device ID resolution | **Done** | `cmd/bridge/device.go`: config override → persisted → freshly generated UUIDv4 |
| Config loading + env overrides + defaults | **Done** | `internal/config/{load,env,defaults,duration,yaml}.go` |
| Config validation | **Done** | `internal/config/validate.go: Config.Validate()` |
| `configs/bridge.example.yaml` | **Done**, comprehensive, documents every env var override | present in repo |
| Static Linux build via Makefile | **Done** | `Makefile`: `CGO_ENABLED=0`, `-trimpath`, `-ldflags '-s -w -X main.version=$(VERSION)'` |
| Version injection | **Done** | `var version = "0.1.0-dev"` in `main.go`, overridden via `-X main.version=$(VERSION)` |
| `--version` | **Done** | prints `version` and returns nil |
| Logger initialization | **Done** | `internal/logger.New(os.Stderr, cfg.Bridge.LogLevel, cfg.Bridge.LogFormat)` |
| Client/engine construction | **Done** | full wiring of `httpclient.Doer` → `readest.Auth`/`readest.Client` → `bookorbit.Client` → `sync.Engine` |

**Not yet present** (the actual Phase 7 gap):

1. `-h`/`--help` is mishandled (exits 1, prints an ugly `bridge: flag: help requested` line) — see §3.3.
2. No differentiated exit codes (everything that isn't success is exit 1).
3. No unit tests for `cmd/bridge` (`main.go`, `device.go` have zero test coverage — every other package in the module has tests).
4. No systemd unit file, despite `docs/planning.md`'s explicit deployment-target answer ("systemd other docker") and `configs/bridge.example.yaml`'s `state_file`/env-var design already anticipating it.
5. No release/cross-compile workflow beyond the single-target `make build`.
6. No documented startup/shutdown log message contract (what gets logged at `info` vs `warn` vs `error`, and when).
7. No explicit statement of the "first-run" flow — it exists functionally (via `readest.Auth.AccessToken`'s lazy sign-in, Phase 3) but is undocumented as a *CLI-observable behavior*, which matters for support/troubleshooting.

Phase 7's job is exactly items 1–7. Everything else is confirmed working and is **not** touched.

---

## 1. `cmd/bridge` startup flow

### 1.1 Current flow (verified, `main.go: run()`)

```
1. flag.NewFlagSet("bridge", flag.ContinueOnError); parse argv
2. --version → print version, return nil (exit 0)
3. config.Load(*configPath)
     - missing file → warning to stderr, proceed with defaults+env
     - malformed file / failed validation → fatal, return err
4. logger.New(stderr, cfg.Bridge.LogLevel, cfg.Bridge.LogFormat)
5. state.NewFileStore(cfg.Bridge.StateFile); st.Load()
     - missing file → empty state, no error (state/file.go:Load)
     - malformed file → fatal, return err
6. resolveDeviceID(cfg, st)              — device.go
7. httpclient.New(cfg.Bridge.HTTPTimeout)
8. readest.NewAuth(...)                  — shares st (state.Store) for token persistence
9. readest.NewClient(...)                — shares rdAuth
10. bookorbit.DeviceInfo{...}
11. bookorbit.NewClient(...)
12. sync.NewEngine(cfg, rdAuth, rdClient, boClient, st, log)
13. log.Info("bridge starting", version, mode, state_file, device_id, poll_interval)
14. signal.NotifyContext(background, SIGINT, SIGTERM) → ctx, stop
15. defer stop(); defer st.Save()
16. --once → engine.RunOnce(ctx); return
17. else → engine.Run(ctx) [loops until ctx.Done()]
18. errors.Is(runErr, context.Canceled) → log "shutdown requested; exiting"; return nil
19. else → return runErr
```

This is the correct shape and needs **no structural change**. Every dependency is constructed exactly once, in dependency order, with no network I/O before step 16/17 (confirmed: none of `NewAuth`/`NewClient`/`NewEngine` perform I/O in their constructors — verified against `internal/readest/auth.go`, `internal/readest/client.go`, `internal/bookorbit/client.go`, all of which only assign fields).

### 1.2 What Phase 7 adds to this flow

Nothing structural. Three small, additive fixes:

- **Help handling** (§3.3): a new early branch for `errors.Is(parseErr, flag.ErrHelp)`.
- **Exit code mapping** (§3.4): `run()`'s return value needs a way to signal "usage error" vs "runtime error" to `main()`, since today every non-nil error maps to the same `os.Exit(1)`.
- **Startup/shutdown log message contract** (§7.2): formalizing what's already logged, adding one or two lines (see below), not changing the wiring.

### 1.3 Dependency construction order — why it's already correct

The order in `main.go` is: config → logger → state → device ID → HTTP doer → Readest auth → Readest client → BookOrbit client → engine. This matches the module's own dependency graph in `docs/implementation-roadmap.md`'s mermaid diagram (`CFG --> STATE`, `RAUTH --> RCLI`, `BCLI` independent, `RCLI/BCLI/STATE --> ENG`). No reordering is needed or proposed.

One thing worth calling out explicitly (not a change, a confirmation): `httpDoer` is a **single shared `httpclient.Doer`** passed to both `readest.NewClient` and `bookorbit.NewClient`. This is intentional and correct — both clients apply their own per-call `context.WithTimeout` on top of it (Phase 4 §9, Phase 5 §13), so a single underlying `*http.Client` with `cfg.Bridge.HTTPTimeout` as its own timeout is a reasonable shared floor, not a conflict. No change proposed.

---

## 2. First-run experience

### 2.1 Verified: there is no separate "login" flow, by design

`config.Validate()` (`internal/config/validate.go`) unconditionally requires `readest.email` and `readest.password` (or `BRIDGE_READEST_EMAIL`/`BRIDGE_READEST_PASSWORD`) regardless of whether a token is already persisted in the state file. This means:

- **First run** (no `bridge-state.json`, or one with a zero-value `Token`): `readest.Auth.AccessToken` (Phase 3, `internal/readest/auth.go`) sees `current.IsZero() == true` and calls `SignIn` — a single Supabase password-grant call — the very first time `Engine.RunOnce` calls `PullBooks`. This is not a CLI-level concept at all; it is fully inside `readest.Client.PullBooks → Auth.AccessToken`, already built and unit-tested (Phase 3 §13, Phase 4 §5).
- **Subsequent runs**: the persisted `Token` is reused; `AccessToken` refreshes proactively at the 50%-TTL threshold (Phase 3, confirmed).
- **Refresh-token expiry**: `Auth.reauth` (Phase 3, an intentional deviation from the plugin, already approved) falls back to a fresh `SignIn` from the same stored `email`/`password` — so even a fully-expired local state self-heals without any user action, as long as the config still has valid credentials.

**Design conclusion: no `bridge login`/`bridge setup` subcommand is needed.** The roadmap's Phase 7 description ("Initial setup / login helper if tokens missing and password provided") is **already satisfied** by the existing `AccessToken` lazy-sign-in path — adding a separate CLI verb would duplicate logic that Phase 3 already owns correctly, and would violate the "avoid future architectural churn" instruction. This is flagged in §10 as a decision to confirm (skip building a login subcommand), since it's a direct, deliberate divergence from the roadmap's phrasing.

### 2.2 What IS a legitimate first-run UX gap

The only real gap: on a *first* run with bad credentials, the failure happens silently inside the first `RunOnce`/`Run` iteration and, in **daemon mode**, is only logged at `warn` — the process keeps running and retrying forever (Phase 6, `Engine.Run`: "logs it, never propagates"). For `--once` mode the error properly propagates to `main()`'s exit code. This asymmetry is **already correct and approved behavior** (Phase 6 Decision, "a daemon keeps trying... a supervisor decides whether persistent failure warrants a restart") — but it means an operator running `bridge --daemon` for the first time with a typo'd password will see nothing but a `warn` log line every `poll_interval`, not a startup failure.

**Design recommendation (bridge-specific, not from any reference plugin — there is no live-UI equivalent to verify against):** add one **startup credential probe**, run once before entering `Run`'s loop (and before `RunOnce` in `--once` mode too, though it's redundant there since `RunOnce` will surface the same failure immediately). Concretely: call `bo.Auth(ctx)` (BookOrbit's existing `GET /koreader/users/auth` health check, already implemented in `bookorbit.Client.Auth`, Phase 5 §5) once at startup, log its result at `info` (success) or `warn` (failure, with guidance to check `bookorbit.username`/`password`/`server_url`), but **do not abort the process on failure** — this keeps the daemon's "keep trying" philosophy from Phase 6 intact while giving an operator immediate, visible feedback on the most common first-run mistake (wrong BookOrbit credentials) without adding a new failure mode.

Readest has no equivalent standalone "ping" endpoint in the client surface (`readest.SyncClient` only exposes `PullBooks`); a Readest credential probe would require either a throwaway `PullBooks(ctx, 0)` call (wasteful — it would also perform a full library pull on every process start) or a new method on `readest.Auth`/`readest.Client` that doesn't exist today. **Recommendation: do not add a Readest-side probe.** The first `RunOnce` already validates Readest credentials within one poll interval (or immediately in `--once` mode), which is an acceptable startup-feedback latency, and inventing a new client method purely for a startup check adds surface area for a single log line's benefit.

This probe is the one new piece of *behavior* proposed in this whole document; everything else is CLI/packaging scaffolding. It is called out explicitly in §10.

---

## 3. CLI behavior

### 3.1 `--config`

Unchanged. Default `"configs/bridge.yaml"` (gitignored — `configs/bridge.yaml` is in `.gitignore` alongside `configs/*.local.yaml`, confirming the example file is meant to be copied, never committed). A missing file at the default path is tolerated (`config.ErrNoConfigFile`, non-fatal warning) — this is correct for a fresh checkout driven entirely by environment variables (e.g., a Docker container with `BRIDGE_*` env vars and no mounted config file at all).

### 3.2 `--once` vs daemon/default vs `--daemon`

Verified current behavior: `--once` runs one pass and returns; everything else (including bare `bridge` with no flags, and `bridge --daemon`) runs `engine.Run(ctx)`, which loops until cancellation. `--daemon` today changes **only** the log line printed at startup (`modeName(*once, *daemon)`); it has no effect on control flow, because daemon *is* the default.

**Design decision:** keep `--daemon` as a **documentation flag** — an explicit, self-describing way for a systemd unit or a human to say "I mean to run continuously," even though it's a no-op relative to the default. This costs nothing, matches the existing code exactly as shipped, and gives the systemd unit (§6.1) a self-documenting `ExecStart` line (`bridge --daemon --config /etc/bridge/bridge.yaml`) instead of a bare invocation that looks like it might be missing a flag. No code change is needed here beyond what already exists — this is a confirmation, not a proposal.

### 3.3 `-h` / `--help` — **gap, needs a fix**

**Current (verified) behavior is wrong.** `flag.NewFlagSet("bridge", flag.ContinueOnError)` means Go's `flag` package, on seeing `-h`/`--help`, prints its own usage text (via the flag set's default usage function) to `os.Stderr`, **and then** `fs.Parse` returns `flag.ErrHelp` as a non-nil error. Today's `run()`:

```go
if err := fs.Parse(argv); err != nil {
    return err
}
```

propagates `flag.ErrHelp` straight up to `main()`, which prints `bridge: flag: help requested` and calls `os.Exit(1)`. That is a double-print (flag's own usage, then a spurious error line) and a wrong exit code (`--help` should never exit non-zero by Unix convention).

**Proposed fix (minimal, additive):** in `run()`, immediately after `fs.Parse`, special-case:

```go
if errors.Is(err, flag.ErrHelp) {
    return nil   // usage already printed by the flag package; exit 0
}
```

No other change. This does not touch `fs.Usage` or write a custom help renderer — the flag package's auto-generated usage (which lists each flag's name, type, and its `Usage` string as already supplied in `fs.String(...)`/`fs.Bool(...)` calls) is sufficient and idiomatic; writing a custom banner would be complexity with no functional benefit.

### 3.4 Exit codes

**Current (verified) behavior:** every error from `run()` (config invalid, state file corrupt, CLI parse error, `RunOnce` failure in `--once` mode) results in `os.Exit(1)` via `main()`'s single `if err := run(...); err != nil { ...; os.Exit(1) }`. Graceful shutdown (`context.Canceled` in daemon mode) and `--version`/`--help` (after the §3.3 fix) return `nil` → exit 0.

**Proposed refinement (bridge-specific; no reference plugin has a concept of "exit code" since KOReader plugins run inside a long-lived UI process):**

| Exit code | Condition | Rationale |
|---|---|---|
| `0` | success; graceful shutdown via signal; `--version`; `--help` | standard |
| `1` | runtime/operational failure: config validation failed, state file unreadable/corrupt, `RunOnce` failed in `--once` mode, `engine.Run` returned a non-`context.Canceled` error | matches current behavior exactly — **no change** |
| `2` | CLI usage error: unknown flag, malformed flag value (anything `fs.Parse` rejects that is **not** `flag.ErrHelp`) | new, matches the common Unix convention (e.g. `grep`, `bash`, and Go's own `flag.ExitOnError` mode, which uses `os.Exit(2)` for parse errors) |

This requires `run()` to distinguish "usage error" from "everything else" when returning to `main()`. The minimal mechanism, avoiding a bespoke error-code type: wrap CLI-usage errors in a small sentinel check at the `fs.Parse` call site only (the only place usage errors originate), e.g. `main()` checks `errors.Is(err, flag.ErrHelp)` (handled, exit 0) vs "the error came from `fs.Parse`" — since `fs.Parse` is the *only* caller-visible source of a usage error in the current `run()` body, this can be done without a new error type by having `run()` return the raw `fs.Parse` error immediately (as it does today) and having **`main()`** distinguish `errors.Is(err, flag.ErrHelp)` (exit 0) from "parse failed for another reason" — but that still can't distinguish a parse failure from a later runtime failure, since both are plain `error` values returned from the same `run()` function.

**Resolution:** this is exactly the kind of complexity the instructions ask to minimize. Given the CLI surface is four flags, all boolean/string, with no subcommands, **parse errors are rare and already loud** (flag package prints the exact bad argument to stderr before returning). The two-tier scheme (0 / 1) already shipped is adequate; a three-tier scheme is a nice-to-have, not a requirement. **Recommendation: adopt the 0/1/2 scheme only if trivial to implement without restructuring `run()`'s signature; otherwise keep 0/1 and document that CLI usage errors and runtime errors share exit code 1.** This is flagged in §10 as a decision — I lean toward **keeping 0/1** (do the `flag.ErrHelp` fix from §3.3, skip the exit-code-2 refinement) to avoid adding a `run() (error, int)` or sentinel-wrapping mechanism for a benefit that mostly matters to shell-scripting purists, not to this project's actual operators (systemd, Docker — neither cares about the difference between exit 1 and exit 2).

---

## 4. Graceful shutdown

### 4.1 Signal handling — verified, already correct

`signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` is the standard library's idiomatic pattern (Go 1.16+): it returns a `context.Context` that is cancelled the moment SIGINT or SIGTERM arrives, and a `stop()` function to unregister the signal handler (called via `defer stop()`). This is correct and requires no change.

### 4.2 Draining in-flight work

There is no separate goroutine pool to drain: `Engine.Run` executes `RunOnce` **synchronously** in the same goroutine as `main()`'s call, confirmed in Phase 6 §10 ("No synchronization is introduced... entirely single-goroutine and sequential"). This means:

- **Signal arrives while `RunOnce` is mid-flight** (e.g., blocked on an HTTP call to Readest or BookOrbit): the `ctx` passed all the way down to `http.NewRequestWithContext` in both `readest.Client` and `bookorbit.Client` is the **same** cancellable context from `signal.NotifyContext`. Cancellation therefore propagates through Go's `net/http` transport immediately — the in-flight request is aborted, `c.http.Do(req)` returns wrapping `context.Canceled`, both clients' `do()`/`pull()` helpers explicitly let `context.Canceled`/`context.DeadlineExceeded` propagate untouched (verified: `readest/client.go: if errors.Is(err, context.Canceled) ... return nil, false, err`; `bookorbit/client.go: do()` has the identical check). `Engine.withRetry`'s classifiers (`classifyReadestErr`/`classifyBookOrbitErr`) map `context.Canceled`/`context.DeadlineExceeded` to `outcomeFatal`, so no retry is attempted — the aborted call surfaces immediately as `RunOnce`'s return error.
- **Signal arrives while `Run` is sleeping between polls**: `e.sleep(ctx, e.cfg.Bridge.PollInterval)` (the injectable `defaultSleep`) selects on `ctx.Done()` vs a timer and returns `ctx.Err()` immediately on cancellation; `Run` returns that error (`context.Canceled`) to `main()`.
- **`state.Store.Save()` still runs**: `RunOnce`'s `finish()` calls `e.st.Save()` unconditionally at the end of every pass (Phase 6 Decision C), including a pass that was aborted mid-flight by cancellation — so whatever state mutations (matches, watermark) committed before the cancellation-triggered abort are still persisted for that pass. `main()`'s own `defer st.Save()` is a redundant final safety net on top of this (harmless, already documented in Phase 6 §5.5 as "a harmless, redundant final safety net").

**Conclusion: no new "draining" logic is needed.** There is no background work outside the single synchronous call chain, and cancellation already propagates cleanly through every layer (verified by Phase 4's and Phase 5's own test suites: `TestPullBooksContextCancelled`, `TestMatchCheckContextCancelled`, `TestRunRespectsCancellationDuringPollSleep`). This is a case where three completed phases already did the hard work; Phase 7 only needs to document it (see §7.2 for the log lines that make this visible to an operator).

### 4.3 Resource cleanup

The only resources the process holds are: the state file handle (opened/closed per `Load`/`Save` call, never held open — `FileStore.Save` writes to a temp file and renames, confirmed in `internal/sync/state/file.go`) and the HTTP transport's connection pool (owned by `*http.Client`, cleaned up by process exit; no explicit `Close()` exists or is needed on `httpclient.Doer`). **No new cleanup code is required.**

### 4.4 Shutdown timeout

**Question worth deciding, not yet addressed:** should there be a maximum wait time between SIGTERM and process exit, in case a single HTTP call hangs despite context cancellation (e.g., a buggy transport that ignores context)? Go's `net/http` with a `context.Context`-aware request reliably aborts on cancellation at the transport level (this is a core `net/http` guarantee, not something the bridge needs to reimplement). **Recommendation: no additional shutdown-timeout/force-kill logic in the application itself** — this is exactly what systemd's `TimeoutStopSec` is for (§6.1), and duplicating it in Go would be redundant with the process supervisor's own responsibility.

---

## 5. Configuration

### 5.1 `configs/bridge.example.yaml` — already comprehensive

The shipped example file already documents: every section (`readest`, `bookorbit`, `bridge`), every field with an inline comment, every environment-variable override name in a header block, and sensible commented-out defaults matching `internal/config/defaults.go` exactly (`poll_interval: "15m"`, `match_batch_size: 500`, `progress_batch_size: 100`, `max_body_bytes: 921600` = 900×1024, `unmatched_cooldown: "24h"`, `http_timeout: "30s"`, `retry_max_attempts: 5`, `retry_initial_backoff: "500ms"`, `retry_max_backoff: "30s"`). **No changes proposed to this file's content.**

### 5.2 Validation rules — already comprehensive

`internal/config/validate.go: Config.Validate()` already aggregates every problem (not fail-fast on the first), covering: Readest email/password/supabase_url/anon_key/sync_base_url required and non-empty; BookOrbit server_url/username required, password-or-userkey required, userkey format-checked via `util.IsMD5Hex`; Bridge poll_interval/match_batch_size/progress_batch_size/max_body_bytes/http_timeout must be positive, retry_max_attempts must be non-negative. This is already **more thorough** than what a typical Phase 7 "validation" task would add. **No changes proposed.**

One confirmation worth stating explicitly: `Config.Validate()` runs **inside** `config.Load()`, so an invalid config (missing credentials, bad userkey format, non-positive interval) is caught and returned as a `*ValidationError` **before** `main()`'s `run()` reaches the logger/state/client construction steps — meaning a bad config never gets as far as opening the state file or attempting a network call. This is the correct fail-fast ordering and requires no change.

---

## 6. Deployment artifacts

### 6.1 systemd service (new)

**Design (not code — described here, implemented as a plain-text unit file with no templating/build-system involvement beyond copying it into the release artifact or documenting it in `README.md`):**

- `Type=simple` — the bridge does not fork, and Go's `net/http`/`slog` stack has no `sd_notify` integration; adding one would be a new dependency (`github.com/coreos/go-systemd` or hand-rolled `sd_notify` socket writes) for a feature (`Type=notify`) that provides no behavioral benefit here (the process is either running or it isn't; there is no "ready but not yet accepting requests" phase to signal). **Recommendation: `Type=simple`, no new dependency.**
- `ExecStart=/usr/local/bin/bridge --daemon --config /etc/bridge/bridge.yaml` — using the explicit `--daemon` flag (§3.2) purely for operator legibility in `systemctl status`/`journalctl` output.
- `Restart=on-failure`, `RestartSec=10` — since daemon-mode `Run()` deliberately never exits on transient sync failures (Phase 6, confirmed), a non-zero exit only happens on a genuinely fatal condition (state file corruption, `context.Canceled`-unrelated `Run` error — which today's `Run` signature cannot actually produce other than via `sleep`'s own context error, meaning in practice **the daemon only exits non-zero if the state file becomes unreadable/corrupt after startup**, an extremely rare event). `Restart=on-failure` is still correct as a safety net for that rare case and for any future error path, at negligible cost.
- `WorkingDirectory=/var/lib/bridge` (or wherever `state_file`/`--config` point), so relative paths in the config resolve predictably.
- `EnvironmentFile=/etc/bridge/bridge.env` (optional, `-`-prefixed so a missing file doesn't block startup) — this is the natural place for the `BRIDGE_READEST_PASSWORD`/`BRIDGE_BOOKORBIT_PASSWORD` secrets so they never need to live in `bridge.yaml` on disk, consistent with `configs/bridge.example.yaml`'s own header ("Prefer env vars for these").
- `User=`/`Group=` a dedicated unprivileged service account (not root) — standard practice; the state file already gets `0600` permissions (`internal/sync/state/file.go: filePerm = 0o600`), so a non-root user owning that file and directory is sufficient isolation.
- Hardening directives worth including, all zero-cost given the bridge's actual I/O footprint (one state file, outbound HTTPS only, no listening sockets): `NoNewPrivileges=true`, `ProtectSystem=strict` with `ReadWritePaths=` scoped to the state file's directory, `PrivateTmp=true`, `ProtectHome=true`. These cost nothing to configure and match the "state is the only persistence" design already stated in `README.md`.
- `TimeoutStopSec=30` (or similar) — systemd sends SIGTERM, waits this long, then SIGKILL. Given §4.4's conclusion (no reason to expect a hang beyond a single in-flight HTTP call, itself bounded by `cfg.Bridge.HTTPTimeout`, default 30s), a `TimeoutStopSec` slightly above `HTTPTimeout` (e.g. 35–40s) is a sensible default so a shutdown mid-request has time to actually finish rather than getting SIGKILLed while `state.Save()` is running.

**Location:** propose `systemd/bridge.service` at the repo root (mirroring the roadmap's own `Phase 7` bullet: "Add `systemd/` example unit file"), referenced from `README.md`'s deployment section. This is a **plain static text artifact**, not a template requiring a build step — the same file works for every install, with `/etc/bridge/bridge.yaml` and `/etc/bridge/bridge.env` as the only install-time customization points.

### 6.2 Makefile / build system — already adequate, minor addition proposed

The shipped `Makefile` already has `build`, `run`, `test`, `vet`, `fmt`, `lint`, `tidy`, `clean`, `help`, with static-binary flags (`CGO_ENABLED=0`, `-trimpath`, `-ldflags '-s -w -X main.version=$(VERSION)'`) and a `VERSION` derived from `git describe --tags --always --dirty`. This is correct and sufficient for local development and for a single-platform release build.

**Proposed addition:** a `release` target that cross-compiles for the handful of platforms a self-hosted BookOrbit-adjacent deployment realistically needs — `linux/amd64`, `linux/arm64` (common for NAS/Raspberry Pi deployments, explicitly named as a candidate in `docs/reverse-engineering-report.md` §6 recommendation 5 and `docs/planning.md`'s architecture comparison table), and optionally `darwin/arm64`/`darwin/amd64` for local development on Apple Silicon/Intel Macs. Each target is a simple `GOOS=... GOARCH=... go build` invocation with the same `LDFLAGS`, writing to `bin/bridge-$(GOOS)-$(GOARCH)`. This needs no new tooling (no `goreleaser`, no CI-only dependency) — it is a natural `Makefile` loop over a small target list, consistent with the existing `Makefile`'s style and the project's stated aversion to unnecessary dependencies (`docs/implementation-brief.md`: "Never copy plugin code... idiomatic Go", `docs/planning.md`: single static binary is the whole point of choosing Go).

### 6.3 Release workflow

**Design, not implementation:** a CI workflow (e.g. GitHub Actions, since the repository is hosted on GitHub) that, on a tag push (`v*`), runs `make release`, then attaches each `bin/bridge-$(GOOS)-$(GOARCH)` artifact plus the `systemd/bridge.service` file and `configs/bridge.example.yaml` to a GitHub Release. This is described here as a design requirement, not authored as YAML, per the "no implementation code" instruction. Two points worth deciding explicitly before this is built:

- Whether release builds should also run the full test suite (`go test ./... -race`) as a release gate — **recommended: yes**, since it's already fast (the entire suite is unit tests with fakes/stubs, no network, confirmed across every `_test.go` file reviewed) and costs nothing in CI time.
- Whether to also publish a container image — **not recommended for v1.** The problem statement and `docs/planning.md`'s open question ("systemd other docker") indicate the operator wants *either*; a static binary + systemd unit already fully serves the systemd case, and a minimal `FROM scratch` image built from the same static binary is a trivial follow-on (no code changes, just a `Dockerfile` copying the release binary) that can be added later without any architectural impact. Flagged in §10 as explicitly deferred, not rejected.

---

## 7. Operational concerns

### 7.1 Logging behavior — verified against `internal/logger`

`logger.New` (Phase 2, already shipped) builds a `log/slog.Logger` with either `slog.NewTextHandler` or `slog.NewJSONHandler` depending on `cfg.Bridge.LogFormat` (`"text"` or default-to-`"json"`), at a level parsed from `cfg.Bridge.LogLevel` (`debug`/`info`/`warn`/`error`, default `info`). This is already the right choice for a headless daemon (`internal/logger/logger.go`'s own doc comment: "every operational signal the KOReader plugins surfaced through InfoMessage/Notification becomes a structured log line instead"). **No change proposed to the logger package itself.**

### 7.2 Startup/shutdown message contract (documentation, one small addition)

**Already logged today** (verified, `main.go`):
- One `info`-level "bridge starting" line with `version`, `mode`, `state_file`, `device_id`, `poll_interval` fields.
- On config-file-missing: a `fmt.Fprintf(os.Stderr, ...)` **before** the logger exists (unavoidable — the logger's own config comes from the config being loaded; this is a deliberate, minimal exception, not a design flaw).
- On graceful shutdown: `log.Info("shutdown requested; exiting")`.
- Per-pass failures in daemon mode: `log.Warn("sync: run once failed", "error", err)` (Phase 6, `Engine.Run`).
- Persist-on-exit failure: `log.Error("failed to save state", "error", err)` (in the `defer` block).

**Proposed addition (only one, tied to §2.2's startup probe):** an `info`/`warn` line immediately after engine construction, before entering the run loop, reporting the result of the one-time `bo.Auth(ctx)` startup probe:

- Success: `log.Info("bookorbit connectivity check passed")`
- Failure: `log.Warn("bookorbit connectivity check failed; sync will retry on schedule", "error", err)`

This is the only new log statement proposed in this entire document. Everything else in the "operational concerns" section is a description of already-correct behavior.

### 7.3 Recoverable vs. fatal errors — classification table

This table makes explicit what is implicit across `main.go` and `Engine.Run` today; no behavior changes, just documentation for operators:

| Error | Where it happens | Fatal (process exits) or recoverable? |
|---|---|---|
| Config file malformed / fails `Validate()` | `config.Load` | **Fatal** — process never starts |
| State file exists but is corrupt JSON | `st.Load()` | **Fatal** — process never starts |
| State file directory not writable | `st.Save()` at any point | **Fatal in `--once` mode** (propagates as `RunOnce`'s return error via `finish()`); **recoverable in daemon mode** (logged via `Engine.Run`'s `Warn`, loop continues — though every subsequent pass will also fail to persist, so state effectively stops advancing until the underlying disk issue is fixed; this is an accepted, Phase-6-inherited limitation, not new) |
| Readest auth failure (bad credentials, revoked refresh token, and re-auth from stored credentials also fails) | `readest.Client.PullBooks` → `Engine.pullBooks` | **Fatal in `--once` mode**; **recoverable (logged, retried next poll) in daemon mode** — per Phase 6's explicit, approved design |
| BookOrbit auth failure (bad `x-auth-user`/`x-auth-key`) | `bookorbit.Client.{MatchCheck,BulkProgress,UpdateProgress}` → `Engine` | same as above |
| Transient network/5xx/429 from either service | classified `outcomeRetry` by `Engine`'s classifiers | **Recoverable within the pass** (Phase 6 `withRetry`, bounded by `RetryMaxAttempts`); if retries are exhausted, treated as a batch failure, watermark retreats (Phase 6 §5.4), pass continues for other rows, **not fatal** |
| BookOrbit bulk-progress endpoint unsupported (older server) | `bookorbit.ErrUnsupportedEndpoint` | **Not an error at all from an operator's perspective** — silently and permanently (per-process) falls back to `UpdateProgress` (Phase 6 §8) |
| SIGINT/SIGTERM | any point | **Graceful, exit 0** |

### 7.4 Restart behavior

Given §7.3, systemd's `Restart=on-failure` (§6.1) will, in practice, almost never fire during normal operation — the daemon's own retry/backoff and "log and continue" philosophy (Phase 6) already absorbs everything except state-file corruption. This is **intentional** (matches `docs/implementation-brief.md`: "No UI... Polling daemon") and should be documented in `README.md`'s deployment section so an operator doesn't mistake "the service never restarts" for "the service is broken" when, say, Readest is down for an hour — the correct signal to watch is the `warn`-level log volume, not the systemd restart count.

### 7.5 Production deployment recommendations (documentation only)

- Run behind a `journald`-backed systemd unit; use `cfg.Bridge.LogFormat = "json"` in production so `journalctl -o json` / a log shipper can parse fields, reserving `"text"` for interactive `make run` / local debugging (this dual-format support already exists in `internal/logger`, no new work).
- Set `poll_interval` no lower than a few minutes; the reference plugins' own analogous cadence (kosync's push debounce, `readest_syncstats`'s cursor-based incremental pulls) and the reverse-engineering report's own recommendation ("5–60 min... user-configurable") both support a low-frequency default; `configs/bridge.example.yaml`'s already-commented `15m` default is a reasonable production starting point.
- Store `bridge-state.json` on persistent storage that survives container/VM restarts if deployed in Docker (a bind mount or named volume) — this is the *only* thing that needs to survive a restart; the binary itself is stateless otherwise.
- Treat `--once` + a system timer (`systemd.timer` or plain `cron`) as a legitimate alternative to the daemon mode for very low-frequency sync needs (e.g., once per day) — both modes are already fully supported by the existing CLI with no additional code, per `README.md`'s own documented usage.

---

## 8. Testing strategy

### 8.1 Gap: `cmd/bridge` has zero test coverage today

Every other package in the module (`internal/config`, `internal/logger`, `internal/util`, `internal/token`, `internal/readest`, `internal/bookorbit`, `internal/sync`, `internal/sync/state`) has a `_test.go` file with substantial coverage, verified by reading each one. `cmd/bridge/main.go` and `cmd/bridge/device.go` have none. This is the single clearest, most objective Phase 7 gap and should be the primary code deliverable once this design is approved.

### 8.2 Proposed unit-test matrix for `cmd/bridge`

The existing `run(argv []string) error` signature (already factored out of `main()` specifically to be testable — `main()` itself is a two-line wrapper) makes this straightforward without any refactor:

1. `--version` prints the version string and returns nil.
2. `-h`/`--help` returns nil (after the §3.3 fix), and does not print the `bridge:` error prefix (this needs `run`'s stdout/stderr to be capturable — either by temporarily redirecting `os.Stdout`/`os.Stderr` in the test, or, cleaner, if `run`'s signature already takes `argv []string` and could optionally be extended to accept explicit `io.Writer`s for output — flagged as a possible **test-only** signature tweak in §10, not a behavior change).
3. Unknown flag → non-nil error (post-fix: distinguishable from `flag.ErrHelp`).
4. Missing config file (`--config` pointing at a nonexistent path) with insufficient env vars set → `Validate()` fails → `run` returns a non-nil `*config.ValidationError`-wrapping error.
5. Missing config file **with** sufficient `BRIDGE_*` env vars set → `run` proceeds (this exercises `config.ErrNoConfigFile`'s "non-fatal" path all the way through engine construction) — this test would need to stop short of actually calling `engine.RunOnce`/`Run` against the real network; see §8.3 for how.
6. `resolveDeviceID` (`device.go`): explicit `cfg.BookOrbit.DeviceID` wins over anything persisted; a persisted `state.Store.DeviceID` is reused across calls; with neither set, a fresh UUID is generated and is a valid, stable v4 UUID (already partially testable via `state.MemStore`, which is already used throughout `internal/sync/state/state_test.go`).
7. `newUUID()` produces a syntactically valid RFC 4122 v4 UUID (version nibble `4`, variant bits `10`) across many invocations — a simple property test, no mocking needed.
8. `modeName(once, daemon bool) string` — pure function, trivial table test (`once=true`→"once"; anything else→"daemon").
9. **Signal-driven shutdown, end-to-end within `run`:** the trickiest case to test without real process signals. Two options:
   - (a) Extract the "build ctx, wire engine, dispatch on `once`" portion of `run` so a test can inject a pre-cancelled `context.Context` directly instead of relying on `signal.NotifyContext` — this would require a small, test-only seam (e.g., a package-level `newSignalContext` function variable, mirroring the existing `now`/`sleep` injectable-function convention already used in `readest.Auth`, `bookorbit.Client`, and `sync.Engine`). This is the **only** structural change to `main.go` proposed anywhere in this document, and it is purely a testability seam — it does not change runtime behavior.
   - (b) Test only up to `sync.Engine.Run`'s own already-verified cancellation behavior (Phase 6 §12 `TestRunRespectsCancellationDuringPollSleep`) and treat `main.go`'s `signal.NotifyContext` wiring itself as effectively untestable in a unit test (since sending real OS signals to `go test`'s own process is fragile and non-hermetic) — **acceptable and lower-effort**, given the underlying cancellation-propagation logic is already thoroughly tested one layer down.
   
   **Recommendation: (b).** Adding a test seam purely to unit-test three lines of standard-library signal wiring is disproportionate; the signal-to-context translation is `signal.NotifyContext`'s own well-tested standard-library behavior, not bridge logic. Flagged in §10 in case reviewers disagree.

### 8.3 How to test `run()` without hitting the network

`run()` currently constructs the *real* `readest.NewAuth`/`readest.NewClient`/`bookorbit.NewClient` unconditionally. For tests 4–5 above (which only need to exercise config/state/wiring, not actual sync), the cleanest option is to **stop the assertions before `engine.RunOnce`/`Run` is invoked** — i.e., structure new tests around a config that is guaranteed to fail validation (test 4) or, for test 5, accept that the constructed `engine` will attempt a real `RunOnce` and simply use `--once` against an unreachable/invalid `bookorbit.server_url` (e.g. `http://127.0.0.1:1` — a guaranteed-refused local connection), asserting that `run()` returns a non-nil error whose message contains the expected classification (`bookorbit: network error` or `readest: sync: network error`), which exercises the **entire** wiring path end-to-end (config → state → clients → engine → `RunOnce` → error propagation → `os.Exit` code) without any mocking and without depending on external services being reachable. This "point the client at a closed local port" technique needs no new test infrastructure — it is a plain config value, not a code change.

### 8.4 Integration tests

Per the project's own established convention (`docs/adr/phase-6-decision-record.md`: "All tests use `state.NewMemStore()`... and hand-written fakes... No real network"), the module has deliberately avoided a live-server integration-test tier throughout Phases 3–6, relying on stubbed transports (`stubDoer`) and documented **live-validation items** instead (Phase 4 §12, Phase 5 §16). Phase 7 should **preserve this convention** rather than introduce a new integration-test harness (e.g. `testcontainers`, a mock BookOrbit server, VCR-style HTTP cassettes) — doing so would be exactly the kind of "unnecessary dependency" and "future architectural churn" the brief asks to avoid, for a benefit (catching real-server drift) that the project has already chosen to handle via documented manual verification instead.

**Recommendation:** Phase 7 contributes **no new integration-test tier**. The `cmd/bridge` unit tests in §8.2/§8.3 (config → wiring → classified-error-on-unreachable-host) are the closest thing to an integration test this project's conventions call for, and they require zero new dependencies.

### 8.5 Manual verification checklist (for `README.md` / a release checklist, not automated)

1. `bridge --version` prints the expected version string built via `-ldflags`.
2. `bridge --help` / `bridge -h` prints usage and exits 0 (`echo $?`).
3. `bridge --config /nonexistent.yaml` with no env vars set prints the "invalid configuration" problem list and exits non-zero.
4. `bridge --once --config <valid config, real credentials>` against a real Readest account + real BookOrbit server performs one pull→match→push pass, updates `bridge-state.json`'s `watermarkMs`, and exits 0.
5. Running the same `--once` invocation a second time immediately afterward performs a near-instant pass (since `since` is now the advanced watermark) with zero or minimal BookOrbit calls (percentage-unchanged skip, Phase 6 verified).
6. `bridge --config <valid config>` (daemon mode) started under a terminal; `Ctrl+C` (SIGINT) produces the "shutdown requested; exiting" log line and the process exits 0 promptly (not after a full poll interval).
7. Same as 6, but sent `SIGTERM` (`kill -TERM <pid>`) instead — same result (the `signal.NotifyContext` call already listens for both).
8. Install the systemd unit (§6.1) on a test host, `systemctl start bridge`, confirm `systemctl status` shows "active (running)", `journalctl -u bridge` shows the startup log line, `systemctl stop bridge` produces a clean, prompt shutdown.
9. Deliberately misconfigure `bookorbit.password` and confirm the new startup probe (§2.2/§7.2) logs a `warn` line at startup rather than the operator discovering the problem only after `poll_interval` minutes pass.

### 8.6 Deployment validation

- Confirm `make release`'s output binaries are actually static (`file bin/bridge-linux-amd64` should show "statically linked" — already true today per the existing single-target `make build`, just needs re-confirming per cross-compiled artifact).
- Confirm the systemd unit's hardening directives (`ProtectSystem=strict`, etc.) don't block the state file's read/write path — verify via `systemd-analyze security bridge.service` after installation, matching the sandboxing already implied by the state file's `0600` permission model.

---

## 9. Explicit non-goals for Phase 7 (stated for clarity, not because they were asked for)

- No `bridge login`/`bridge setup` subcommand (§2.1) — already handled by Phase 3's `Auth.AccessToken`.
- No container image in this phase (§6.3) — deferred, trivial follow-on once a static release binary exists.
- No new integration-test tier (§8.4) — preserves the project's existing stubbed-transport testing convention.
- No shutdown-timeout/force-kill logic in the application itself (§4.4) — systemd's `TimeoutStopSec` already owns this.
- No changes to `internal/config`, `internal/logger`, `internal/readest`, `internal/bookorbit`, or `internal/sync` — Phase 7 is `cmd/bridge` + packaging only, as scoped.

---

## 10. Decisions requiring approval before Phase 7 coding begins

**A.** Skip building a `bridge login`/setup subcommand; rely entirely on the existing `readest.Auth.AccessToken` lazy sign-in (§2.1). *Recommended: approve — this is already fully implemented and tested in Phase 3.*

**B.** Add a one-time BookOrbit connectivity probe (`bo.Auth(ctx)`) at startup, logged at `info`/`warn`, **non-fatal**, with no Readest-side equivalent (§2.2, §7.2). This is the only new runtime *behavior* in this whole document. *Recommend: approve — small, well-scoped, uses an already-existing client method (`bookorbit.Client.Auth`, Phase 5), addresses a real first-run support gap.*

**C.** Fix `-h`/`--help` to exit 0 instead of 1 and stop printing the spurious `bridge: flag: help requested` line (§3.3). *Recommend: approve — pure bug fix, no behavioral trade-off.*

**D.** Exit code scheme: keep the current 0/1 scheme rather than adding a distinct exit code 2 for CLI usage errors (§3.4). *Recommend: approve keeping 0/1 (do not add exit code 2) to avoid restructuring `run()`'s error-reporting shape for a benefit that mostly matters to shell-scripting purists.*

**E.** Keep `--daemon` as a documentation-only, functionally-inert flag (§3.2) — no code change, just confirming the existing shipped behavior is intentional and should stay. *Recommend: approve, no change needed.*

**F.** Add `systemd/bridge.service` as a new deployment artifact with the directives described in §6.1 (`Type=simple`, `Restart=on-failure`, hardening directives, `EnvironmentFile`). *Recommend: approve — directly requested by the roadmap and by `docs/planning.md`'s stated deployment target.*

**G.** Add a `release` target to the `Makefile` cross-compiling for `linux/amd64`, `linux/arm64`, and optionally `darwin/{amd64,arm64}` (§6.2), plus a CI release workflow description running the full test suite as a gate before publishing (§6.3). *Recommend: approve the Makefile target now; treat the CI workflow file itself as an implementation detail to author alongside it (still no new dependencies — a standard Go-toolchain-only GitHub Actions job).*

**H.** Defer container image publishing entirely (§6.3, §9). *Recommend: approve deferring — no architectural blocker, easy to add later from the same static binaries.*

**I.** Add unit tests for `cmd/bridge` (`main.go`, `device.go`) per the matrix in §8.2, testing signal-driven shutdown only indirectly (option (b): rely on `sync.Engine`'s already-verified cancellation behavior, rather than adding a test-only seam around `signal.NotifyContext`) (§8.2 item 9). *Recommend: approve; this is the single most concrete code gap identified in this whole investigation.*

**J.** No new integration-test tier; preserve the existing stubbed-transport-only testing convention, using "point the client at a closed local port" as the closest thing to an end-to-end wiring test (§8.3, §8.4). *Recommend: approve — consistent with every prior phase's testing philosophy.*

Awaiting sign-off on A–J before any Phase 7 code (tests, `main.go`/`device.go` fixes, `systemd/bridge.service`, `Makefile` `release` target) is written.
