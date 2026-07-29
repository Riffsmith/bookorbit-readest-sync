# BookOrbit ↔ Readest Sync Bridge

A standalone Go daemon/CLI that syncs **reading progress** from **Readest**
(the source of truth) into **BookOrbit** (write-only), replacing the KOReader
plugins that previously relayed it.

```
Readest → Readest Sync → Standalone Bridge → BookOrbit
```

> **Status: Phase 6 complete.** Foundation (config, logging, utilities, state
> store), Readest Supabase auth + sync client, BookOrbit client, and the sync
> engine are all implemented and unit-tested. The bridge can authenticate to
> Readest, pull the books table, match-check against BookOrbit, and push
> batched progress. What remains is packaging polish (Phase 7) and formal
> integration-test validation against live servers (Phase 8) — see
> `docs/implementation-roadmap.md`. Manual end-to-end runs against real
> accounts work today (see "Testing against live accounts" below).

---

## What it does

- Polls the Readest sync API for books changed since the last watermark.
- Resolves each book's partial-MD5 hash to a BookOrbit library entry
  (`match-check`), caching the result.
- Pushes progress percentages to BookOrbit in batches (`bulk-progress`), with
  an automatic fallback to per-book `update-progress` PUTs if the bulk
  endpoint is unsupported by the server.
- Refreshes the Supabase token proactively (50 % TTL threshold + 60 s guard,
  matching the reference plugin) and persists everything in a small local
  state file.

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
or `bookorbit.userkey` (see "BookOrbit authentication" below).

## BookOrbit authentication

BookOrbit's protocol authenticates every request with two headers:

- `x-auth-user`: your BookOrbit username
- `x-auth-key`: `md5(<password>)` as a 32-char lowercase hex string

**Nothing on the BookOrbit side is stored in plaintext** — only the MD5
digest ever leaves the account, and the bridge sends exactly that digest
verbatim. You have two equivalent ways to supply it; pick whichever is
convenient.

### Option A — password (the bridge hashes it for you)

Set `bookorbit.password` (or `BRIDGE_BOOKORBIT_PASSWORD`) to your BookOrbit
account password. The bridge MD5-hashes it on startup and uses the result as
`x-auth-key`. This is the simplest path if you already know the password.

```sh
export BRIDGE_BOOKORBIT_USERNAME="your-username"
export BRIDGE_BOOKORBIT_PASSWORD="your-password"
export BRIDGE_BOOKORBIT_SERVER_URL="https://your-bookorbit.example.com"
```

### Option B — pre-hashed `userkey` (no password required)

Set `bookorbit.userkey` (or `BRIDGE_BOOKORBIT_USERKEY`) to a 32-char
lowercase hex MD5 of your BookOrbit password. The bridge uses it verbatim.
If both `password` and `userkey` are set, `userkey` wins.

This is the most useful option because **the BookOrbit web UI will hand you
this exact value pre-computed**, so you never have to type or hash the
password yourself. Two ways to obtain it:

1. **From a preconfigured plugin download (recommended).** BookOrbit's web
   settings has a "Download preconfigured plugin" action that bundles a
   `bookorbit_provision.lua` file into the plugin zip. That file already
   contains `{ server_url, username, userkey }` — the server MD5-hashed your
   password when you registered the device. Unzip the download and read
   `userkey` straight out of `bookorbit_provision.lua` (or copy the three
   fields directly into your `bridge.yaml` / env vars). The same zip also
   gives you the canonical `server_url` and `username`, so everything you
   need for the bridge's BookOrbit side ships in one file.

2. **From an already-provisioned KOReader install.** If you've ever run the
   preconfigured plugin in KOReader, the same value was persisted into
   `G_reader_settings` as `bookorbit.userkey` (KOReader's
   `settings.reader.lua` on most setups). Read it from there.

Either way, the wire result is byte-identical to Option A — both paths
converge on `x-auth-key = md5(password)`, persisted verbatim and never
re-hashed by the bridge. The server is the source of the pre-provisioned
value; the bridge only relays it.

