# Phase 7 — Decision Record

**Status:** Implemented and verified. Scope was intentionally tight, per the
approved design (`docs/phase-7-design.md`) and the explicit go-ahead: `cmd/bridge`
plus deployment artifacts only. No file under `internal/config`,
`internal/logger`, `internal/readest`, `internal/bookorbit`, or `internal/sync`
was touched. `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...`,
and `go test -race ./...` all pass, including the new `cmd/bridge` tests.

Phase 7 is complete. Phase 8 (formal integration-test validation against live
servers) is the only remaining item on `docs/implementation-roadmap.md`.

---

## Decisions implemented (A–J, per the approved design)

| # | Decision | Status |
|---|---|---|
| A | No `bridge login`/setup subcommand; rely on `readest.Auth.AccessToken`'s lazy sign-in | **Unchanged, confirmed** — no code needed |
| B | One-time, non-fatal BookOrbit connectivity probe (`bo.Auth(ctx)`) at startup, logged `info`/`warn` | **Implemented** |
| C | Fix `-h`/`--help` to exit 0, stop the spurious `bridge: flag: help requested` line | **Implemented** |
| D | Keep the 0/1 exit-code scheme; no new exit code 2 | **Unchanged, confirmed** — no code needed |
| E | Keep `--daemon` as a documentation-only, functionally inert flag | **Unchanged, confirmed** — no code needed |
| F | Add `systemd/bridge.service` with the directives from design §6.1 | **Implemented** |
| G | Add a Makefile `release` target for static cross-compiled binaries | **Implemented — Linux only** (see deviation below) |
| H | Defer container image publishing | **Deferred, as instructed** |
| I | Add `cmd/bridge` unit tests (help, mode selection, device ID, startup wiring) | **Implemented** |
| J | No new integration-test tier | **Confirmed** — the one wiring test uses `httptest.Server`, not a new harness |

---

## What changed

### `cmd/bridge/main.go`

- `run(argv []string) error` became `run(argv []string, stdout, stderr io.Writer) error`.
  `main()` now calls `run(os.Args[1:], os.Stdout, os.Stderr)`, so production
  behavior is byte-for-byte unchanged — this is the test-only signature seam
  the design flagged as optional in §8.2 item 2, taken because it makes
  `--version`/`--help`/wiring output assertable without touching the real
  process streams. `fs.SetOutput(stderr)` was added for the same reason (the
  flag package's default usage output already goes to `os.Stderr` in
  production, so this is not a behavior change either).
