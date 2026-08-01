# BookOrbit Readest Sync Bridge

A standalone Go service that syncs reading progress from Readest into BookOrbit. Inspired by the KOReader plugins for both services.

```
Readest -> Readest Sync -> This Bridge -> BookOrbit
```

> This is a personal project that I have made for my usecase. Anyone is welcome to use it if they want. However, do note that AI did the heavy lifting on this project - tests, docs, and help in implementation - with me a steering the important decision and verifying against live accounts.

## What it does

Polls the Readest sync API on an interval, matches books to your BookOrbit library by file hash, and pushes progress percentages. Optionally pushes `finished`/`abandoned` reading status (see `docs/future-status-sync.md`). One-way only. Syncing highlights and notes is in the roadmap. I'll probably implement it when I know for sure the primary features i.e. progress and status sync works without a hitch.

Caveat: only validated against a small personal library. If yours exceeds ~300 books, watch for slow first-pull behavior or untested edge cases.

## Install with Docker (recommended)

Requires only Docker. No Go toolchain or YAML config file. Everything is set via environment variables inside docker-compose.yml.

1. This bridge talks to BookOrbit through BookOrbit's own KOReader plugin protocol, so BookOrbit will only accept it once your account has KOReader credentials. In the BookOrbit web UI go to Settings -> KOReader, enable it, and pick a username and password.

2. Find your BookOrbit credentials to feed the bridge. You already know the username and password since you set them yourself; the password can be supplied directly and the bridge will hash it. If you'd rather not type the password, the BookOrbit web UI's Download preconfigured plugin action produces a zip with a bookorbit_provision.lua file that already contains the server_url, username, and a pre-hashed userkey, copy all three straight into your docker-compose.yml (or bridge.yaml). The same userkey is also present in any KOReader install that has previously run the preconfigured plugin, under bookorbit.userkey in its settings file.

3. Copy docker-compose.yml from this repo and fill in the environment: block: your Readest email and password, and the BookOrbit server URL, username, and either the password (BRIDGE_BOOKORBIT_PASSWORD) or the pre-hashed key (BRIDGE_BOOKORBIT_USERKEY) from step 2. If both are set, userkey wins.

4. Optional tweaks, all in the same block: BRIDGE_POLL_INTERVAL (default 15m), BRIDGE_SYNC_STATUS (true to also mirror finished/abandoned status, default off), BRIDGE_LOG_LEVEL, device name/id, and everything else, documented inline in the file.

5. Run:

   ```sh
   docker compose up -d
   ```

   Useful follow-ups:

   ```sh
   docker compose logs -f          # watch the sync
   docker compose run --rm bridge --once   # single test pass
   docker compose down             # stop (state volume is kept)
   ```

Sync state lives in the `bridge-state`
volume. The image is built and published by GitHub Actions
(`.github/workflows/docker.yml`) to GHCR with three tag channels:

- `:stable` - tip of `main`, updates on each merge (what the compose file pulls)
- `:latest` - last release tag (set when you push `v*`); for users who want
  only released versions
- `:dev` - tip of `dev` branch (bleeding edge)

To build the image yourself instead of pulling, uncomment the `build: .` line
in `docker-compose.yml`.

## Install with systemd (native binary)

Requires Go 1.26+ (see `go.mod`) to build, plus a Readest account and a
self-hosted BookOrbit server.

```sh
make build                                        # static binary -> bin/bridge
cp configs/bridge.example.yaml configs/bridge.yaml
# edit configs/bridge.yaml, or export BRIDGE_* env vars (secrets preferred)
./bin/bridge --config configs/bridge.yaml
```

Prefer `https://` for `bookorbit.server_url`; the `x-auth-key` header is the
MD5 of your password (a password-equivalent). See
`bookorbit.allow_insecure_transport` in `configs/bridge.example.yaml` for the
cleartext case.

The same KOReader credentials from steps 1-2 above go into `bookorbit.username`
and `bookorbit.password` (or `bookorbit.userkey`). For running as a service,
see `systemd/bridge.service` - its header comment has copy-paste install steps.
`bridge --once` runs a single pass (useful for a systemd timer or cron).

## Development

Requires Go 1.26+ (see `go.mod`).

```sh
make test      # unit tests
make vet       # go vet
make fmt       # gofmt -s
make release   # static linux/amd64 + linux/arm64 binaries into bin/
```

For protocols and internals, see `docs/reverse-engineering-report.md` and
`docs/reference-map.md`.

## License

AGPL 3: [[./LICENSE]]
