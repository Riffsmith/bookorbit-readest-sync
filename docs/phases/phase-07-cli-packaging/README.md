# Phase 7 — CLI, Packaging, Deployment

**Status:** Complete and verified.
**Design:** [design.md](./design.md) — its top banner records the Phase 6 follow-up resolution that unblocked this phase.
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`cmd/bridge/main.go`](../../../cmd/bridge/main.go), [`cmd/bridge/main_test.go`](../../../cmd/bridge/main_test.go), [`cmd/bridge/device.go`](../../../cmd/bridge/device.go), [`cmd/bridge/device_test.go`](../../../cmd/bridge/device_test.go), [`Makefile`](../../../Makefile), [`systemd/bridge.service`](../../../systemd/bridge.service), `README.md` (deployment section).
**Depends on:** [Phase 6 — Sync Engine](../phase-06-engine/README.md).
**Unblocked by this phase:** [Phase 8 — Tests + Validation](../phase-08-tests-validation/README.md)

## What this phase shipped

The executable shell around the verified Phase 6 engine: signal-hardened `cmd/bridge` with `--config` / `--once` / `--daemon` (inert by design — Decision E, the engine's poll loop already runs both modes identically) / `--version` / `-h`/`--help` (exit 0, no spurious `bridge: flag: help requested` prefix); a one-time non-fatal BookOrbit connectivity probe (`bo.Auth(ctx)`) at startup logged `info`/`warn`; `Makefile release` target for cross-compiled static Linux binaries (amd64 + arm64, `CGO_ENABLED=0`); `systemd/bridge.service` with hardening directives (`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectedHome`, `PrivateTmp`, dedicated `User=bridge`, `TimeoutStopSec=40`); `cmd/bridge` unit tests.

## What shipped DIFFERENTLY from the design

- **Makefile `release` target is Linux-only** (the design proposed `linux/amd64`, `linux/arm64`, plus optional `darwin/{amd64,arm64}`). Operator instruction explicitly narrowed this to "static cross-compiled **Linux** builds"; adding `darwin` targets later is one line in `RELEASE_PLATFORMS`. See [decision-record.md "Deviation from the design"](./decision-record.md#deviation-from-the-design-the-full-wiring-test-uses-httptestserver-not-a-closed-port).

- **The end-to-end wiring test uses `httptest.Server`, not a closed port** (the design proposed pointing `bookorbit.server_url` at `127.0.0.1:1`). A closed-port network error is classified `outcomeRetry` and would force the engine's real exponential backoff (~15.5 s of real sleeping) with no config-level way to shorten it. The `httptest.Server` responds with a valid token then `400 Bad Request` to `/sync`, which classifies as `outcomeSkip` and never retries. Same construction chain, deterministic, single-digit milliseconds. See `TestRunOnceFullWiringSurfacesClassifiedError` in [`cmd/bridge/main_test.go`](../../../cmd/bridge/main_test.go).

## Live-validation items this phase opened

- **L1 — `SIGTERM` as a distinct signal** (the auto-shutdown signal the systemd unit sends). Open, scheduled for a live run. Tracked in [`../../live-validation-status.md`](../../live-validation-status.md).

## Tests

[`cmd/bridge/main_test.go`](../../../cmd/bridge/main_test.go): `--version`, `-h`/`--help` (both spellings), unknown-flag not misclassified as help, `modeName` pure-function table, missing-config surfaces `*config.ValidationError`, full-wiring test against `httptest.Server`. [`cmd/bridge/device_test.go`](../../../cmd/bridge/device_test.go): 200 UUIDs are valid v4 + unique; `resolveDeviceID` precedence (explicit > persisted > fresh-generated-and-persisted). `systemd-analyze verify` passes against a dummy `ExecStart` binary.