- `fs.Parse(argv)`'s error handling now special-cases `errors.Is(err,
  flag.ErrHelp)` and returns `nil` — `-h`/`--help` already printed usage via
  the flag package's own `f.usage()` call before returning `ErrHelp`; nothing
  else was added or replaced. Verified manually: `bridge --help` and
  `bridge -h` now exit 0 with clean usage output and no `bridge:` prefix; an
  actually-unknown flag (`--nope`) still exits 1 and is not misclassified as
  help (see `TestRunUnknownFlagIsNotClassifiedAsHelp`).
- One-time BookOrbit connectivity probe added, placed after `ctx` is created
  (`signal.NotifyContext`) and before the `--once`/daemon branch, so it runs
  exactly once regardless of mode: `boClient.Auth(ctx)`, logged at `info` on
  success or `warn` on failure. Never returns an error from `run()` — matches
  design §2.2/§7.2 exactly. No Readest-side probe was added, per the design's
  explicit recommendation against inventing a new client method for one log
  line.
- No other control-flow, construction-order, or dependency change. The
  daemon-mode comment was expanded by one line to record *why* `--daemon` is
  inert (design §3.2/Decision E), not to change behavior.

### `cmd/bridge/main_test.go` (new)

- `TestRunVersion` — `--version` prints the version string to the injected
  stdout and returns nil.
- `TestRunHelp` — both `-h` and `--help` return nil and print `Usage` to
  stderr.
- `TestRunUnknownFlagIsNotClassifiedAsHelp` — an unknown flag returns a
  non-nil error that is not `flag.ErrHelp`.
- `TestModeName` — pure-function table test over all four `(once, daemon)`
  combinations, including the `daemon=true` case that pins Decision E
  (`--daemon` never changes the effective mode).
- `TestRunMissingConfigInsufficientEnvFailsValidation` — a nonexistent
  `--config` path with no credentials set surfaces `*config.ValidationError`
  via `errors.As`, fast, with no engine construction reached.
- `TestRunOnceFullWiringSurfacesClassifiedError` — the end-to-end wiring
  test; see "Deviation from the design" below for why it uses a local
  `httptest.Server` instead of the design's literal closed-port suggestion.

### `cmd/bridge/device_test.go` (new)

- `TestNewUUIDIsValidV4` — 200 generated IDs, each checked against an RFC
  4122 v4 pattern (version nibble `4`, variant nibble `8|9|a|b`) and checked
  for uniqueness across the run.
- `TestResolveDeviceIDExplicitConfigWins` — `cfg.BookOrbit.DeviceID` beats a
  pre-seeded persisted value.
- `TestResolveDeviceIDReusesPersisted` — a persisted value is returned as-is
  when no explicit config value is set.
- `TestResolveDeviceIDGeneratesAndPersistsFreshUUID` — with neither set, a
  fresh v4 UUID is generated and is stable across a second call in the same
  process (the underlying `state.Store.DeviceID` persists it after first
  generation).

### `Makefile`

- Added `RELEASE_PLATFORMS ?= linux/amd64 linux/arm64` and a `release`
  target that loops over it, building
  `$(BUILD_DIR)/$(BINARY)-$$os-$$arch` with the same `CGO_ENABLED=0`,
  `-trimpath`, and `-ldflags` the existing `build` target already uses, so
  version embedding and static linking are identical between `make build`
  and `make release`. Added to `.PHONY`.
- **Deviation from the design, per your explicit instruction:** the design
  (§6.2) proposed `linux/amd64`, `linux/arm64`, and *optionally*
  `darwin/{amd64,arm64}`. Your task instructions specifically said "static
  cross-compiled **Linux** builds," so `darwin` targets were left out
  entirely. This is a narrower, not a superset, change — adding darwin
  targets later is a one-line addition to `RELEASE_PLATFORMS` with no other
  change, if ever wanted.

### `systemd/bridge.service` (new)

Implements every directive design §6.1 called for: `Type=simple`,
`ExecStart=/usr/local/bin/bridge --daemon --config /etc/bridge/bridge.yaml`,
`Restart=on-failure` / `RestartSec=10`, `WorkingDirectory=/var/lib/bridge`,
an optional `EnvironmentFile=-/etc/bridge/bridge.env` for secrets,
a dedicated unprivileged `User=`/`Group=bridge`, `TimeoutStopSec=40`
(slightly above the default `bridge.http_timeout` of 30s), and the
hardening set (`NoNewPrivileges`, `ProtectSystem=strict` with
`ReadWritePaths=/var/lib/bridge`, `ProtectHome`, `PrivateTmp`). Verified with
`systemd-analyze verify` against a dummy executable at the `ExecStart` path —
passes with no warnings.

### `README.md`

Additive only, no rewrite of unrelated sections:

- Status banner updated from "Phase 6 complete" to "Phase 7 complete."
- `## Run` section: added `--daemon` and `--help` to the command list, a
  paragraph on the startup connectivity probe, and a paragraph documenting
  the 0/1 exit-code scheme.
- `## Development`: added the `make release` line.
- New `## Deployment` section (systemd install steps, `make release`
  description, explicit note that a container image is deferred, not
  rejected).
- `## Roadmap`: updated to "Phases 0–7 complete... Remaining: Phase 8."
- `### Layout` table: added a row for `systemd/`.

---

## Deviation from the design: the full-wiring test uses `httptest.Server`, not a closed port

