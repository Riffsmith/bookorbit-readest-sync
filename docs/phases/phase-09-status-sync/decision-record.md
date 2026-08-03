# Phase 9 — Decision Record

**Status:** Implemented exactly per the approved design
(`docs/phase-9-status-sync-design.md`). Scope was the four additive
touch-points named there — `internal/readest`, `internal/bookorbit`,
`internal/sync/state`, `internal/sync`, `internal/config` — plus the example
config and the README/ADR documentation. No package boundary changed
(`internal/readest` and `internal/bookorbit` still never import each other;
only `internal/sync` imports both). `go build ./...`, `go vet ./...`,
`gofmt -l .`, `go test ./...`, and `go test -race ./...` all pass, including
every newly added test.

Phase 9 is complete. The `on_hold` probe and the live `BRIDGE_SYNC_STATUS=true`
smoke run remain manual/live items, tracked alongside the other open live
checks in `docs/live-validation-status.md`; neither blocks this phase.

---

## Decisions implemented (A–F carried forward; G and H resolved here)

Decisions A–F were settled by `docs/status-sync-design-investigation.md` §6
against the BookOrbit server source and were carried into the design as given.
They are confirmed implemented exactly as answered:

| # | Decision | Implementation |
|---|----------|----------------|
| A | `unread` needs local bookkeeping even though it's a no-op push | **Implemented** — `MatchRecord` carries both pairs: `LastSeenStatus`/`LastSeenStatusAt` (last Readest value observed) and `LastPushedStatus`/`LastPushedStatusAt` (last BookOrbit token written). The `pushStatus` skip branch records the seen pair for recognized non-push values; the `unread→finished` transition diffs against it. |
| B | Channel B request/response shape | **Implemented** — `PUT /koreader/plugin/catalog/books/{bookId}/read-status`, body exactly `{"status":"<token>"}` with **no** device wrapper (the server DTO is single-field and `forbidNonWhitelisted: true` rejects extras with 400). Success `200 {"readStatus":"<token>"}` is decoded to guard the body-size bound; the echoed token is informational only (the persisted DB status can differ via the `rereading` projection at `reading-attempt.service.ts:114`, but that case is unreachable here since Readest `reading` is always skipped). |
| C | 404 classification on Channel B | **Implemented** — 404 → `bookorbit.ErrBookGone` ("stale cached bookId": book deleted or content-filtered), `DeleteMatch` + `SetUnmatched`, **no** watermark retreat. 405 remains `ErrUnsupportedEndpoint` (unreachable for this first-class route). |
| D | Unrecognized Readest status value | **Implemented** — log `WARN` once per distinct value (in-memory, process-lifetime dedup set `Engine.warnedStatusValues`), then skip. No bookkeeping update for an unrecognized value. |
| E | Status-push failure coupling | **Implemented** — decoupled. A status failure never appends to `failedWatermarks`, never touches `LastPushedPct`/`LastPushedAt`, and never advances `LastPushedStatus`; the next poll retries because the mapped value still differs. The progress watermark advances independently in the same pass. |
| F | Config-gated, default | **Implemented** — `Bridge.SyncStatus bool` / yaml `bridge.sync_status` / env `BRIDGE_SYNC_STATUS`, default `false` (opt-in). |

### Decision G — ship v1 with `on_hold` excluded → **G-1**

Implemented. `mapReadingStatus` maps only `finished` and `abandoned` to a push
token; every other value — including a future `on_hold` token Readest does not
currently emit (verified: no `on_hold` enum value exists in
`readingstatus.lua` / `librarystore.lua` / `library-design.md`) — returns
push=false and falls into the unrecognized-value path (Decision D):
warn-once, skip, no state corruption. Shipping cannot mis-sync an on-hold
book; it can only leave its BookOrbit status unchanged. The live probe
described in `docs/future-status-sync.md` §5 stays open as a manual item.

### Decision H — 404 sentinel mechanism → **Mech-α**

Implemented. A new exported sentinel `bookorbit.ErrBookGone` is classified
from HTTP 404 on the `SetReadStatus` call path *only*. `SetReadStatus`
intercepts `resp.StatusCode == http.StatusNotFound` first and returns
`ErrBookGone`, then delegates every other status (400/401/403/429/5xx/network)
to the unchanged shared `classifyErrorResponse`. The engine branches on
`errors.Is(err, bookorbit.ErrBookGone)` to drop the book from the sync set.
The shared classifier still maps 404 → `ErrUnsupportedEndpoint` for the other
four endpoints, so the Phase 6 bulk-fallback semantics are untouched.

### Config field shape — confirmed

`Bridge.SyncStatus bool` / `bridge.sync_status` / `BRIDGE_SYNC_STATUS`,
default `false`; no `Validate()` invariant (a bool has none) — carried out
exactly as the design's §3.0 approval gate specified.

