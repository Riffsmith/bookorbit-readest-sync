# Design Investigation Plan: Status Sync (Phase 9)

**Status:** design investigation plan — no production code in this document.
**Scope:** one-way Readest → BookOrbit reading-status sync
(`finished`/`abandoned`/`unread`/`reading`), extending the already-shipped
progress bridge. Two-way sync, Channel A (`book-states`), and `on_hold` are
explicitly out of scope for v1 (see §4).

**This document does not re-derive anything.** `docs/future-status-sync.md`
did the reference-source investigation. `docs/status-sync-design-investigation.md`
§6 answered Decisions A–F against the live BookOrbit server source
(cross-referenced in `docs/reverse-engineering-report.md` §9). Those six
decisions are **closed** and are not reopened here. This document's only job
is to (1) confirm there is nothing architecturally new to decide beyond what
§6 already settled, (2) name the two things that are genuinely still open
(§2), and (3) turn the settled design into a checkable implementation plan
that follows the project's existing phase pattern (design → approval →
implementation → verification, per `docs/implementation-brief.md`).

---

## 1. What's already settled (no re-approval needed)

Pulled from `docs/status-sync-design-investigation.md` §6.2–§6.7, each
answer anchored to BookOrbit server source:

| #   | Decision                                                            | Answer                                                                                                                                                                                                                                                                                                                                                                                        | Source anchor                                                                                                                  |
| --- | ------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| A   | Does `unread` need local bookkeeping even though it's a no-op push? | Yes — `MatchRecord` gets **two** field pairs, not one: `LastSeenStatus`/`LastSeenStatusAt` (last Readest value observed) and `LastPushedStatus`/`LastPushedStatusAt` (last BookOrbit token written). Without the "seen" pair, an `unread → finished` transition has nothing correct to diff against.                                                                                          | `internal/sync/state/state.go:23-28` (existing `MatchRecord` shape to extend)                                                  |
| B   | Request/response shape for Channel B?                               | `PUT /koreader/plugin/catalog/books/{bookId}/read-status`, body **exactly** `{"status": "<token>"}` — no device wrapper. Success is `200 {"readStatus": "<token>"}` (echoes the request, not a projection).                                                                                                                                                                                   | `koreader-catalog-query.dto.ts:245-248`, `main.ts:64-70` (`forbidNonWhitelisted: true`), `koreader-catalog.service.ts:275-279` |
| C   | How to classify 404/405?                                            | 404 = **stale cached `bookId`** (book deleted from user's library, or access revoked) — not "endpoint unsupported." Treat as "drop this book from the status-sync set": `DeleteMatch` + `SetUnmatched`, **no** watermark retreat. 405 is unreachable (Channel B is a first-class route, no legacy-server fallback exists).                                                                    | `book.service.ts:472-481` (404 source), `koreader-catalog.controller.ts:24,81-84` (first-class route)                          |
| D   | Unrecognized Readest status value?                                  | Log `WARN` once per distinct value (in-memory, process-lifetime dedup set on `Engine`), then skip. Do not update `LastSeenStatus`/`LastPushedStatus` for an unrecognized value.                                                                                                                                                                                                               | N/A — Readest-side schema-drift concern, not server-verifiable                                                                 |
| E   | Does a status-push failure retreat the progress watermark?          | **No — decoupled.** Status and progress are different tables/write paths server-side (`reading_progress` vs. `user_book_status`), so a status failure never touches the progress watermark or `LastPushedPct`. A status failure just skips advancing `LastPushedStatus` for that book this poll; it's picked up again next poll since the mapped value still differs from `LastPushedStatus`. | `koreader.repository.ts:536-562` vs. `reading-attempt.repository.ts:85-111`                                                    |
| F   | Config-gated, default on or off?                                    | Default **off**, opt-in. `docs/implementation-brief.md` states "No status sync" as the current scope; a status write changes what the operator's BookOrbit catalog displays, unlike progress (purely informational), so silent opt-in is the wrong default.                                                                                                                                   | `docs/implementation-brief.md` (existing scope constraint)                                                                     |

None of A–F require sign-off in this document — they're carried forward as
given. If anyone wants to reopen one, that's a new decision, not a
continuation of this plan.

---

## 2. What's still genuinely open (needs your approval before Phase 9 coding starts)

Everything else in `docs/status-sync-design-investigation.md` is either
settled (§1) or an implementation detail with a stated recommendation. Only
two things actually need a decision from you:

### Decision G — Ship v1 with `on_hold` excluded, or hold the whole feature until the `on_hold` probe runs?

`docs/future-status-sync.md` §1.1 confirms: **no `on_hold` token exists
anywhere in the Readest plugin source, schema, or tests** (verified by grep
across the whole reference tree). Whatever the Readest web UI's "Mark on
hold" button does is not represented in the API contract the bridge reads.
This is unlike Decisions A–F — it cannot be resolved by reading BookOrbit's
source, because the missing piece is entirely on the Readest side, and this
project doesn't have write access to (or a spec for) Readest's web client.

Two options:

- **(G-1, recommended) Ship now, `on_hold` deferred.** Implement `finished`,
  `abandoned`, `unread` (no-op), `reading` (skip) — the three-plus-one
  mappings that already have full source evidence. Document `on_hold` as an
  explicit, permanent no-op (falls into the "unrecognized value" path, §1
  Decision D) until a live probe (`GET /sync?type=books&since=0` after
  triggering "Mark on hold" in the Readest UI — the exact procedure
  `docs/future-status-sync.md` §5 already describes) reveals what token, if
  any, Readest actually writes. This matches how progress sync itself
  shipped — `docs/adr/phase-6-decision-record.md`'s Addenda show the project
  already has a working pattern of "ship the verified part, live-probe the
  rest, patch later."
- **(G-2) Block the whole feature on the probe first.** Nothing ships until
  the on_hold question is answered. Slower, and the probe itself doesn't
  need code to run — it's a manual `--once` + `GET` call against a live
  account — so gating implementation on it doesn't obviously buy anything.

**Recommendation: G-1.** The probe can run in parallel with or even before
implementation (it needs zero bridge code — it's a raw API call against the
operator's existing Readest account), and there's no reason the three
source-verified mappings should wait on an unrelated fourth one.

### Decision H — Sentinel mechanism for the 404 "stale `bookId`" case

`docs/status-sync-design-investigation.md` §6.4 names two equivalent
mechanisms and recommends one without treating it as settled:

- **(Mech-α, recommended)** Add a new `bookorbit.ErrBookGone` sentinel,
  classified from HTTP 404 **only on the `SetReadStatus` call path**. The
  engine does `errors.Is(err, bookorbit.ErrBookGone)` and drops the book from
  the sync set.
- **(Mech-β)** Reuse the existing `bookorbit.ErrBadRequest`/`ErrUnsupportedEndpoint`
  taxonomy for both 400 and 404 on this endpoint, and let the engine's status
  step run its own local `classifyStatusPushErr(err)` helper to decide
  `drop` vs. `retry` vs. `fatal` vs. `skip`.

This is a real API-surface choice (Mech-α adds a new exported sentinel to
`internal/bookorbit`; Mech-β keeps the sentinel set unchanged but pushes the
404-vs-400 distinction into engine-only logic), so it's worth a explicit
yes/no rather than silently picking one mid-implementation.

**Recommendation: Mech-α.** It's consistent with the existing taxonomy
(`ErrUnsupportedEndpoint` already exists for exactly this "give the engine
something to `errors.Is` against" reason — Phase 5 design §10). The
classifier in `bookorbit/client.go` (`classifyErrorResponse`) is currently
shared across all four methods and maps 404/405 → `ErrUnsupportedEndpoint`
uniformly; Mech-α means `SetReadStatus` needs its **own** narrower
classification for 404 specifically (since 404 means something different
on this endpoint than on the others — "book gone," not "route missing").
That's a small, contained deviation from the shared classifier, not a
rewrite of it.

**Everything else in this plan assumes G-1 and Mech-α.** If you'd rather
have G-2 or Mech-β, say so before I start on §3.

---

## 3. Implementation plan

Each package gets one focused change, following the same layering the
project has used since Phase 4: models → client/data → state → engine →
config → docs. No package boundary changes (`readest` still doesn't know
`bookorbit` exists; only `sync` imports both — unchanged).

### 3.0 Approval gate

- [ ] Confirm Decision G (recommend: G-1, ship now, `on_hold` deferred)
- [ ] Confirm Decision H (recommend: Mech-α, new `bookorbit.ErrBookGone` sentinel)
- [ ] Confirm config field name/shape: `Bridge.SyncStatus bool`, yaml key
      `bridge.sync_status`, env var `BRIDGE_SYNC_STATUS`, default `false`

### 3.1 `internal/readest` — two new `BookRow` fields

- [ ] Add `ReadingStatus string` (json tag `reading_status`) to `BookRow`
- [ ] Add `ReadingStatusUpdatedAt string` (json tag `reading_status_updated_at`,
      ISO-8601 string, same convention as `UpdatedAt`/`DeletedAt`/`SyncedAt` —
      **not** pre-converted to ms in the model, matching how the other
      timestamp fields are stored raw and converted on demand via
      `util.ISOToMs`)
- [ ] No new methods needed on `BookRow` for v1 — `mapReadingStatus` (§3.4)
      operates on the raw string; a `ReadingStatusUpdatedMs()` helper is not
      required because Decision E means status pushes don't participate in
      watermark math at all
- [ ] `models_test.go`: extend `TestBooksResponseDecoding`-style fixture
      coverage — a row with `reading_status`/`reading_status_updated_at` set
      decodes correctly; a row with the fields absent/null decodes to `""`
      (tolerance pattern already established for `ProgressTuple`)

**Why no client change:** `docs/future-status-sync.md` §1.1 confirms both
fields already arrive on the existing `GET /sync?type=books` response
(`syncbooks.lua:168-169`). `readest.Client.PullBooks` needs zero changes —
this is purely two new struct fields decoded from a response the client
already fetches.

### 3.2 `internal/bookorbit` — one new client method, one new sentinel

- [ ] `models.go`: add
      ``go
    type SetReadStatusRequest struct {
        Status string `json:"status"`
    }
    type SetReadStatusResponse struct {
        ReadStatus string `json:"readStatus"`
    }
    ``
      No device wrapper (per Decision B — the DTO on the server rejects
      extra fields via `forbidNonWhitelisted: true`).
- [ ] `client.go`: add `ErrBookGone = errors.New("bookorbit: book not found or access revoked")`
      to the sentinel block, doc-commented as 404-on-`SetReadStatus`-only,
      distinct from `ErrUnsupportedEndpoint` (which stays 404/405 elsewhere).
- [ ] `client.go`: add
      `go
    func (c *Client) SetReadStatus(ctx context.Context, bookID int64, status string) error
    `
      `PUT /koreader/plugin/catalog/books/{bookID}/read-status`. Reuses
      `newRequest`/`do`/`withTimeout`/`encodeAndCheckSize` exactly as the
      existing four methods do — no new transport plumbing.
- [ ] `client.go`: the method needs its **own** small status-classification
      step before falling through to the shared `classifyErrorResponse`,
      specifically to redirect 404 to `ErrBookGone` instead of the shared
      classifier's `ErrUnsupportedEndpoint`. Concretely: check
      `resp.StatusCode == http.StatusNotFound` first and return `ErrBookGone`
      wrapped with the same status+snippet shape every other error already
      uses; otherwise delegate to `classifyErrorResponse` unchanged (400→
      `ErrBadRequest`, 401/403→`ErrUnauthorized`, 429→`ErrRateLimited`,
      5xx→`ErrServer`, network→`ErrNetwork` — all reused as-is).
- [ ] `API` interface: add `SetReadStatus(ctx context.Context, bookID int64, status string) error`
      — additive, mirrors how `UpdateProgress` was added in Phase 5 without
      touching the other three methods' signatures.
- [ ] `client_test.go`: request shape (method=PUT, path
      `/koreader/plugin/catalog/books/{id}/read-status`, body is exactly
      `{"status":"..."}`, **no** `deviceId`/`pluginVersion`/`deviceTime`
      keys present — assert their absence the same way
      `TestUpdateProgressRequestShape` already asserts absence of the
      device-wrapper keys on that endpoint); response decode
      (`{"readStatus":"read"}` → `SetReadStatusResponse{ReadStatus:"read"}`);
      error matrix: 400→`ErrBadRequest`, 401/403→`ErrUnauthorized`,
      404→`ErrBookGone` (**not** `ErrUnsupportedEndpoint` — this is the
      pinning test for the Mech-α deviation), 429→`ErrRateLimited`,
      5xx→`ErrServer`; network error→`ErrNetwork`; context cancellation
      propagates untouched (same pattern as the other three methods).

### 3.3 `internal/sync/state` — four new `MatchRecord` fields

- [ ] Add to `MatchRecord` (`internal/sync/state/state.go`):
      ``go
    LastSeenStatus     string `json:"lastSeenStatus"`
    LastSeenStatusAt   int64  `json:"lastSeenStatusAt"`
    LastPushedStatus   string `json:"lastPushedStatus"`
    LastPushedStatusAt int64  `json:"lastPushedStatusAt"`
    ``
      Slots in next to the existing `LastPushedAt`/`LastPushedPct` — no
      restructuring, no interface change (`Store.SetMatch`/`Match` already
      operate on the whole `MatchRecord` value).
- [ ] `state_test.go`: extend the existing `exerciseStore` helper (not a new
      test function — that helper's whole design purpose, confirmed in its
      own doc comment, is absorbing exactly this kind of additive field
      change against both `MemStore` and `FileStore` in one place) to
      round-trip the four new fields.
- [ ] Confirm `FileStore`'s atomic-write-leaves-valid-JSON test
      (`TestFileStoreAtomicityLeavesValidFile`) still passes unmodified with
      the larger struct — no test change needed there, just re-run.

### 3.4 `internal/sync` — pure mapper + engine step, gated by config

- [ ] New unexported pure function in `engine.go` (or a new `status.go` in
      the same package if `engine.go` gets unwieldy — small enough that a
      new file isn't obviously warranted, judge at implementation time):
      `go
    func mapReadingStatus(readestStatus string) (token string, push bool)
    `
      `"finished"`→`("read", true)`; `"abandoned"`→`("abandoned", true)`;
      `"unread"`→`("", false)`; `""`/`"reading"`→`("", false)`; anything else
      (including a future `on_hold` token, per Decision G)→`("", false)`.
- [ ] `engine_test.go`: table test over all named cases plus one
      unrecognized-value case — mirrors the existing `TestComputeWatermark`
      table-test shape exactly.
- [ ] `Engine` struct: add one unexported field for the per-process
      unrecognized-status-value dedup set (Decision D), e.g.
      `warnedStatusValues map[string]bool`, initialized in `NewEngine`.
- [ ] `RunOnce`: insert a status step after the existing push-progress phase,
      gated by `if e.cfg.Bridge.SyncStatus { ... }`. For every row with a
      resolved `MatchRecord` (i.e., already matched — status push never
      triggers its own match-check; it rides on whatever progress's
      match-check phase already resolved this poll or in a prior one): - compute `(token, push) := mapReadingStatus(row.ReadingStatus)` - if `!push`: if the raw value is non-empty, non-decisive-but-known
      (`"reading"`) → skip silently; if unrecognized → warn-once (Decision
      D) and skip; either way, still update `LastSeenStatus`/`LastSeenStatusAt`
      so a later transition is detected correctly (Decision A) — **except**
      for the unrecognized-value case, which must NOT update bookkeeping
      (per §3.5 of the investigation doc: "a token we don't understand is
      not a status we can claim to have processed") - if `push` and `token == rec.LastPushedStatus`: no-op (already
      current) - if `push` and `token != rec.LastPushedStatus`: call `SetReadStatus`
      via the existing `withRetry` helper with a new
      `classifyBookOrbitStatusErr` classifier (see below)
- [ ] New classifier `classifyBookOrbitStatusErr(err) outcome` (distinct
      from `classifyBookOrbitErr`, living alongside it in `engine.go`,
      because `ErrBookGone` needs its own verdict that the existing
      progress-push classifier doesn't have): - `ErrBookGone` → **not** `outcomeFatal`/`outcomeRetry`/`outcomeSkip`
      in the withRetry sense — this needs its own handling path in the
      status step (drop-from-sync-set), not a generic retry verdict. Two
      implementation options: (a) let `classifyBookOrbitStatusErr` return
      a new `outcomeDrop` value threaded through a status-specific small
      retry wrapper, or (b) skip `withRetry` for `ErrBookGone` entirely
      and check `errors.Is(err, bookorbit.ErrBookGone)` directly on the
      raw `SetReadStatus` call before deciding whether to route through
      retry at all. **Recommend (b)** — `ErrBookGone` is definitionally
      non-retryable and needs an immediate, unambiguous branch, and
      overloading the existing `outcome` enum with a `Drop` case that only
      one caller ever produces adds a new enum value's worth of complexity
      to a shared type (`outcome` is currently shared by
      `classifyReadestErr`/`classifyBookOrbitErr`) for a one-call-site
      need. - `ErrUnauthorized` → `outcomeFatal` (same as progress push — aborts
      `RunOnce`) - `ErrBadRequest` → `outcomeSkip` (log `WARN`, do not advance
      `LastPushedStatus`, do not retreat progress watermark per Decision E) - `ErrRateLimited`/`ErrServer`/`ErrNetwork` → `outcomeRetry` (existing
      `withRetry` backoff applies; on exhaustion, same as `ErrBadRequest`:
      skip, no watermark interaction) - `context.Canceled`/`context.DeadlineExceeded` → `outcomeFatal`
      (unchanged convention)
- [ ] On `ErrBookGone`: `state.DeleteMatch(hash)` + `state.SetUnmatched(hash, now)`
      — identical call shape to the existing absent-from-match-check path
      (Phase 6 ADR Addendum 2), **explicitly not** appended to
      `failedWatermarks` (Decision E: status failures never touch the
      progress watermark).
- [ ] On success: update all four new `MatchRecord` fields
      (`LastSeenStatus`, `LastSeenStatusAt`, `LastPushedStatus`,
      `LastPushedStatusAt`) via `state.SetMatch`.
- [ ] `engine_test.go`: the §3.5 matrix from
      `docs/status-sync-design-investigation.md`, finalized (no longer
      "pending Decision E" placeholders): - matched book, `finished`, never pushed → one `SetReadStatus("read")`
      call, state updated - matched book, `finished`, already pushed `"read"` → zero calls - matched book, `reading`/`""` → zero calls, regardless of
      `LastPushedStatus` - matched book, `unread` → zero calls, but `LastSeenStatus` updates
      (regression guard for the "later transition detected correctly"
      property) - unmatched book (no `MatchRecord`/`BookID` yet) → status step
      skipped entirely for that book, no panic - `SetReadStatus` returns `ErrBookGone` → `DeleteMatch`+`SetUnmatched`
      fire, **progress watermark for that poll is unaffected** - `SetReadStatus` returns a retryable error, retries exhausted →
      `LastPushedStatus` NOT advanced, progress watermark/state for that
      book's _progress_ push (if any, in the same poll) still advances
      normally — this is the direct pin for Decision E's decoupling - `SetReadStatus` returns `ErrUnauthorized` → `RunOnce` aborts, prior
      committed mutations preserved, `Save()` still runs once (mirrors
      every existing auth-abort test) - mixed multi-book batch: one push, one skip (unchanged), one drop
      (`ErrBookGone`), one non-decisive-skip, in the same pass - unrecognized status value: first occurrence → one `WARN`; second
      occurrence of the same value → zero further log lines (capture via
      a `slog` test handler, same technique already available) - `Engine.cfg.Bridge.SyncStatus == false` → the status step never
      runs at all, zero `SetReadStatus` calls, regardless of any row's
      `ReadingStatus` — the gate test - `Save()` is still called exactly once per `RunOnce` with the new
      step present (extend the existing counting-store test)

### 3.5 `internal/config` — the opt-in toggle

- [ ] `internal/config/types.go`: add `SyncStatus bool` to `BridgeConfig`,
      yaml tag `sync_status`
- [ ] `internal/config/defaults.go`: `SyncStatus: false` in `Default()`
- [ ] `internal/config/env.go`: **new pattern needed** — `env.go` currently
      only has a string setter (`setStr`) and an inline duration parse; there
      is no existing bool-env helper. Add
      `go
    func setBool(dst *bool, name string) {
        if v, ok := lookupNonEmpty(name); ok {
            if b, err := strconv.ParseBool(v); err == nil {
                *dst = b
            }
        }
    }
    `
      and a new constant `EnvSyncStatus = "BRIDGE_SYNC_STATUS"`. This is the
      one genuinely new piece of plumbing in this whole plan — everything
      else reuses an existing pattern verbatim. Flagging it so it isn't
      missed in review, not because it needs approval (it's a direct,
      obvious extension of the existing `setStr`/`lookupNonEmpty` shape).
- [ ] `internal/config/load.go` (`applyValues`): add `case "bridge.sync_status":`
      parsing via `strconv.ParseBool`, matching the existing
      `bridge.match_batch_size`-style case error-wrapping.
- [ ] `internal/config/validate.go`: no-op case — a bool has no invariant to
      check, matching how other boolean-shaped settings would be handled if
      any existed today (none currently do, so this is also the first bool
      config field; worth a one-line comment in `Validate()` noting there's
      intentionally nothing to validate).
- [ ] `config_test.go`: env-override test (`TestEnvOverridesFile`-style) and
      a YAML-file-sets-it test, following `TestLoadFromFileAndNormalize`'s
      existing shape.
- [ ] `configs/bridge.example.yaml`: document the new key under the
      `bridge:` section, commented out, default `false`, one-line
      description ("Push Readest reading-status changes — finished/abandoned
      — to BookOrbit. Off by default because, unlike progress, this edits
      what your BookOrbit catalog displays.")

### 3.6 Documentation

- [ ] `docs/adr/phase-9-decision-record.md` — the as-implemented record,
      matching the style of Phases 5–8's ADRs: file-by-file summary,
      confirmation that Decisions A–F were implemented exactly as answered
      in §6, explicit record of how G and H were resolved, verification
      output (`go build`/`vet`/`gofmt`/`test`/`test -race`).
- [ ] `README.md`: add a short "Status sync (optional)" subsection
      describing the opt-in flag, the three supported mappings, and the
      explicit `on_hold` deferral — mirroring the existing "BookOrbit
      authentication" section's tone (plain, operator-facing, not a design
      doc).
- [ ] `docs/future-status-sync.md` / `docs/status-sync-design-investigation.md`:
      leave as-is, per the project's established convention (design docs are
      historical records; the ADR is where "as-built" lives — same pattern
      every prior phase followed).

### 3.7 Verification

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `gofmt -l .`
- [ ] `go test ./...`
- [ ] `go test -race ./...`
- [ ] Manual smoke test against a real account, `BRIDGE_SYNC_STATUS=true`,
      following the same template `live-test-reports.md` already uses
      (state purpose → setup steps → expected log/state-file assertions →
      paste real output → immediate second run for idempotency). Not part
      of this repo's automated suite — same convention as every other live
      check in `docs/live-validation-status.md`.

---

## 4. Explicit non-goals (carried forward, not reopened)

- Two-way status sync (BookOrbit → Readest) — no source-of-truth signal
  exists for a reverse write; out of scope per `docs/problem-statement-prompt.md:24`.
- Channel A (`POST /koreader/plugin/book-states`) — wrong enum
  (`reading|complete|abandoned`, not BookOrbit-native tokens); Channel B is
  the only endpoint this plan touches.
- `POST /koreader/plugin/sweeps` (sweep-complete notification) — already
  excluded in Phase 5 design §6; irrelevant to a headless bridge.
- Full-library recheck / `libraryVersion` gating for status — same reasoning
  as Phase 6 Decision D for progress: the existing `UnmatchedCooldown`
  mechanism is the only recheck gate; no new schema surface for this.
- `want_to_read`/`rereading`/`skimmed` mappings — no Readest-side
  source-of-truth value exists to drive a push; nothing to sync.

---

## 5. Edge cases and test inventory (consolidated from §3, for quick review)

**`internal/bookorbit`**

- 404 on `SetReadStatus` → `ErrBookGone`, not `ErrUnsupportedEndpoint` (the
  one behavioral deviation from the shared classifier — needs its own test)
- Request body is exactly `{"status":"..."}`, no device-wrapper keys
- Response decodes `{"readStatus":"..."}`
- Full status matrix (400/401/403/404/429/5xx/network/cancellation)

**`internal/readest`**

- `reading_status`/`reading_status_updated_at` present → decodes correctly
- Both absent/null → decodes to `""` without error (tolerance, not failure)

**`internal/sync/state`**

- Four new fields round-trip through both `MemStore` and `FileStore`
- `FileStore` atomicity/permissions guarantees hold with the larger struct

**`internal/sync` (engine)**

- `mapReadingStatus` table test (finished/abandoned/unread/reading/empty/unrecognized)
- Gate: `SyncStatus=false` → zero `SetReadStatus` calls ever
- Fresh decisive push (finished/abandoned)
- Unchanged status → no re-push
- Non-decisive (`reading`, `""`) → no-op, no bookkeeping change
- `unread` → no-op push, but `LastSeenStatus` still updates
- Unmatched book → status step skipped, no panic on zero `BookID`
- `ErrBookGone` → drop-from-sync-set, watermark **unaffected**
- Retryable failure, exhausted → `LastPushedStatus` not advanced, progress
  watermark for the same book's _progress_ push (if any) still advances —
  the decoupling pin (Decision E)
- `ErrUnauthorized` → abort `RunOnce`, prior mutations preserved, `Save()`
  still runs once
- Unrecognized value → warn once, silent thereafter; no bookkeeping update
- Mixed multi-book batch composes correctly (push + skip + drop + no-op in
  one pass)
- `Save()` still exactly once per `RunOnce` with the new step present

**`internal/config`**

- `sync_status` YAML key parses
- `BRIDGE_SYNC_STATUS` env override parses and wins over file
- Default is `false` when neither is set

**Manual/live (post-implementation, not blocking merge)**

- Real "Mark as finished" → BookOrbit shows `read`, idempotent on re-run
- Real "Mark as Unread" → zero `SetReadStatus` calls, BookOrbit untouched
- Real "Mark as abandoned" → token-identical round-trip
- The `on_hold` probe (§2, Decision G) — informational only, does not gate
  this phase's merge

---

## 6. Summary — what I need from you before starting §3

1. **Decision G**: ship v1 now with `on_hold` excluded (recommended), or
   hold the feature until the probe runs?
2. **Decision H**: add `bookorbit.ErrBookGone` as a new sentinel
   (recommended), or classify 404 via engine-side logic instead?
3. Confirm the config field shape (`Bridge.SyncStatus bool` /
   `bridge.sync_status` / `BRIDGE_SYNC_STATUS`, default `false`) — this one
   has no real alternative given the project's existing config conventions,
   flagged only for completeness.

Everything in §3 is otherwise ready to implement once G and H are confirmed.
Stopping here, as this repo's own convention requires, before any code is
written.

Decision G — Ship v1 with on_hold excluded, or hold for the probe?

My recommendation: G-1 — ship now, on_hold deferred. Approve as recommended.
Reasoning specific to this codebase, not generic:

- The probe requires zero bridge code (docs/future-status-sync.md §5): it's a manual GET /sync?type=books&since=0 after clicking "Mark on hold" in the Readest UI. Gating the three source-verified mappings (finished→read, abandoned→abandoned, unread no-op) on an unrelated fourth one buys nothing.
- The design already routes a future on_hold token into the existing "unrecognized value" path (Decision D): warn-once, skip, no bookkeeping update. So shipping G-1 fails safe, not silently — if Readest ever starts emitting a real on_hold-ish token, the bridge logs it rather than corrupting state.
- There is a safety margin worth naming: the current Readest plugin writes no on_hold token anywhere (grep-verified across the reference tree). So today, clicking "Mark on hold" either writes nothing the bridge reads, or writes a token we've never seen and skip. Neither case can accidentally push read/abandoned for a book the operator put on hold. G-1 cannot mis-sync an on-hold book; it can only leave its BookOrbit status unchanged. That's the correct conservative failure mode.
  If you want, I can fold a one-line note into the on_hold no-op path ("probe pending, see docs/future-status-sync.md §5") so a future reader doesn't re-litigate it — but that's a comment, not behavior.

Decision H — bookorbit.ErrBookGone sentinel, or engine-side classification?
My recommendation: Mech-α — add bookorbit.ErrBookGone. Approve as recommended.
This is the one decision with a real API-surface consequence, so here's the concrete reasoning:

- 404 on Channel B means "stale cached bookId" (book deleted / access revoked), which is semantically different from 404 on the progress endpoints ("endpoint unsupported"). The existing shared classifier (client.go:396) maps all 404s to ErrUnsupportedEndpoint, which would be wrong here — it would trigger the Phase 6 "soft-disable the whole feature" logic instead of "drop this one book."
- The taxonomy already has the exact precedent: ErrUnsupportedEndpoint exists specifically so the engine can errors.Is against it (Phase 5 §10). ErrBookGone is the same pattern, one level over. Consistent, greppable, and self-documenting.
- Mech-β pushes the 400-vs-404 distinction into engine-only logic, which means the engine would need to know that "on this endpoint, ErrBadRequest might actually mean book-gone" — a context-dependent reinterpretation of a shared sentinel. That's exactly the kind of implicit coupling this project's layering (models → client → engine) is designed to avoid.
  The contained deviation the design describes is correct and minimal: SetReadStatus checks resp.StatusCode == http.StatusNotFound first and returns ErrBookGone, then delegates everything else (400/401/403/429/5xx/network) to the unchanged shared classifyErrorResponse. No rewrite of the classifier; one endpoint-specific branch.

Config field shape — confirmed, nothing to decide
Bridge.SyncStatus bool / bridge.sync_status / BRIDGE_SYNC_STATUS, default false — confirmed correct. It follows the existing config.Bridge conventions exactly, no cross-field invariants for Validate(), and it's the first boolean config field (the design correctly flags that config/env.go needs a new setBool helper, since only setStr exists today). This one is settled; no approval needed.

Carry these two corrections into the ADR when you write it: (1) reword the "grep returns zero matches" citation (it returns 9 hits; the correct claim is "no on_hold enum value exists in readingstatus.lua/librarystore.lua/library-design.md"); (2) phrase Claim 3's response shape as "echoes the request body in the HTTP response; persisted DB status may differ via the rereading projection at reading-attempt.service.ts:114" since the current "(echo of request, not projection)" understates a real server behavior the investigation otherwise acknowledges.