Design §8.3 proposed, for the "missing config + sufficient env, exercises
the whole call chain" test, pointing `bookorbit.server_url` at an
unreachable local port (e.g. `http://127.0.0.1:1`) and asserting the
resulting classified network error. I implemented the equivalent test
(`TestRunOnceFullWiringSurfacesClassifiedError`) against a local
`httptest.Server` instead, for one concrete reason: a closed-port network
error is classified `outcomeRetry` by `internal/sync/engine.go`'s
`classifyReadestErr`, and the engine's real (non-mocked, per the project's
own convention) exponential backoff would then run — `RetryInitialBackoff`
500ms doubling up to `RetryMaxAttempts` 5, i.e. ~15.5s of real sleeping
before the pull step gives up, with no config-level way to shorten it (there
is no environment-variable override for `retry_max_attempts`, and adding one
would touch `internal/config`, out of this phase's scope).

The `httptest.Server` I used instead responds to `/auth/v1/token` with a
valid token (so `PullBooks` actually reaches the sync call), then to `/sync`
with **400 Bad Request** — `classifyReadestErr` maps `readest.ErrBadRequest`
to `outcomeSkip`, which the engine's `withRetry` never retries. The test is
therefore deterministic and takes single-digit milliseconds, while still
exercising the identical chain the design asked for: config load (missing
file, non-fatal `ErrNoConfigFile` path) → state store → device ID
resolution → Readest auth + sync client construction → BookOrbit client
construction → engine construction → the startup probe → `RunOnce` → error
classification → propagation out of `run()`. Nothing here is mocked; it is
still "point the client at a real server that fails," just a same-process
`httptest.Server` instead of a closed port, and it still uses zero new test
infrastructure beyond the standard library — consistent with Decision J (no
new integration-test tier).

---

## Verification performed

```
$ go build ./...        # clean
$ go vet ./...           # clean
$ gofmt -l .             # no files reported
$ go test ./...          # all packages green, including cmd/bridge (previously "no test files")
$ go test ./... -race    # all packages green
$ make release           # bin/bridge-linux-amd64, bin/bridge-linux-arm64
$ file bin/bridge-linux-amd64   # ELF 64-bit LSB executable, x86-64, statically linked, stripped
$ file bin/bridge-linux-arm64   # ELF 64-bit LSB executable, ARM aarch64, statically linked, stripped
$ systemd-analyze verify systemd/bridge.service   # clean (against a dummy ExecStart binary)
```

Manual CLI smoke test (built binary):

| Invocation | Exit code | Notes |
|---|---|---|
| `bridge --help` | 0 | prints `Usage of bridge:` + flag list to stderr, no `bridge:` prefix |
| `bridge -h` | 0 | same |
| `bridge --version` | 0 | prints `0.1.0-dev` to stdout |
| `bridge --nope` | 1 | unknown-flag error, unaffected by the `ErrHelp` fix |

---

## Explicitly out of scope (confirmed, not revisited)

Per the design's own §9 and this phase's instructions: no `bridge login`
subcommand, no container image, no new integration-test tier, no shutdown-
timeout/force-kill logic in the application (that's `TimeoutStopSec`'s job),
and no changes to `internal/config`, `internal/logger`, `internal/readest`,
`internal/bookorbit`, or `internal/sync`. Signal-driven shutdown itself
(`signal.NotifyContext` wiring) was **not** given a dedicated `cmd/bridge`
test, per the design's own recommendation (§8.2 item 9, option b): the
underlying cancellation-propagation behavior is already covered one layer
down by `internal/sync/engine_test.go`'s
`TestRunRespectsCancellationDuringPollSleep`, and adding a test-only seam
around three lines of stdlib signal wiring was judged disproportionate.

---

## Open items for Phase 8

None introduced by this phase beyond what `docs/phase-7-design.md` already
listed as deferred (container image, CI release workflow authoring). Phase 8
remains: formal integration-test validation against live Readest/BookOrbit
servers per `docs/implementation-roadmap.md`.