---

## What changed, file by file

### `internal/readest/models.go`
- `BookRow` gained two fields storing the wire values raw (ISO strings, not
  pre-converted to ms — same convention as `UpdatedAt`/`DeletedAt`/`SyncedAt`):
  `ReadingStatus string` (`json:"reading_status"`) and
  `ReadingStatusUpdatedAt string` (`json:"reading_status_updated_at"`).
  No new methods — `mapReadingStatus` operates on the raw string, and the
  status timestamp is not used in watermark math (Decision E).

### `internal/readest/models_test.go`
- `TestBookRowReadingStatusFieldsDecode` — a row carrying both fields decodes
  them; `reading_status_updated_at` parses via the existing `util.ISOToMs`.
- `TestBookRowReadingStatusFieldsAbsentDecodeEmpty` — absent and `null`
  variants decode to `""` with no error (same tolerance discipline as the
  `null` progress tuple).

### `internal/bookorbit/models.go`
- `SetReadStatusRequest{Status string}` / `SetReadStatusResponse{ReadStatus
  string}`, both documented as carrying no device wrapper.

### `internal/bookorbit/client.go`
- New sentinel `ErrBookGone`, doc-commented as 404-on-`SetReadStatus`-only.
- `API` interface gained `SetReadStatus(ctx, bookID int64, status string)
  error` — additive, mirroring how `UpdateProgress` was added in Phase 5.
- `Client.SetReadStatus` reuses `newRequest`/`do`/`withTimeout`/
  `encodeAndCheckSize` exactly as the existing four methods do; it intercepts
  404 → `ErrBookGone` (via the small `classifyBookGone` helper) before the
  shared classifier, and decodes the success body through `readBoundedBody` to
  keep the size bound and surface malformed-body errors.

### `internal/bookorbit/models_test.go` / `client_test.go`
- `TestSetReadStatusRequestWireBody` (model) pins the exact body
  `{"status":"read"}` and asserts the device-wrapper keys are absent.
- `TestSetReadStatusResponseDecodes` pins the echo-body decode.
- Client tests: success, request shape (method/path/headers/exact body), echo
  decode, the full status matrix — 404→`ErrBookGone` (**not**
  `ErrUnsupportedEndpoint`), 400→`ErrBadRequest`, 401/403→`ErrUnauthorized`,
  429→`ErrRateLimited`, 5xx→`ErrServer` — network error→`ErrNetwork`,
  malformed body→`ErrMalformedResponse`, and context cancellation propagating
  untouched.

### `internal/sync/state/state.go`
- `MatchRecord` gained the four Phase 9 fields (`LastSeenStatus`,
  `LastSeenStatusAt`, `LastPushedStatus`, `LastPushedStatusAt`), slotted next
  to `LastPushedAt`/`LastPushedPct` with no restructuring and no interface
  change (`Store.SetMatch`/`Match` already operate on the whole value).

### `internal/sync/state/state_test.go`
- `exerciseStore` (the helper whose design purpose is absorbing additive
  `MatchRecord` changes against both `MemStore` and `FileStore`) now
  round-trips the four new fields.
- `TestFileStoreRoundTrip` additionally asserts the seen/pushed status pairs
  survive a reload from disk.
- `TestFileStoreAtomicityLeavesValidFile` re-run unmodified with the larger
  struct; it still passes.

### `internal/sync/engine.go`
- `mapReadingStatus(readestStatus) (token, push)` — the pure mapper:
  `finished→read`, `abandoned→abandoned`, everything else `(, false)`.
- `classifyStatusValue` and the `statusValueKind` enum split a non-push value
  into "record the seen bookkeeping" (known: `""`/`reading`/`unread`) vs.
  "warn once, record nothing" (unrecognized).
- `Engine` gained `warnedStatusValues map[string]bool` (initialized in
  `NewEngine`), the per-process warn-once dedup set (Decision D).
- `RunOnce` inserts the status-push phase after the push-progress phase,
  gated by `if e.cfg.Bridge.SyncStatus`, aborting on BookOrbit auth failure
  exactly like the other phases.
- `pushStatuses` / `pushStatus` / `recordSeenStatus` implement the per-book
  loop: skip dummy/deleted/unusable-progress/unmatched rows; skip non-push
  values (recording seen bookkeeping for the known ones); skip unchanged;
  push changed decisive tokens; on `ErrBookGone` drop the book (no watermark
  interaction); on `ErrUnauthorized` abort; on other errors skip the book
  without advancing `LastPushedStatus`.
- `classifyBookOrbitStatusErr` — the status-specific classifier
  (`ErrUnauthorized`→fatal, `ErrBookGone`/`ErrBadRequest`→skip,
  rate-limit/server/network→retry, context→fatal). `ErrBookGone` is branched
  on in `pushStatus` directly, so the classifier's skip verdict for it is
  defense-in-depth only.