> Reference: the preconfigured-plugin workflow is `BookOrbit:applyProvision`
> in `reference/koreader-plugin/bookorbit.koplugin/main.lua` (lines 188-232),
> and the manual-login MD5 path is `MainMenu:doLogin` in
> `bookorbit_main_menu.lua` (lines 649-678).

## Run

```sh
bridge --config configs/bridge.yaml            # daemon (default)
bridge --config configs/bridge.yaml --once     # single pass (cron/systemd timer)
bridge --version
```

`--once` runs a single sync pass and exits — the recommended mode for manual
verification and for cron/systemd-timer deployments. The default daemon mode
polls on `bridge.poll_interval`.

## Testing against live accounts

The bridge is fully wired against the real Readest and BookOrbit APIs as of
Phase 6, so you can run a real end-to-end sync today. A few live-server
behaviors were left unverified by the unit tests (see
`docs/adr/phase-5-decision-record.md` and `docs/reverse-engineering-report.md`
§5); a manual run is the way to confirm or surface them.

1. **Build** the binary: `make build` → `bin/bridge`.
2. **Provide credentials.** Either edit `configs/bridge.yaml` (preferred for a
   persistent setup) or export the `BRIDGE_*` env vars (preferred for secrets).
   The four required fields are `readest.email`, `readest.password`,
   `bookorbit.server_url`, and `bookorbit.username` plus one of
   `bookorbit.password` or `bookorbit.userkey`. The full env-var list is in the
   header of `configs/bridge.example.yaml`.
3. **Run one pass with debug logging:**

   ```sh
   BRIDGE_LOG_LEVEL=debug ./bin/bridge --once --config configs/bridge.yaml
   ```

   You should see, in order: Supabase login → `GET /sync?type=books` →
   `match-check` for unknown hashes → `bulk-progress` push for changed books.
4. **Verify the round-trip in BookOrbit.** Open a book in Readest on any
   device, wait for Readest Sync to upload it (usually within a minute), re-run
   `--once`, then check the book's reading progress in the BookOrbit UI.
5. **State file.** `bridge-state.json` (default path, `0600`) is created on
   first run and holds the Supabase tokens, pull watermark, per-hash match
   cache, and the unmatched-cooldown map. Delete it to force a fresh full
   re-pull and re-match.
6. **Daemon mode** (optional):

   ```sh
   ./bin/bridge --config configs/bridge.yaml
   BRIDGE_POLL_INTERVAL=5m ./bin/bridge --daemon --config configs/bridge.yaml
   ```

   SIGINT/SIGTERM triggers graceful shutdown and a final state save.

### Known live-server unknowns to watch for

- Whether BookOrbit accepts an **empty `progress` string** in
  `bulk-progress`/`update-progress`. The bridge sends `progress: ""` because
  it has no open document; if pushes come back as `ErrBadRequest`, this is the
  issue surfacing (the referenced fallback `"0"` is not yet implemented).
- The **exact status code** BookOrbit returns for an unsupported endpoint,
  which drives the bulk→single-PUT fallback in `engine.RunOnce`. If the wrong
  code comes back, the fallback won't trigger and you'll see bulk-progress
  failures instead.
- Whether BookOrbit honors **older `updated_at` seconds** or silently keeps a
  newer value it already has — harmless for one-way sync either way, but
  progress may appear "stuck" if the server discards older writes.
- Re-running `--once` with unchanged percentages **will not re-push** —
  `lastPushedPct` deduplication is intentional, so trial runs are safe and
  won't spam BookOrbit.

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
| `internal/readest` | Readest models, Supabase auth (password grant + refresh), sync client |
| `internal/bookorbit` | BookOrbit models + REST client (match-check, bulk-progress, update-progress fallback) |
| `internal/sync` | Orchestration engine: pull → diff → match → push, watermark, retry/backoff |
| `internal/sync/state` | Persistent state store (file + in-memory): tokens, match cache, watermark, device id |
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

See `docs/implementation-roadmap.md`. Phases 0–6 are complete (foundation,
config/utilities/logging/state, Readest auth + sync client, BookOrbit client,
sync engine). Remaining: Phase 7 (CLI/packaging polish, systemd unit,
Makefile) and Phase 8 (formal integration-test validation against live
servers) — both suitable as follow-ups now that Phase 6 is in place.

## License

TBD
