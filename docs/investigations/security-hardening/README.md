# Security Hardening — BookOrbit URL scheme/loopback validation & cleartext-credential transport guard

**Status:** Complete ("the two compounding problems the design fixes are now loud at config load instead of silent at request time" — [decision-record.md](./decision-record.md)).
**Design:** [design.md](./design.md)
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`internal/util/loopback.go`](../../../internal/util/loopback.go) (new), [`internal/util/url.go`](../../../internal/util/url.go), [`internal/util/util_test.go`](../../../internal/util/util_test.go), [`internal/config/{types,env,load,validate,config_test}.go`](../../../internal/config/), [`cmd/bridge/main.go`](../../../cmd/bridge/main.go), [`cmd/bridge/main_test.go`](../../../cmd/bridge/main_test.go), and four docs touch-ups (`configs/bridge.example.yaml`, `docker-compose.yml`, `README.md`, `systemd/bridge.service`).

> This is a hardening pass, **not** a feature phase — it is filed under `investigations/` rather than `phases/` because it was a parallel-track work item that landed between phases without a phase number.

## What this work shipped

Two additive validation surfaces:

- **BookOrbit URL scheme + loopback-tolerance validation** at config-load time. `https` is the recommended scheme (password-equivalent `x-auth-key` over cleartext is rejected loudly unless the operator explicitly opts in via `bookorbit.allow_insecure_transport`). Loopback endpoints (`127.x` / `localhost` / `::1` / `.local` mDNS) are tolerated under cleartext on a best-effort basis.
- **Cleartext-egress guard** for the password-equivalent `x-auth-key` MD5 — never fixable in the bridge (it's a fixed property of the BookOrbit wire protocol), but reachable-by-accident is now reachable-by-config-load-error instead of by-request-time-leak.

No sync-engine, BookOrbit-wire-protocol, or Readest-auth-lifecycle behavior was altered — the change only constrains and warns about operator-supplied configuration.

## What shipped DIFFERENTLY from the design

Decisions A–J were implemented as proposed in [design.md §3](./design.md). See [decision-record.md "Decisions implemented"](./decision-record.md#decisions-implemented-aj-per-the-approved-design-3) for the table.

## Live-validation items this work opened

None. The validation is unit-test-driven; the hardening surfaces only operator-supplied config, so there is no third-party server state to validate against.

## Tests

[`internal/util/util_test.go`](../../../internal/util/util_test.go) grown for the new `IsLoopback`/URL-scheme functions; [`internal/config/config_test.go`](../../../internal/config/config_test.go) for the new validation rules and the `allow_insecure_transport` opt-in flag; [`cmd/bridge/main_test.go`](../../../cmd/bridge/main_test.go) for the new startup validation errors. `go build ./...`, `go vet ./...`, `gofmt -l .`, `go test ./...`, `go test -race ./...` all green.
