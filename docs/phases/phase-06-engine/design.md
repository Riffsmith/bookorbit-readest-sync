# Design: `internal/sync/engine` — Sync Orchestration (Phase 6)

> **⚠️ Two ADR addenda supersede parts of this document.** The design as approved
> is preserved unchanged below, but two live-server invalidations rewrote
> §6.2 (the `MatchCandidate.Source` value) and §6.3 second bullet (the
> "hash absent from both lists" handling). Before relying on this doc's
> §6.2 or §6.3 for current contract, see the hub README's
> "[What shipped DIFFERENTLY](./README.md#what-shipped-differently-from-the-design)"
> section, and read
> [Addendum 1](./decision-record.md#addendum--live-server-invalidation-of-decision-e-matchcandidatesource)
> and
> [Addendum 2](./decision-record.md#addendum-2--live-server-invalidation-of-63-hash-absent-from-match-check-response-handling)
> in the ADR.

**Status:** implemented and accepted. This document is the authoritative design record for the shipped engine — it describes the code as built in internal/sync/engine.go and internal/sync/engine_test.go, not a proposal.
**Scope:** the sync engine only — `Engine.RunOnce` / `Engine.Run`, the batching/retry/watermark/cache orchestration that ties `internal/readest`, `internal/bookorbit`, and `internal/sync/state` together. `internal/readest`, `internal/bookorbit`, and `internal/sync/state` are **complete and accepted** (Phases 3–5); this document does not redesign them and proposes no changes to their public APIs unless explicitly called out as a required companion change (see §14).

Authoritative behavior is taken from the reference plugins, re-read for this document: `bookorbit_sweep.lua` (batch sizes, match/unmatched bookkeeping, per-book completion gating, stats-watermark back-off), `bookorbit_state.lua` (`setMatched`/`setUnmatched` coupling), `bookorbit_book_sync.lua` (`stepProgress`'s unchanged-percentage tolerance), `syncbooks.lua`/`librarystore.lua` (pull watermark = `max(synced_at, updated_at, deleted_at)`, dummy-hash filter), and `docs/implementation-roadmap.md`'s Phase 6 description. Where the reference plugins don't have a directly applicable behavior (the bridge has no per-device reader, no live document, no full-library sweep), that gap is called out explicitly in §5 and a bridge-specific decision is proposed and flagged for approval.

---

## 0. Housekeeping item required at implementation time

**`go.mod` currently declares `module github.com/user/bookorbit-readest-sync`.** `github.com/user/...` is a placeholder module path, not a real one. Every import in the codebase (`internal/readest`, `internal/bookorbit`, `internal/config`, `internal/sync`, `internal/util`, `internal/token`, `cmd/bridge`) is rooted at this path. Before or during Phase 6 implementation, **this placeholder must be replaced with the real module path** (presumably `github.com/Riffsmith/bookorbit-readest-sync`, per the repository this design targets), and every import statement across the tree updated to match (`go mod edit -module ...` plus a project-wide import rewrite, e.g. via `gofmt -r` or `go fix`). This is called out explicitly here because Phase 6 is the phase that finally makes the binary do real work end-to-end (pull → match → push), and shipping it under a placeholder module path would make it unusable as an importable/installable Go module and would need to be redone anyway. The same applies to any other dummy/placeholder value discovered during implementation (e.g., `cmd/bridge/main.go`'s `version = "0.1.0-dev"` build-time placeholder is fine to keep — it's designed to be overridden via `-ldflags` — but should be confirmed, and any TODO/FIXME markers or copy-pasted example values left over from scaffolding should be swept and corrected before Phase 6 lands).

---

## 1. Overall responsibility — ownership boundaries

Phase 6 draws the boundary the roadmap always intended: **the engine is the only package that knows both `readest` and `bookorbit` exist.** Everything it needs from either side is already built and already faithful to its own domain (Phases 3–5). The engine's entire job is:

1. Decide **what** to pull (the watermark) and drive `readest.SyncClient.PullBooks`.
2. Decide **what changed** (percentage deltas) from the rows it gets back.
3. Decide **which hashes need resolving** in BookOrbit (`bookorbit.API.MatchCheck`) and **which resolved books need a progress write** (`bookorbit.API.BulkProgress`, with `UpdateProgress` as a version fallback).
4. Own **all retry/backoff policy** — neither client retries on its own (verified: both clients return classified, unretried sentinel errors and document this explicitly in their own design docs).
5. Own **all local-state read/write ordering** — `state.Store` is a passive key-value surface; the engine is the only thing that decides when a match is trustworthy, when an unmatched hash should be rechecked, and how far the watermark is allowed to advance.

The engine owns **no HTTP, no JSON, no token lifecycle** — those stay exactly where Phases 3–5 put them.

| Concern | Owner | Crosses via |
|---|---|---|
| Readest token lifecycle | `readest.Auth` (unchanged) | `readest.Authenticator`, consumed internally by `readest.Client` |
| Readest transport + one 401/403 retry | `readest.Client` (unchanged) | `readest.SyncClient` interface |
| BookOrbit transport + error classification | `bookorbit.Client` (unchanged) | `bookorbit.API` interface |
| Local persistence (tokens, watermark, match cache, unmatched cooldown, device id) | `state.Store` (unchanged) | `state.Store` interface |
| **Pull cadence, watermark advance/retreat** | **engine (new)** | — |
| **Percentage-change detection, unchanged-skip** | **engine (new)** | — |
| **Match-check / bulk-progress batching** | **engine (new)** | — |
| **Retry/backoff across batches** | **engine (new)** | — |
| **Bulk→singular-PUT fallback policy** | **engine (new)** | — |
| **Match/unmatched cache coupling rules** | **engine (new)** | — |

Nothing in Phases 3–5 needs to change to support this. The engine consumes `readest.SyncClient`, `bookorbit.API`, and `state.Store` purely as already-defined interfaces.

---

## 2. What the reference plugins verify, and what the bridge has no equivalent for

This table is the spine of the whole design: every behavior below is either **verified** (a specific line/function in the reference plugin is cited) or **bridge-specific** (no reference precedent exists because the bridge has no live document/reader, so a new but minimal decision must be made and is flagged in §13).

| Behavior | Verified reference | Bridge equivalent |
|---|---|---|
| Match-check batch size 500 | `bookorbit_sweep.lua: MATCH_BATCH = 500` | `config.Bridge.MatchBatchSize` (already defaults to 500) |
| Bulk-progress batch size 100 | `bookorbit_sweep.lua: PROGRESS_BATCH = 100` | `config.Bridge.ProgressBatchSize` (already defaults to 100) |
| `setMatched` clears the unmatched cooldown for that hash | `bookorbit_state.lua:setMatched`: `self.unmatched[md5] = nil` | Engine must call `state.ClearUnmatched(hash)` whenever it calls `state.SetMatch(hash, ...)` |
| `setUnmatched` clears any existing match record for that hash | `bookorbit_state.lua:setUnmatched`: `self.books[md5] = nil` | Engine must call `state.DeleteMatch(hash)` whenever it calls `state.SetUnmatched(hash, now)` |
| A bulk-progress response's `unmatched` list invalidates a previously-good match | `bookorbit_sweep.lua:stepProgressNext`: `if unmatched[item.hash] then ctx.state:setUnmatched(item.hash) ...` | Engine treats `BulkProgressResponse.Unmatched` exactly the same way |
| Unchanged-percentage skip tolerance | `bookorbit_book_sync.lua:stepProgress`: `math.abs(pct - pushed) <= 0.001` | Engine uses the same `0.001` tolerance against `MatchRecord.LastPushedPct` |
| Pull watermark = `max(synced_at, updated_at, deleted_at)`, dummy hash excluded | `librarystore.lua:parseSyncRow`/`row_pull_cursor`; already implemented in `readest.BookRow.WatermarkMs()`/`IsDummy()` | Reused as-is; no new code needed |
| A partial batch's cursor backs off so nothing is lost | `bookorbit_state.lua:applyStatsAck`: "back off by one second so the remainder is fetched next round" | **Extended** by the engine to the coarser Readest pull watermark (§5.4) — bridge-specific, because the bridge's schema has one scalar watermark, not a per-book stats watermark |
| Deleted-book handling | reverse-engineering report §8: "Skip/ignore on bridge side... don't attempt to un-sync" | Engine skips push but also actively clears any stale match/unmatched record (§5.3) — a small, deliberate addition beyond "ignore," flagged for approval |
| Full-library sweep, `libraryVersion`/`needsFullRecheck` gating | `bookorbit_sweep.lua`, `bookorbit_state.lua` global fields | **Explicitly out of scope** — no equivalent state field exists or is proposed (§5.6) |
| Bulk→singular-PUT fallback trigger | **No reference precedent** — Phase 5 design §9/§15.7 explicitly defers this to Phase 6 | Bridge-specific policy, defined in §8 |
| `MatchCandidate.Source`/`.LastOpen`/`.MetadataAmbiguous` values | Plugin populates from live reader context (`"current_file"`, `"statistics"`, stats-derived last-open, ambiguity from duplicate stats rows) | **No such context exists in a headless bridge.** Bridge-specific constants proposed in §6.2. (`Source` originally proposed as `"readest"`, later invalidated by live-server evidence — see ADR Addendum — and replaced with `"file"`.) |
| Retry/backoff shape | Not present in either plugin (KOReader's own network manager retries UI-level actions; the plugins themselves do not implement exponential backoff) | Fully bridge-specific (§7), governed by already-shipped `config.Bridge.Retry*` fields |

---

## 3. Public API (proposed)

No new exported types. The interface the engine already exposes is sufficient and stays exactly as constructed in Phase 0/2:

```go
func NewEngine(cfg config.Config, rdAuth readest.Authenticator, rd readest.SyncClient,
    bo bookorbit.API, st state.Store, log *slog.Logger) *Engine   // unchanged signature

func (e *Engine) RunOnce(ctx context.Context) error   // implemented; ErrNotImplemented removed
func (e *Engine) Run(ctx context.Context) error       // implemented; ErrNotImplemented removed
func (e *Engine) PollInterval() time.Duration         // unchanged
```

`sync.ErrNotImplemented` is deleted once no method returns it — same convention Phases 4/5 followed when their stubs were filled in (`ErrNotImplemented` was removed from `readest` in Phase 4 and never existed in `bookorbit`).

**Note on `rdAuth`:** `Engine` already stores a `readest.Authenticator` separately from `rd readest.SyncClient`, even though `readest.Client.PullBooks` already owns its own auth handling internally (including the 401/403 force-refresh-and-retry). The engine has no independent need to call `rdAuth` directly — I recommend **leaving the field unused** rather than either wiring up a redundant pre-flight auth check (which `PullBooks` already performs) or removing the field (a breaking constructor change with no functional benefit). This is a zero-risk, non-blocking note, not a decision requiring approval.

---

## 4. High-level flow

```
RunOnce(ctx):
  1. since := st.Watermark()
  2. rows, err := withRetry(pull)(ctx, since)          — auth error => abort RunOnce
  3. partition rows into: dummy (drop) / deleted / normal
  4. compute the "safe" watermark ceiling from ALL non-dummy rows (§5.4)
  5. for each deleted row: reset its local state (§5.3)
  6. for each normal row: compute percentage; classify by cache state (§5.1–5.2, §9 state diagram)
  7. gather hashes needing match-check; batch by MatchBatchSize; call bo.MatchCheck (retried)
  8. apply match-check results: SetMatch/ClearUnmatched or SetUnmatched/DeleteMatch (§6.3)
  9. gather (hash, bookFileID, bookID, pct, ts) needing a push; batch by ProgressBatchSize
 10. call bo.BulkProgress (retried), or bo.UpdateProgress per item if bulk is known-unsupported (§8)
 11. apply push results: SetMatch (success) or SetUnmatched+DeleteMatch (hash in response.Unmatched)
 12. compute final watermark: full ceiling if nothing failed, else retreat to just before the
     earliest row that failed a step (§5.4)
 13. st.SetWatermark(newWatermark); st.Save()
 14. return errors.Join of every batch failure encountered (nil if none)

Run(ctx):
  loop:
    RunOnce(ctx) — log any error, never exit the loop because of it
    sleep PollInterval(), interruptible by ctx.Done()
    on ctx.Done(): return ctx.Err()
```

---

## 5. Watermark handling

### 5.1 Pulling

`since := st.Watermark()` (0 on first run — full library, matching the plugin's `since=0` full pull). `rd.PullBooks(ctx, since)` is called through the retry helper (§7); a classified auth failure (`readest.ErrUnauthorized`) aborts `RunOnce` immediately with no further processing and no state mutation. Any other classified error, after retries are exhausted, also aborts `RunOnce` for this pass — there is nothing to process without a library, so unlike a match-check/bulk-progress batch failure (which is scoped to specific hashes), a pull failure is **fatal to the whole RunOnce**, matching `docs/implementation-roadmap.md`'s phrase "auth errors → fail fast" extended to any unrecoverable pull failure, since a failed pull leaves nothing else to do this pass. No `st.Save()` runs in this case (nothing changed).

### 5.2 Row classification

For every row in the response:

- `row.IsDummy()` → drop immediately. **Excluded from every watermark calculation**, exactly as `parseSyncRow` in the reference plugin filters it before the row ever reaches `pull_ts` accumulation.
- `row.IsDeleted()` → handled per §5.3. **Included** in watermark ceiling calculation (a deleted row still has a real `updated_at`/`deleted_at` and the reference plugin's own cursor math includes it).
- otherwise → a "normal" row, handled per §6.

### 5.3 Deleted rows

Per the reverse-engineering report (§8, verified conclusion): "Skip/ignore on bridge side... don't attempt to un-sync in BookOrbit for v1." The engine does not call BookOrbit at all for a deleted row. Beyond pure skipping, I propose the engine also **resets any local tracking for that hash**: `state.DeleteMatch(hash)` and `state.ClearUnmatched(hash)`. Rationale: if the same hash reappears later (the user re-uploads the book to Readest, or restores it from Readest's own trash), the bridge should treat it as brand-new rather than trusting a stale `MatchRecord.LastPushedPct` or a stale `Unmatched` cooldown timestamp that predates the deletion. This is a small, deliberate addition beyond "ignore" and is flagged in §13 for explicit approval, since it is not directly verified in any reference source (BookOrbit's own KOReader plugin never handles a "book deleted upstream" case at all — deletion detection is unique to this bridge design).

### 5.4 Advancing vs. retreating the watermark

This is the single most important correctness decision in this design, so it gets its own subsection.

**The problem:** if the watermark advances to include a row whose match-check or push genuinely failed (after retries), the next poll's delta-pull (`since=newWatermark`) will never return that row again unless Readest independently produces a new update for it. The failed push would be silently lost forever.

**The reference precedent:** `bookorbit_state.lua:applyStatsAck` handles an analogous problem for the *reading-statistics* watermark: when a batch of stat events is only partially acknowledged, the code deliberately **backs the cursor off by one second** rather than advancing past the boundary, so "the remainder is fetched next round." The comment in that function is explicit about the intent: never let a partially-processed batch permanently lose data.

**The bridge's extension of that pattern:** the bridge has one scalar watermark (`state.Store.Watermark()`), not a per-book stats watermark, so the "back off" has to happen at the level of the whole pull rather than per-book. The rule:

- Track, per `RunOnce`, the set of rows whose match-check or push step **genuinely failed** (a classified, non-auth, retry-exhausted error on the batch that row belonged to). Rows skipped for benign reasons (deleted, dummy, no usable progress tuple, unchanged percentage, unmatched-and-still-in-cooldown) are **not** failures.
- If the failed set is empty: `newWatermark = max(row.WatermarkMs() for every non-dummy row)`.
- If the failed set is non-empty: `newWatermark = min(row.WatermarkMs() for row in failed set) - 1` (millisecond back-off, the direct analogue of the plugin's one-second back-off, scaled to the bridge's millisecond-resolution cursor). This guarantees every failed row — and, harmlessly, every row that happened to succeed but shares or exceeds that timestamp — is re-included in the next pull. Re-processing an already-successful row is safe and cheap: a cached match short-circuits `MatchCheck`, and an unchanged percentage short-circuits the push (§6.1).
- `newWatermark` never regresses below the pre-existing `st.Watermark()` (a defensive floor — should not be reachable given the math above, but guards against a pathological row with a zero/garbage timestamp).

This keeps the watermark strictly a "how far can I safely stop re-asking Readest for this data" cursor, and keeps retry-ability entirely a function of "did this row's timestamp survive the ceiling," with no new persisted fields.

### 5.5 Persistence timing

`state.Store.Save()` is called **once, at the end of every `RunOnce`** (success or partial failure), regardless of outcome, so a crash between polls loses at most one poll interval's worth of progress instead of everything since the process started. `cmd/bridge/main.go`'s existing `defer st.Save()` at process-exit becomes a harmless, redundant final safety net and needs no change. This is flagged as a design decision in §13 (whether to save once per `RunOnce`, once per batch, or only at the very end) — I recommend once per `RunOnce` as the simplest option that closes the actual gap (today, nothing calls `Save()` between polls at all, which is a real bug-in-waiting once `Run` starts looping for hours).

### 5.6 Explicitly out of scope: full-library recheck / `libraryVersion`

`bookorbit_sweep.lua`/`bookorbit_state.lua` track a `libraryVersion` token and a `needsFullRecheck` flag so that if BookOrbit's own library changes underneath a device, previously-unmatched hashes get rechecked even without new Readest-side activity. The bridge's `state.Data` (Phase 2, already shipped) has no equivalent field, and adding one is a schema change explicitly outside this phase's stated goal of minimizing complexity. **Proposed:** do not implement this for v1. The existing `UnmatchedCooldown` (24h default) already provides an eventual-recheck safety net — a hash stuck as unmatched will be retried once per cooldown period regardless of new Readest activity, since the engine's recheck trigger (§6.1) is "cooldown expired," not "new activity since last check." This is flagged in §13.

---

## 6. Match cache and percentage-change interaction

### 6.1 Per-hash decision

For every normal (non-deleted, non-dummy) row with a usable `row.Percentage()` (skip the row, benignly, if `ok == false` — no progress tuple or non-positive total):

```
rec, err := st.Match(hash)
switch {
case err == nil && rec.LastPushedAt != 0 && math.Abs(pct-rec.LastPushedPct) <= 0.001:
    // Matched, already pushed, unchanged — nothing to do.
case err == nil:
    // Matched (possibly never pushed, LastPushedAt == 0) and changed (or fresh) — queue for push.
case errors.Is(err, state.ErrNotFound):
    at, inCooldown := st.UnmatchedAt(hash)
    if inCooldown && time.Since(at) < cfg.Bridge.UnmatchedCooldown {
        // Skip: recently checked and confirmed unmatched.
    } else {
        // Queue for match-check.
    }
}
```

`rec.LastPushedAt == 0` is the existing zero-value sentinel for "matched but never successfully pushed" — no schema change needed; a freshly-matched hash always gets pushed at least once regardless of the 0.001 comparison (guards against the degenerate case where a brand-new book's real percentage happens to be exactly 0.0, which would otherwise compare "unchanged" against the zero value default).

### 6.2 Match-check candidates

For every hash queued for match-check, the engine builds a `bookorbit.MatchCandidate` from the corresponding `BookRow`:

| Field | Source | Verified or bridge-specific |
|---|---|---|
| `Hash` | `row.BookHash` | verified |
| `Title` | `row.Title` | verified |
| `Authors` | `row.Author` | verified |
| `LastOpen` | `util.MsToSeconds(row.UpdatedMs())` (fallback to 0 if unparseable) | **bridge-specific** — the plugin populates this from a live reader/stats "last opened" timestamp; the bridge has no such signal, so Readest's own `updated_at` is the closest available proxy |
| `Source` | constant `"file"` | **bridge-specific, later validated by live server** — see §6.2 note below. The plugin uses `"current_file"`/`"statistics"`/`"file"` depending on which live KOReader subsystem produced the candidate; the bridge uses `"file"` (the value the plugin itself uses for a hash-resolvable candidate with no live-reader or stats-DB context, `bookorbit_catalog_download.lua:335`). The original Phase 6 value `"readest"` was invalidated by the first live match-check (HTTP 400: `source must be one of: current_file, file, statistics`); see `docs/adr/phase-6-decision-record.md` Addendum. |
| `MetadataAmbiguous` | constant `false` | **bridge-specific** — the plugin sets this when multiple `statistics.sqlite3` rows share a hash with different titles/authors; the bridge has no local statistics database and therefore no ambiguity signal |

Batching uses `internal/util.BatchFunc` against `config.Bridge.MatchBatchSize`, mirroring `MATCH_BATCH = 500`. Each chunk is one `MatchCheck` call, retried per §7.

### 6.3 Match-check result handling

Per response:
- Every hash in `resp.Matches`: `st.SetMatch(hash, state.MatchRecord{BookFileID: m.BookFileID, BookID: m.BookID})` (leaving `LastPushedAt`/`LastPushedPct` at zero — "matched, not yet pushed"), then `st.ClearUnmatched(hash)` (§2, verified coupling).
- Every hash **not** in `resp.Matches` — whether the server lists it explicitly in `resp.Unmatched` or omits it from both lists (the latter is the common case for books BookOrbit's library has never seen; verified against the live server in Phase 6 ADR Addendum 2): `st.SetUnmatched(hash, now)`, then `st.DeleteMatch(hash)` (§2, verified coupling — defensive; normally there's nothing to delete for a hash that was never matched, but harmless and correct if it was previously matched and has now regressed). This mirrors the reference plugin's `stepMatchNext` (`bookorbit_sweep.lua:328-332`: "if not matched[md5] then setUnmatched(md5)"), which reaches the same verdict by consulting only `body.matches`. Doing this lets the watermark advance normally for these rows (Decision A) and settles them into the `UnmatchedCooldown` recheck gate (§6.1), so they are not re-submitted on every poll. Logged at `debug` (not `warn`) — the situation is normal, not exceptional. In particular, such hashes are **not** appended to `failedWatermarks`; only a genuine classified batch failure (next bullet) retreats the watermark.
- A classified, non-auth batch failure (after retry exhaustion): every hash in that chunk is a "failure" for watermark purposes; no state is mutated for them.
- An auth failure (`bookorbit.ErrUnauthorized`) on any chunk: abort the remainder of `RunOnce` immediately (fail-fast), returning the wrapped error. Whatever state mutations already happened in earlier chunks/steps this pass are kept (there is no rollback), and `st.Save()` still runs before returning.

---

## 7. Retry / backoff

Neither `readest.Client` nor `bookorbit.Client` retries; both explicitly document that this is deliberate and is the engine's job. The engine introduces one small, unexported, pure-enough-to-unit-test helper, used identically for the pull, each match-check chunk, and each bulk-progress/update-progress chunk:

```go
// classification the helper needs from a call's error, supplied by a
// small package-level function per client (readest vs bookorbit) that maps
// their respective sentinel errors to a shared three-way verdict.
type outcome int
const (
    outcomeSuccess outcome = iota
    outcomeRetry            // rate limited / server error / network error
    outcomeFatal             // auth error — abort the whole RunOnce
    outcomeSkip              // bad request / malformed / unsupported / body too large — non-retryable, batch-scoped
)

func withRetry(ctx context.Context, cfg config.BridgeConfig, clock func() time.Time, sleep func(time.Duration),
    call func() error, classify func(error) outcome) error
```

- Exponential backoff: `delay = min(cfg.RetryInitialBackoff * 2^attempt, cfg.RetryMaxBackoff)`, attempted up to `cfg.RetryMaxAttempts` times (already-shipped config defaults: 5 attempts, 500ms initial, 30s cap — no new config fields).
- `outcomeFatal` (auth) never retries; propagates immediately.
- `outcomeSkip` (bad request / malformed / unsupported endpoint / body too large) never retries — retrying an unchanging request against an unchanging server bug cannot help, matching the reasoning already documented in the Phase 5 design for `ErrBodyTooLarge`.
- `outcomeRetry` (rate limited / server error / network error) retries with backoff; the sleep between attempts respects `ctx.Done()` (a `select` on a timer and the context, so shutdown remains immediate — matching the existing requirement that context cancellation propagate transparently through both clients).
- `sleep`/`clock` are injectable function values (same convention already used for `Auth.now` and `Client.now` in Phases 3 and 5), so retry/backoff timing is fully unit-testable without real sleeps.

Classification functions (`classifyReadestErr(err) outcome`, `classifyBookOrbitErr(err) outcome`) are two small, pure `errors.Is` switches, one per package, living in `internal/sync` (they must live here, not in `readest`/`bookorbit`, since only the engine is allowed to know both packages' sentinel sets — this is exactly the boundary Phase 4's design insisted on).

---

## 8. Bulk-progress → singular-PUT fallback

Phase 5's design explicitly deferred this policy (§9, §15.7 of `docs/phase-5-design-temp.md`): `bookorbit.ErrUnsupportedEndpoint` (404/405) is the client-level signal; the *trigger* and *behavior* of falling back to `UpdateProgress` were left for this phase.

**Proposed policy:**

- `Engine` carries one unexported, in-memory field: `bulkUnsupported bool`. It starts `false` on every process start (deliberately **not persisted** — see rationale below) and is set `true` the first time any `BulkProgress` call returns `bookorbit.ErrUnsupportedEndpoint`.
- Once set, for the **remainder of the process's lifetime** (across every subsequent `RunOnce`, not just the current one), the engine stops calling `BulkProgress` entirely and instead pushes each queued item individually via `UpdateProgress`, one HTTP call per hash, still going through the same retry helper (§7) per item.
- **Not persisting this flag is a deliberate choice.** Persisting it would require a `state.Data` schema addition (again cutting against "minimize complexity" and "avoid future churn") for a condition that is expected to be rare (an old, unupgraded BookOrbit server) and cheap to rediscover: one wasted, fast-failing `BulkProgress` call per fresh process start, which then falls back for the rest of that process's uptime. Given the bridge's target deployment (a long-running daemon or an infrequent cron/systemd-timer invocation), rediscovering this once per restart is negligible cost for meaningfully less state-schema surface.
- `UpdateProgress` has no response body and cannot report which hashes BookOrbit failed to recognize (unlike `BulkProgress`'s `Unmatched` field) — this is a known, already-documented gap (`docs/phase-5-design-temp.md` §16 items 2/3/6 flag this exact ambiguity as unresolved without a live server). Consequence: while in fallback mode, the engine **cannot** invalidate a stale match via the "hash came back unmatched" path — it can only detect success (`nil` error → `SetMatch` with fresh `LastPushedAt`/`LastPushedPct`) or failure (any error → per §7's classification, either a retry-exhausted batch-scoped "failure" for watermark purposes, or an auth abort). This degradation is accepted and documented rather than worked around, since inventing an unverified detection mechanism here would be building on speculation.

This fallback logic and its rationale for not persisting the flag are called out in §13 for explicit confirmation, since it is a bridge-specific behavior with no direct reference precedent.

---

## 9. State transitions (per book hash)

```
                         ┌──────────────────────────┐
                         │         Unknown           │◄───────────────┐
                         │ (no Match, no Unmatched)   │                │
                         └─────────────┬─────────────┘                │
                       MatchCheck: no match            MatchCheck: match found
                                   │                                   │
                                   ▼                                   ▼
                    ┌───────────────────────────┐          ┌───────────────────────┐
                    │   Unmatched (cooldown)     │          │   Matched, unpushed     │
                    │ st.Unmatched[hash] = now    │          │ LastPushedAt == 0        │
                    └─────────────┬──────────────┘          └───────────┬───────────┘
                    cooldown expires, re-check                    push succeeds
                                   │                                   │
                                   └────────────► Unknown-equivalent   ▼
                                     (re-enters MatchCheck path)  ┌────────────────────────┐
                                                                  │ Matched, pushed-current  │
                                                                  │ (pct == LastPushedPct)    │
                                                                  └───────────┬────────────┘
                                                                     pct changes beyond 0.001
                                                                              │
                                                                              ▼
                                                                  ┌────────────────────────┐
                                                                  │  Matched, pushed-stale   │
                                                                  │  (queued for push again) │
                                                                  └───────────┬────────────┘
                                                                        push succeeds
                                                                              │
                                                                              └──► back to pushed-current

Any state ──(BulkProgress/UpdateProgress reports this hash as unmatched)──► Unmatched (cooldown)
Any state ──(row.IsDeleted())──► Unknown  (DeleteMatch + ClearUnmatched — full reset)
```

---

## 10. Concurrency expectations

**No synchronization is introduced.** `RunOnce` is entirely single-goroutine and sequential — pull, then match-check batches in order, then push batches in order — matching `docs/implementation-brief.md`'s "polling daemon" model and the identical conclusion already reached for both `readest.Client` (Phase 4 §8) and `bookorbit.Client` (Phase 5 §12): each is safe for concurrent use by construction, but the bridge never actually calls them concurrently, because there is exactly one caller (`Engine.RunOnce`) and it runs serially. `state.Store`'s own internal locking (`FileStore`/`MemStore` both guard their maps with a `sync.Mutex`) is sufficient for the engine's single-writer access pattern and needs nothing additional from this package.

`Run(ctx)` is a simple `for` loop: call `RunOnce`, log its error (if any) without stopping, then wait on `PollInterval()` via an interruptible timer/`ctx.Done()` select, matching the roadmap's explicit requirement that "`--once` and shutdown are fast." `Run` returns only on `ctx.Done()` (returning `ctx.Err()`), never because `RunOnce` failed — a daemon should keep trying on its configured cadence through transient outages; a supervisor (systemd, Docker restart policy) is the correct place to decide whether persistent failure should restart the process, not this loop.

---

## 11. Error propagation

- **Auth failures** (`readest.ErrUnauthorized` from the pull, or `bookorbit.ErrUnauthorized` from any match-check/bulk-progress/update-progress call) abort `RunOnce` immediately and are returned as the sole error (wrapped with context, e.g. `fmt.Errorf("sync: pull books: %w", err)`), after still calling `st.Save()` for whatever was already committed.
- **Batch-scoped failures** (retry-exhausted `outcomeRetry` or immediate `outcomeSkip` classifications on a match-check or push chunk) do not abort `RunOnce`. They are logged at `warn` with enough context (family: match-check/bulk-progress/update-progress, chunk size, affected hash count) to diagnose without a debugger, accumulated into a slice, and combined via `errors.Join` into `RunOnce`'s final return value. `errors.Is` against any of `readest`'s or `bookorbit`'s sentinels continues to work through the joined error (a property of `errors.Join` since Go 1.20; the module targets Go 1.26, so this is available with no new dependency).
- **`--once` mode** (`cmd/bridge/main.go`) surfaces `RunOnce`'s error (joined or not) as today, exiting non-zero on any failure — appropriate for a cron/systemd-timer deployment, where a non-zero exit is the expected signal that something needs attention.
- **Daemon mode** never propagates `RunOnce`'s error out of `Run`; only `ctx.Done()` does. `cmd/bridge/main.go`'s existing `errors.Is(runErr, sync.ErrNotImplemented)` checks (for both the `--once` and daemon branches) become dead code once `ErrNotImplemented` is removed from `sync` — they should be deleted as part of this phase's companion change to `cmd/bridge/main.go` (§14).

---

## 12. Testing strategy

All tests use `state.NewMemStore()` (already exists, already exercises the full `Store` contract per `internal/sync/state/state_test.go`) and hand-written fakes for `readest.SyncClient` and `bookorbit.API` (both are already plain interfaces — no test-double machinery needs to be built beyond simple struct-based fakes, following the exact pattern `internal/readest/client_test.go` and `internal/bookorbit/client_test.go` already established with their `stubDoer`). No real network, no real sleeps (the retry helper's `sleep`/`clock` are injected, per §7).

Proposed cases (table-driven where the shape allows):

1. Empty pull → no BookOrbit calls, watermark unchanged, `RunOnce` returns nil.
2. Only-dummy-row pull → filtered before any processing; watermark unchanged.
3. Fresh unmatched hash → `MatchCheck` called with correct candidate fields (`LastOpen` derived, `Source="file"`, `MetadataAmbiguous=false`); match found → `SetMatch`+`ClearUnmatched`; always pushed regardless of pct value (including pct == 0). (`Source` was originally `"readest"`; invalidated by first live run, see ADR Addendum.)
4. Already-matched, percentage unchanged (within 0.001) → no `BulkProgress` call; watermark still advances.
5. Already-matched, percentage changed → pushed; `MatchRecord` updated with new `LastPushedAt`/`LastPushedPct`.
6. Deleted row → `DeleteMatch`+`ClearUnmatched` called, no BookOrbit calls, watermark still advances.
7. Row with an unusable progress tuple (`Percentage()` returns `ok=false`) → skipped, watermark still advances.
 8. `MatchCheck` returns a hash in `Unmatched` (or omits it from both `Matches` and `Unmatched` — the verified live-server behavior; see ADR Addendum 2) → `SetUnmatched`+`DeleteMatch`, no push, watermark advances (not retreats), next `RunOnce` doesn't recheck within cooldown. (Tested by `TestRunOnceUnmatchedPastCooldownRechecks` for the explicit-unmatched-list shape and `TestRunOnceAbsentFromMatchResponseIsUnmatchedNotFailure` for the absent-from-both shape.)
9. Unmatched hash within cooldown → no `MatchCheck` call at all for that hash.
10. Unmatched hash past cooldown → rechecked.
11. `MatchCheck` chunk fails with a retryable error on every attempt → hash is not marked unmatched, watermark retreats to `min(failed rows) - 1`, `RunOnce` returns a non-nil joined error.
12. `BulkProgress` chunk fails the same way → `MatchRecord.LastPushedPct` not advanced, same watermark retreat.
13. `bookorbit.ErrUnauthorized` from `MatchCheck`/`BulkProgress` → `RunOnce` aborts immediately; prior successful chunks' state changes in this same pass are preserved; `st.Save()` still runs.
14. `readest.ErrUnauthorized` from the pull → `RunOnce` aborts before any processing; no state mutated.
15. `bookorbit.ErrUnsupportedEndpoint` from `BulkProgress` → subsequent pushes in the same `RunOnce`, and in a following `RunOnce` on the same `Engine` instance, go through `UpdateProgress` instead; per-item success updates `MatchRecord` normally.
16. Watermark retreat/advance pure-function tests, independent of the rest of the pipeline, given synthetic rows and a synthetic failed-hash set.
17. Retry/backoff helper pure-function tests: attempt counts, delay growth, cap enforcement, `outcomeFatal`/`outcomeSkip` never retrying, cancellation via `ctx.Done()` interrupting a backoff sleep immediately.
18. `Run` respects `ctx` cancellation promptly even mid-`PollInterval` sleep (a timer-based test with a short interval).
19. `Run` does not exit after a `RunOnce` error (loop continues; a fake client that fails on the first call and succeeds on the second confirms a second `RunOnce` happens).
20. `st.Save()` is called exactly once per `RunOnce` (a `state.Store` wrapper/fake counting calls), regardless of success or partial failure.
21. Percentage-tolerance boundary test at exactly `0.001` vs `0.0011` delta.
22. A hash reappearing after deletion resets cleanly and is treated as brand-new (no stale `LastPushedPct` bleed-through).

---

## 13. Verified implementation decisions

- **A.** Watermark retreat rule (§5.4): retreat to `min(failed rows' WatermarkMs) - 1` when any batch fails, otherwise advance to the max across all non-dummy rows. This is the load-bearing correctness fix for "don't lose a failed push forever," extending the plugin's verified stats-watermark back-off pattern to the bridge's single scalar cursor. **Alternative:** always advance regardless of failures (simpler, but reintroduces the data-loss risk described in §5.4 — not recommended).
Yes. Retrying a failed batch without retreating the watermark risks losing that row forever, and the design’s retreat rule is the direct bridge version of the plugin’s own “back off so the remainder is fetched next round” pattern. This one is the big correctness gate.
- **B.** Deleted-row handling actively clears `Match`/`Unmatched` state (§5.3), beyond the reference's "just ignore it." **Alternative:** truly no-op on deletion, leaving stale cache entries in place until they naturally expire/get overwritten.
Yes. Clearing Match and Unmatched on delete is the safer choice because it prevents stale cache junk from resurfacing later if the same hash comes back. The design calls this a small but deliberate step beyond the reference’s simple ignore behavior.
- **C.** `st.Save()` runs once per `RunOnce` (§5.5). **Alternative:** save less often (riskier) or after every batch (safer but adds I/O and lock churn for negligible benefit given `RunOnce`'s short duration).
Yes. Saving once per RunOnce is the sweet spot: enough durability to survive a crash between polls, without turning every batch into a tiny disk drama. The design says this is the simplest fix for the current “nothing saves during the loop” gap.
- **D.** Full-library recheck (`libraryVersion`/`needsFullRecheck`) is explicitly **out of scope** for v1 (§5.6), relying solely on the existing unmatched-cooldown as an eventual-recheck safety net. **Alternative:** add the schema fields and implement full parity with the plugin's recheck gating — a larger change touching `state.Data`.
Yes. Leaving full-library recheck out of v1 is the right call. The design is already explicit that the existing cooldown is the safety net, and adding libraryVersion parity now would drag state.Data into a wider schema change for limited payoff.
- **E.** `MatchCandidate.Source`/`LastOpen`/`MetadataAmbiguous` bridge-specific values (§6.2): constant `"file"`, `updated_at` as the `LastOpen` proxy, always `false` for ambiguity. **Alternative values:** originally proposed as `"readest"` for BookOrbit-side diagnostics readability. **Invalidated by live-server evidence** — the first live match-check returned HTTP 400 rejecting `"readest"` because BookOrbit's server validates `source` against a fixed enum of exactly `current_file|file|statistics`. Replaced with `"file"`, which is the value the reference plugin itself uses for the structurally equivalent "hash-resolvable candidate without live-reader or stats-DB context" case (`bookorbit_catalog_download.lua:335`). See `docs/adr/phase-6-decision-record.md` Addendum for the full record. `LastOpen` and `MetadataAmbiguous` were not affected by the same 400 and remain unchanged, pending confirmation on the next successful run.
Yes, with the bridge-specific values. Source = "file", LastOpen = updated_at, MetadataAmbiguous = false is the cleanest headless bridge mapping *among the values the live BookOrbit server actually accepts*. It keeps the data stable and does not falsely claim live-reader context (the bridge has none) or stats-DB provenance (the bridge has none). `"file"` is the value the reference plugin itself uses for a hash-resolvable candidate with no other context (`bookorbit_catalog_download.lua:335`), which is the bridge's exact situation. (Originally approved as `"readest"`; see ADR Addendum for the live-server invalidation.)
- **F.** Bulk→singular-PUT fallback policy (§8): in-memory-only flag, not persisted, rediscovered once per process restart; per-item pushes on fallback cannot detect per-hash "unmatched" the way bulk can. **Alternative:** persist the flag in `state.Data` (schema change) to avoid ever re-probing the unsupported endpoint, at the cost of the added field and migration.
Yes. Keep the bulk→singular fallback flag in memory only. Persisting it buys less than it costs, because this is a rare compatibility path and the design already accepts re-discovering unsupported bulk once per restart.
- **G.** Unmatched-cooldown re-check trigger is purely time-based (`config.Bridge.UnmatchedCooldown`, default 24h), **not** the plugin's "new activity since last check" comparison (§6.1, flagged also in §2's table). This is already effectively baked into the shipped Phase 2 `state`/`config` schema (there is no per-hash "last activity at last check" field to compare against), so confirming this is really a confirmation of already-existing scope, not a new decision — but it's called out explicitly since it is a real, if minor, behavioral divergence from the reference plugin.
Yes. Make the unmatched cooldown purely time-based. The design already says that the bridge has no per-hash “last activity at last check” field, so the plugin’s exact activity-based recheck logic is not a clean fit here.
- **H.** Auth-failure mid-run keeps whatever state mutations already happened in earlier batches this pass, rather than rolling them back (§6.3, §11). **Alternative:** treat an auth failure as reason to discard the whole pass's changes — more conservative, but adds complexity (a rollback/staging mechanism) for a scenario (BookOrbit credentials failing mid-run after having succeeded on an earlier call in the same run) that should be rare in practice, since a bad credential normally fails on the very first call.
Yes. Keep already-committed state mutations if auth fails mid-run. Rolling back would add staging machinery for a corner case that should usually fail on the first call anyway. The design already says st.Save() still runs before returning.
- **I.** Module path placeholder (`github.com/user/...` in `go.mod`) must be corrected before/at Phase 6 landing (§0) — confirming the target real path and the scope of the accompanying import rewrite.
Yes, definitely. Replace the placeholder module path with the real one, github.com/Riffsmith/bookorbit-readest-sync, and rewrite imports across the tree. The design explicitly calls this housekeeping item out as required before or during Phase 6 implementation.
