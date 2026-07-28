# BookOrbit ↔ Readest Sync Bridge

A standalone Go daemon/CLI that syncs **reading progress** from **Readest**
(the source of truth) into **BookOrbit** (write-only), replacing the KOReader
plugins that previously relayed it.

```
Readest → Readest Sync → Standalone Bridge → BookOrbit
```

> **Status: foundation phase.** The project structure, configuration, logging,
> utilities, persistent state store, shared models, and HTTP/client interfaces
> are in place and compile cleanly. No sync, auth, or network behavior is
> implemented yet — that lands in the roadmap phases below.

---

## What it does (when complete)

- Polls the Readest sync API for books changed since the last watermark.
- Resolves each book's partial-MD5 hash to a BookOrbit library entry
  (`match-check`), caching the result.
- Pushes progress percentages to BookOrbit in batches (`bulk-progress`).
- Refreshes the Supabase token proactively and persists everything in a small
  local state file.

One-way only: Readest wins, BookOrbit never writes back. No annotations,
highlights, or status sync.

## Requirements

- Go 1.26+ (see `go.mod`)
- A Readest account (hosted `web.readest.com` sync)
- A self-hosted BookOrbit server and credentials

## Build

```sh
make build          # static binary → bin/bridge
# or
go build ./cmd/bridge
```

## Configure

```sh
cp configs/bridge.example.yaml configs/bridge.yaml
# edit configs/bridge.yaml, or export the BRIDGE_* env vars (see the example
# file header for the full list). Secrets are best supplied via env vars.
```

At minimum you must provide `readest.email`, `readest.password`,
`bookorbit.server_url`, `bookorbit.username`, and either `bookorbit.password`
or `bookorbit.userkey`.

## Run

```sh
bridge --config configs/bridge.yaml            # daemon (default)
bridge --config configs/bridge.yaml --once     # single pass (cron/systemd timer)
bridge --version
```

During the foundation phase, running a sync pass logs that the engine is not
implemented yet and exits cleanly — this is expected.

## Development

```sh
make test      # unit tests
make vet       # go vet
make fmt       # gofmt -s
make tidy      # go mod tidy
```

### Layout

| Path | Purpose |
|------|---------|
| `cmd/bridge` | CLI entrypoint: flags, signal handling, wiring |
| `internal/config` | Config loading: defaults, file, env overrides, validation |
| `internal/logger` | Structured logging setup (`log/slog`) |
| `internal/util` | Pure helpers: time, percentage, URL, MD5, batching |
| `internal/util/httpclient` | `Doer` transport seam for mockable HTTP |
| `internal/readest` | Readest models, auth + sync-client interfaces (stubs) |
| `internal/bookorbit` | BookOrbit models + client interface (stub) |
| `internal/sync` | Orchestration engine (stub) |
| `internal/sync/state` | Persistent state store (file + in-memory) |
| `configs` | Example configuration |
| `docs` | Specs, reverse-engineering report, roadmap |
| `reference` | Read-only KOReader plugins used as the spec |

### Design notes

- `internal/readest` and `internal/bookorbit` never import each other; only
  `internal/sync` knows both, so new targets can be added without touching the
  Readest client.
- The HTTP transport is behind a small `Doer` interface so clients are mockable
  and retry/backoff policy has one home.
- State is the only persistence; it is written atomically with `0600`
  permissions because it holds tokens.

## Roadmap

See `docs/implementation-roadmap.md`. Foundation (Phase 0–2) is done; next are
Readest auth (Phase 3), the Readest sync client (Phase 4), the BookOrbit client
(Phase 5), the sync engine (Phase 6), and CLI/packaging + tests (Phases 7–8).

## License

TBD