- `pushReadStatus` — the `withRetry` gateway mirroring the other four.
- `pushChunk` / `pushSingle` progress-write `SetMatch` calls now **preserve**
  the status bookkeeping on `p.rec` instead of rebuilding a bare
  `MatchRecord`, so a progress push cannot reset the status baseline. (See
  "Deviations found in testing" below.)

### `internal/sync/engine_test.go`
- `statusCall` recording on `fakeBookOrbit`, plus per-book-ID scripted errors
  (`statusErrs`) and a `SetReadStatus` no-op stub on `scriptedBookOrbit` so
  both fakes still satisfy the widened `bookorbit.API`.
- `TestMapReadingStatus` table test over all six named cases.
- The full engine matrix from the design §5 inventory: gate-off; fresh
  finished/abandoned push; unchanged skip; non-decisive skip; `unread` records
  seen but pushes nothing; `unread→finished` transition; unmatched book
  skipped; `ErrBookGone` drops the book with the progress watermark
  unaffected; retryable failure decoupled from progress (then retried next
  poll); `ErrUnauthorized` aborts with prior mutations preserved and `Save()`
  still once; mixed multi-book batch (push + skip + drop + no-op); `Save()`
  still exactly once with the status step present; unrecognized value warns
  once then is silent (captured via a `slog.TextHandler` on an in-memory
  buffer) and records nothing.

### `internal/config/types.go` / `defaults.go` / `env.go` / `load.go` / `validate.go`
- `BridgeConfig.SyncStatus bool` (`yaml:"sync_status"`).
- `Default()` sets `SyncStatus: false`.
- `env.go` gained the first bool-env helper, `setBool` (a direct extension of
  the `setStr`/`lookupNonEmpty` shape), plus the `EnvSyncStatus` constant;
  applied in `applyEnv`.
- `load.go` `applyValues` gained `case "bridge.sync_status":` parsing via
  `strconv.ParseBool` with the existing error-wrapping convention.
- `validate.go` gained a one-line comment noting the bool has intentionally
  nothing to validate.

### `internal/config/config_test.go`
- Default-false, YAML-sets-it, env-overrides-file, invalid-env-value-ignored,
  and invalid-file-value-rejected tests.

### `configs/bridge.example.yaml`
- Documented `bridge.sync_status` (commented, default `false`) with a one-line
  rationale, and added `BRIDGE_SYNC_STATUS` to the env-var comment header.

### `README.md`
- Status banner "Phase 8 complete" → "Phase 9 complete."
- "What it does" gains the status-sync bullet and drops "status sync" from the
  excluded list.
- New "Status sync (optional)" subsection: the opt-in flag, the mapping table,
  the deliberate `reading`/clear-status/`unread` no-ops, the documented
  `on_hold` deferral, and the progress-status decoupling.
- Roadmap updated to "Phases 0–9 complete."

---

## Deviations found in testing (one, corrected during implementation)

The design's §4 non-goal list and §3 plan did not call out an interaction the
existing Phase 6 code had with the new fields: `pushChunk` and `pushSingle`
recorded a successful progress push by constructing a **fresh**
`state.MatchRecord` literal carrying only the progress fields. Left as-is,
that write would zero `LastSeenStatus*` / `LastPushedStatus*` on every
progress push, so the status "unchanged" skip (which diffs the mapped token
against `LastPushedStatus`) would never observe a prior status push and would
re-push every poll. This was caught by `TestRunOnceStatusUnchangedSkips` /
`TestRunOnceStatusMixedBatch` failing on the first engine-test run.

The fix keeps the existing record (`p.rec`, which already carries whatever the
status step wrote) and updates only the two progress fields — the minimal,
behavior-preserving correction the design's intent ("status rides on the match
the progress phase produced") already implies. It is not an architecture or
decision change; it is the existing progress-write path made field-preserving
now that `MatchRecord` carries more than progress. All tests pass with it.

No other deviation from the design exists. Design §3.6's live smoke test and
the `on_hold` probe (§2 / Decision G) are manual/live items, unchanged from
how Phases 3–8 handled their live-validation counterparts.

---

## Verification performed

```
$ go build ./...        # clean
$ go vet ./...           # clean
$ gofmt -l .             # no files reported
$ go test ./...          # all packages green
$ go test -race ./...    # all packages green
```

Newly added tests were also run individually with `-run ... -v` (the five
`SyncStatus` config tests and the `internal/sync` status matrix) to confirm
every subtest name and pass status. No existing test regressed.

`docs/future-status-sync.md` and `docs/status-sync-design-investigation.md`
are left as-is, per the project's established convention (design/investigation
docs are historical records; the ADR is where the as-built account lives —
the same pattern every prior phase followed).
