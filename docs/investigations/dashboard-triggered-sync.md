# Dashboard-Triggered Sync — Feasibility Investigation

**Status:** feasibility investigation only, per the brief. No production bridge code, BookOrbit code, or test code is proposed or written here. This document establishes whether the bridge can be triggered automatically when BookOrbit's dashboard is accessed, with the synced progress visible on the *first* page load (no manual refresh required), and — if so — what would have to change on both sides to make that work.

**Scope:** progress-only, one-way (Readest → BookOrbit), matching the shipped bridge. The `bookorbit` repo is a separate project owned by a different team and is treated here as a *downstream consumer of design proposals*, not as something this repo will modify directly. Status sync is explicitly out of scope for this document and is flagged in §9 as a follow-on.

---

## 1. Question and constraint

**Question:** when a user opens BookOrbit's dashboard, can the bridge be nudged to run one Readest pull + BookOrbit push *before* the dashboard's HTTP response is assembled, so the rendered page reflects the synced progress on the first try?

**Hard constraint (from the operator):** the synced progress must be visible on the first page load. The user should not have to refresh to see it.

**Soft constraint (architectural):** the dashboard must not hang if the bridge is slow or unreachable — the operator's reading experience cannot regress because a sidecar isn't running.

---

## 2. Triggers considered

Three shapes would *nudge* the bridge automatically when the dashboard loads. Only one satisfies the hard constraint.

### (a) Push model — BookOrbit awaits the bridge *(recommended, see §4)*
BookOrbit's scroller controller, before assembling cards, calls a tiny loopback HTTP endpoint on the bridge (`POST http://127.0.0.1:<port>/sync-now`) with a short, hard timeout (~2–3s). The bridge runs one `RunOnce` synchronously and returns; only on its return does BookOrbit's scroller query proceed. First-load visibility holds by construction.

### (b) Event model — fire-and-forget to the bridge
BookOrbit emits a "dashboard viewed" event (Socket.IO / SSE / async webhook) and returns the cards immediately. The bridge's `RunOnce` completes in the background and pushes the result via the existing real-time chain (see §3.1). The user sees the synced data ~250ms later, *not* on first load. **Rejected: violates the no-second-refresh constraint.**

### (c) Faster bridge poll cadence
The bridge continues to poll on its timer at a short interval (e.g., 1 min). The dashboard loads get whatever the bridge last synced. **Rejected: cannot satisfy first-load visibility unless the cadence is sub-second (chatty and pointless).**

### Idle / default behavior
If BookOrbit never calls the bridge (older BookOrbit build, or the operator hasn't enabled the integration), the bridge must continue to work on its existing poll timer. The webhook is strictly *additive*.

---

## 3. BookOrbit server-side findings (cited to `reference/bookorbit/server/`)

### 3.1 The real-time chain already exists end-to-end

The codebase already has a working external-write → live-UI-refresh chain. The bridge would slot in as a new trigger of the same chain, not require building a new one.

1. **Write path emits an in-process event.** `koreader.service.ts:303` calls `this.achievementEvents.emit(ACHIEVEMENT_EVENT_BOOK_PROGRESS_CHANGED, { userId, bookId, bookFileId, progress, source: 'koreader' })` after `upsertReadingProgress` commits. The same call fires in the bulk path (`koreader.service.ts:200-285`, per-entry).
2. **The event is a hand-rolled Node `EventEmitter`, not `@nestjs/event-emitter`.** `achievement-events.service.ts:72` is literally `export class AchievementEventsService extends EventEmitter {}`. A `grep` for `EventEmitterModule.forRoot()` across `server/src` returns zero hits — confirmed, the framework's event-emitter module is intentionally not used. The event bus is in-process only; an external bridge cannot `.emit()` into it directly, only via an HTTP webhook that re-enters in-process code (see §3.4).
3. **Gateway subscribes on init and re-emits over Socket.IO.** `scan.gateway.ts:46-52` (`onModuleInit`) registers `handleBookProgressChanged`; `scan.gateway.ts:141-148` (`emitBookProgressChanged`) calls `this.server?.to(\`user:${payload.userId}\`).emit('book:progress-changed', event)`.
4. **Connection handler authenticates by JWT and joins the per-user room.** `scan.gateway.ts:63-79`: `await client.join(\`user:${user.id}\`)`.
5. **Vue client listens and debounces a refresh.** `useBookEvents.ts:80-83` registers the `book:progress-changed` listener; `useBookProgressRefresh.ts:1-22` debounces (250ms via `PROGRESS_REFRESH_DEBOUNCE_MS`) and re-calls `load`.
6. **Dashboard scroller already uses this hook.** `useDashboardScroller.ts:28` calls `useBookProgressRefresh(load)` — so when *any* progress event fires for the connected user, the scrollers re-fetch within 250ms.

This chain is **load-bearing for the push model** in §4: a bridge-triggered write that goes through the existing koreader plugin progress endpoint (or through the existing in-process koreader service) automatically produces a live UI refresh event for the dashboard — no new Socket.IO wiring required for the *progress* path.

### 3.2 Scroller cards are NOT cached — first-load visibility is achievable here by ordering alone

The dashboard scroller path does not hit a server-side cache:

- `dashboard.controller.ts:9-24` → `dashboard.service.ts:34-43` (`getScroller`) → `dashboard.service.ts:23-32` (`loadCardsByIds`) → `book-read.service.ts:30-32` (`findCardsByBookIds`) → `book.repository.ts:704-727` (`findCardsByBookIds`), which performs fresh LEFT JOINs to `userBookStatus` (`book.repository.ts:660`) and `readingProgress` (`book.repository.ts:666-670`) on every call.

A `grep` for `Redis|redis` across `dashboard/` and `book/` modules returns no matches. There is no cache layer between the repository and Postgres on the scroller path. **Therefore: as long as the bridge's `BulkProgress` transaction commits *before* the scroller's `findCardsByBookIds` query begins, the rendered cards reflect the new progress on the first response.** This is what the push model in §4 makes structural — BookOrbit `await`s the bridge and then queries.

### 3.3 Widget row IS cached — and the blocker for first-load freshness on widgets

The widget row (currently-reading count, reading streak, library overview, etc.) is explicitly cached:

- `dashboard-widget.service.ts:36-43` declares `liveCache` (TTL `DASHBOARD_LIVE_TTL_MS = 120_000`) and `staleCache` (TTL `DASHBOARD_STALE_TTL_MS = 300_000`). `dashboard-widget.service.ts:69-77` routes the per-widget calls through `this.liveCache.get(\`user.id\`, 'currently-reading', async () => { … })` *et al*.
- The cache is in-process LRU (`stats-cache.ts:1-122`), keyed `${scope}::${key}`; `clearForScope(scope)` (`common/cache/stats-cache.ts:62-78`) bumps a generation counter and evicts the scope's entries.

**Blocker:** there is today **no** code path that calls `liveCache.clearForScope(...)` or `staleCache.clearForScope(...)` when a koreader progress event fires — confirmed by `grep clearForScope` across `server/src/modules/dashboard` returning hits only in `stats-cache.ts` itself (not in any service caller). So a koreader/bridge progress write today does not invalidate the widget cache; the widget row can show 120s of stale "currently-reading" data after a koreader push. The scroller row refreshes via Socket.IO (§3.1, item 6 — `useBookProgressRefresh` re-`load`s the scroller), but the widget row does not because widgets aren't refreshed on the Socket.IO event either.

**Precedent for invalidation.** `user-statistics.service.ts:394` calls `this.cache.clearForScope(String(user.id))` after a `moveReadingSession` write — the same shape (`StatsCache`, same `clearForScope` method, same user-scoped key). A bridge-triggered write path on BookOrbit would call `liveCache.clearForScope(String(userId))` and `staleCache.clearForScope(String(userId))` after every successful bridge sync, mirroring the user-statistics precedent. The invalidation is small, scoped, and prior-art-blessed.

### 3.4 Webhook-with-custom-guard precedents already exist

The BookOrbit server has `@Public()` + custom `CanActivate` guard as a well-established pattern for "external actor pushes data in":

- `koreader-auth.guard.ts:13-58` — `KoreaderAuthGuard` validates `x-auth-user` / `x-auth-key` headers against `koreaderUsers` in the DB. Guarded at `koreader.controller.ts:51-77` (`@Public() @UseGuards(KoreaderAuthGuard) @Put('syncs/progress')` for the kosync protocol).
- `kobo/guards/kobo-token.guard.ts:18-67` — `KoboTokenGuard` authenticates by path param `:deviceToken` *or* header `x-kobo-deviceid`, looking up a stored token in the DB. Used across `kobo-sync.controller.ts`.

These precedents validate the *shape* of adding a new guard. Note that **both precedents do a DB lookup**, not a static shared-secret comparison — there is no purely config-driven shared-secret guard in the codebase today. Adding one is a small new variant but uses the identical `@Public()` + custom `CanActivate` pattern.

### 3.5 Request lifecycle and hooks

- Server is **Fastify** confirmed: `main.ts:1-2` (`import { FastifyAdapter, NestFastifyApplication } from '@nestjs/platform-fastify'`), `main.ts:32-33` (`const adapter = new FastifyAdapter(...)`), `DEVELOPMENT.md:32` (`NestJS 11, Fastify, Drizzle ORM`).
- Socket.IO adapter mounted at `main.ts:58` (`app.useWebSocketAdapter(new IoAdapter(app))`).
- `app.enableShutdownHooks()` is enabled (`main.ts:113`). `OnModuleInit` and `onApplicationBootstrap` lifecycle hooks are used heavily — see `file-watcher.service.ts:47` (chokidar watcher), `book-dock-watcher.service.ts:38` (Postgres `LISTEN`/`NOTIFY`), `scan.gateway.ts:46` (event-bus subscription).
- Global API prefix is `/api/v1` (`main.ts:60-62`), excluding a small explicit list (`Kobo`, legacy `v3`, `UserStorage`). A new `integrations` controller would receive the `/api/v1` prefix verbatim unless explicitly excluded.
- Global guards (`app.module.ts:157-163`): `SensitiveEndpointThrottlerGuard` → `JwtAuthGuard` → `PermissionGuard` → `LibraryAccessGuard`. The `@Public()` decorator is what opts a controller *out* of the JWT/permission/library-access chain; a custom guard still runs.

### 3.6 Status-change pathway has no Socket.IO subscriber today — out of scope here

`user-book-status.service.ts:69-91` emits `ACHIEVEMENT_EVENT_BOOK_STATUS_CHANGED` (`book.status-changed`) inside the `setManual` path *when the status actually changes*. But a `grep` for `ACHIEVEMENT_EVENT_BOOK_STATUS_CHANGED` across `server/src/modules/**/*gateway.ts` returns zero matches — no gateway subscribes. The only subscribers today are integration-event listeners (Readwise, Hardcover, StoryGraph). Setting a status via the koreader catalog route therefore does **not** trigger a dashboard refresh via the existing real-time chain.

This is flagged only because it's a relevant contrast. The dashboard-trigger as scoped here is **progress only** (§9). Bridging `book.status-changed` into the dashboard is the kind of follow-on that the separate `docs/status-sync-design-investigation.md` task would need to coordinate with BookOrbit changes anyway.

### 3.7 Concurrent-write semantics — the bridge must use a distinct `deviceId`

Progress writes (`PUT /koreader/syncs/progress` and `POST /koreader/plugin/progress`) use Postgres `INSERT … ON CONFLICT DO UPDATE` on `(bookFileId, userId, device, deviceId)` for `koreader_device_progress` and `(bookFileId, userId)` for the shared `reading_progress`. There is **no enclosing transaction** and **no `FOR UPDATE` lock** on the progress upsert path (`koreader.repository.ts:435-467`, `koreader.repository.ts:536-562`). Last writer wins at the row level.

There is one subtle but load-bearing detail: `koreader.repository.ts:559` deliberately does **NOT** update `updatedAt` on the `ON CONFLICT` branch for `readingProgress`:
```ts
set: {
  percentage, cfi, pageNumber: null, koreaderProgress: xpointer,
  ...
  updatedAt: sql`"reading_progress"."updated_at"`,
}
```
This preserves the existing `updatedAt` (set by BookOrbit's web reader) so that `koreader.service.ts:321-369`'s `getProgress` does not falsely timestamp a koreader push as the "newest web-reader sync." The practical consequence: **the bridge must use a `device`/`deviceId` distinct from `bookorbit-web`** so the two write populations don't share a `koreader_device_progress` row and so the deliberate `updatedAt`-preservation semantics stay intact. The existing bridge already does this (`internal/sync/state/state.go:39`, `DeviceID` is a generated UUIDv4 saved separately;_PHASE 6 ADR Addendum confirms the bridge sends `device: "readest-bridge"`), so this requirement is already met for progress.

Status writes (the koreader catalog `read-status` path routed via `user-book-status.service.ts:setManual → applyManualStatus` → `reading-attempt.service.ts:46-118` ⟹ `reading-attempt.repository.ts:98,109` `.for('update')`) **do** use `SELECT … FOR UPDATE` inside a transaction. Two concurrent status writes for the same `(user, book)` serialize cleanly. Not directly relevant here (status is out of scope per §9), but noted for completeness.

---

## 4. Recommended architecture — push model, progress-only

```
User opens BookOrbit dashboard (browser)
  │
  ▼
DashboardScroller.vue:28 (useDashboardScroller.load)
  │ issues GET /api/v1/dashboard/scrollers/:type?...
  ▼
DashboardController.getScroller                                   [block A: webhook fan-out]
  │ BookOrbit-side injected service "BridgeTrigger"
  │   await fetch('http://127.0.0.1:<BRIDGE_PORT>/sync-now', {
  │     method: 'POST', headers: { 'x-bridge-secret': '<shared>' },
  │     body: { userId }, signal: AbortSignal.timeout(2500ms),
  │   })
  │     ↳ 2xx → bridge committed its BulkProgress; bridge stays "running" — no further action taken by BookOrbit
  │     ↳ non-2xx / timeout / ECONNREFUSED → log WARN, swallow, proceed (dashboard never blocks on bridge failure)
  │
  │ (if webhook 2xx) call clearForScope on liveCache + staleCache for userId   [block B: invalidate widget cache]
  │
  ▼
dashboardService.getScroller → loadCardsByIds → findCardsByBookIds   [block C: fresh DB read]
  │ returns the cards with the freshly-pushed progress already committed
  ▼
HTTP response → Vue renders the dashboard scroller
  │
  │ (bridge's BulkProgress already fired achievement_events.emit('book.progress-changed'))
  │ → scan.gateway → server.to(user:${userId}).emit('book:progress-changed')
  │ → useBookEvents 'book:progress-changed' listener fires
  │ → useBookProgressRefresh debounces (250ms) and re-calls load()
  │ → second refresh, which is now a no-op (data already current — useful only if a
  │   koreader/push happened *after* block C ran but within the same dashboard session)
  ▼
Dashboard is up-to-date on the first response.
```

The three blocks above correspond to three concrete change areas.

### Block A — webhook fan-out on BookOrbit, awaited before scroller query
The injected `BridgeTrigger` service wraps `fetch`. The controller method `getScroller` becomes:
```ts
async getScroller(type, ..., user) {
  await this.bridgeTrigger.trigger(user.id);    // awaited; bounded by AbortSignal.timeout(2500ms)
  return this.dashboardService.getScroller(type, user, limit, smartScopeId);
}
```
The service itself is small and *never throws out* — it logs `WARN` and returns `void` on any failure, so a missing/offline bridge cannot break the dashboard. (See failure modes in §6.)

### Block B — widget-cache invalidation (precedes the scroller query, follows bridge success)
When the bridge webhook returns 2xx, BookOrbit calls `dashboardWidgetService.invalidateUserCache(userId)` — a thin method wrapping `this.liveCache.clearForScope(String(userId))` + `this.staleCache.clearForScope(String(userId))`. The precedent is `user-statistics.service.ts:394`'s `this.cache.clearForScope(String(user.id))`. This is the *extra* step beyond scroller-row freshness: it ensures the widget row (`currently-reading`, `reading-streak`, etc.) reflects the new commit too, instead of serving 120s/300s of stale data.

This invalidation must run *only on bridge success*, never on bridge failure (otherwise a down bridge would make the dashboard serve 100% cache misses every poll, killing the server).

### Block C — Vue/client changes: none required
The push model already returns fresh cards; `useBookProgressRefresh` continues to fire on subsequent koreader-pushed events via the existing Socket.IO chain. No new browser code is needed for the *first-load visibility* constraint. (A nice future extra would be to show a "sync in progress" badge if `bridgeTrigger.inFlight()` returns true; out of scope here.)

---

## 5. First-load visibility — three paths, three requirements

| Row on dashboard | Cache? | Required mechanism for first-load visibility |
|---|---|---|
| Scrollers (`continue-reading`, `recently-added`, `random`, smart-scope) | **No server cache** | Block A alone is sufficient: the bridge's `BulkProgress` commit completes before `findCardsByBookIds` runs. |
| Widgets (`currently-reading`, `reading-streak`, library overview, monthly challenge, etc.) | **`liveCache` 120s / `staleCache` 300s** in `dashboard-widget.service.ts:36-43` | Block A + Block B: after a 2xx from the bridge, BookOrbit calls `clearForScope(userId)` on both caches so the widget queries re-fire fresh loaders instead of returning cached rows. |
| Subsequent koreader pushes (after page load) | N/A — handled by existing Socket.IO + `useBookProgressRefresh` 250ms debounced reload | Already wired per §3.1. No new work. |

**Edge case:** if Block A times out (bridge slow / unreachable) but the cached widget row is available, BookOrbit *must not call* `clearForScope` (Block B is conditioned on Block A 2xx). The dashboard renders whatever it had pre-bridge; the operator sees the same first-load they'd get without this integration. This is the failure isolation described in §6.

---

## 6. Failure modes / degradation

| Failure | The dashboard's behavior | Trade-off |
|---|---|---|
| Bridge unreachable (process down, port not listening, firewall) | `fetch` throws `ECONNREFUSED`; `BridgeTrigger` logs `WARN bridge-unreachable` and resolves `void`; controller proceeds to `getScroller` with no degraded path (just no fresh data) | Zero regression from current behavior (no integration = bridge polls in background anyway). |
| Bridge slow (>2.5s) | `AbortSignal.timeout(2500ms)` aborts the request; `BridgeTrigger` logs `WARN bridge-timeout`; proceeds | Dashboard response is bounded to ~timeout + DB query time. Worst case: bridge made a *partial* progress commit and timed out — `BulkProgress` is batched; a partial batch *may* have committed some books already. Eventually consistent on the next bridge poll. |
| Bridge returns non-2xx (e.g., it crashed mid-`RunOnce`) | `BridgeTrigger` logs `WARN bridge-failed status=<n>`; proceeds | Same as slow. |
| BookOrbit server is restarted while bridge is mid-call | Bridge's `fetch` returns 5xx or `ECONNRESET`; `BridgeTrigger` swallows it | Dashboard continues (it had nothing invalidated yet). Next dashboard load will hit a warmed bridge. |
| Cache invalidated by Block B, then scroller query fails | The spec is "Block B only after Block A 2xx", and Block A 2xx commits the bridge write before Block B runs. Scroller query failure at this point is a BookOrbit-side bug, not caused by the integration. | Pre-existing concern, not a new failure mode introduced here. |
| User is actively reading on BookOrbit while bridge pushes | Progress: `INSERT … ON CONFLICT DO UPDATE` last-writer-wins (`koreader.repository.ts:435-467, 536-562`); bridge's `deviceId` is distinct from `bookorbit-web`, so no false bumping of `updatedAt`. Status (out of scope here): `SELECT … FOR UPDATE` serializes. | Documented in §3.7. |

**Hard rule:** the integration cannot *regress* the dashboard. A failed bridge must look identical to "no integration configured." The `BridgeTrigger` service must never rethrow.

---

## 7. Security

- **Webhook binding is loopback only** (`http://127.0.0.1:<port>/sync-now`). The bridge already runs as a daemon and binds its HTTP listener (a new small `internal/api` package) to `127.0.0.1`, not `0.0.0.0`. No external exposure by default.
- **Shared-secret auth.** Bridge accepts `x-bridge-secret` header; BookOrbit sends the value from a typed config (per BookOrbit's `AGENTS.md`, use `registerAs()` config, never `process.env` directly; the bridge equivalently uses its existing `internal/config` package, sourced from `bridge.yaml` or `BRIDGE_*` env).
- The bridge **never accepts BookOrbit credentials** on the webhook (it already has them in its own config). The webhook only carries `{ userId }` so the bridge knows which BookOrbit user the incoming request is for (in case the operator runs BookOrbit multi-user — `bookorbit-book_sync_state` data is user-scoped).
- **No request body parsing complexity.** One small JSON payload `{"userId": <int>}`. The bridge doesn't echo anything sensitive back; the response is `{ ok: true }` or an HTTP error status.
- **Timeout budget:** BookOrbit's `AbortSignal.timeout(2500ms)` caps the *outbound* call. Internally, the bridge's `Engine.RunOnce` honors a `context.WithTimeout(ctx, 2*s)` so a hang inside the bridge (network slow on Readest, deadlock, etc.) is bounded by 2s — slightly less than BookOrbit's 2.5s budget to leave headroom for the HTTP round trip and BookOrbit's Block B.

---

## 8. Bridge-side change shape (proposed only, not implemented here)

Add a tiny package `internal/api` (loopback HTTP only) and a new field/CLI flag:

- `cmd/bridge/main.go` — start the small `http.Server` on `127.0.0.1:<port>`, default port e.g. `8765`, configurable via `bridge.webhook_listen` / `BRIDGE_WEBHOOK_LISTEN` and `bridge.webhook_secret` / `BRIDGE_WEBHOOK_SECRET`. The listener must bind `127.0.0.1` *only*; if the operator wants Docker-network exposure, that's an explicit future opt-in with separate hardening.
- A route `POST /sync-now` guarded by `x-bridge-secret`. The handler:
  1. parses JSON body `{ userId: int }`;
  2. reads a 2s timeout `context.WithTimeout`;
  3. calls `engine.RunOnce(ctx)` (the exact same function the poll loop and `--once` call — no duplication of sync logic);
  4. returns `200 {"ok":true}` on success, `503` on `(context.DeadlineExceeded | sync failure joined error)`, `500` on an unrecoverable bridge error.
- The poll loop (daemon mode) continues to exist unchanged. The webhook is plain "kick `RunOnce` now" — it shares the `Engine` instance and its in-memory `bulkUnsupported` flag.

Cooperation note: if the poll loop and a webhook hit `RunOnce` near-simultaneously, the engine is single-goroutine per Phase 6 §10. A simple internal `sync.Mutex` around `RunOnce` (or a small queue) makes the second call wait for the first to finish, which is correct (they'd race to write the same `koreader_device_progress` rows anyway; serializing them is cheaper than letting them collide on `ON CONFLICT`).

No business logic change inside `internal/readest`, `internal/bookorbit`, or `internal/sync` — the webhook is purely a *new caller* of the existing `Engine.RunOnce`.

---

## 9. Status sync is out of scope here

Status sync (the `book.status-changed` event pathway described in `docs/future-status-sync.md`, sequenced ahead in `docs/status-sync-design-investigation.md`) is out of scope for the dashboard-trigger, for two reasons evidenced in §3.6:

1. The existing real-time chain (`book.progress-changed`) is fully wired from server to Vue. The `book.status-changed` event has zero Socket.IO subscribers today, so a status write would *not* trigger a dashboard refresh via the existing path.
2. Status sync is its own undecidable set of design questions (see `docs/status-sync-design-investigation.md` §4 Decisions A–F, separate task) and is gated on the operator's approval (Decision F: opt-in flag, default-off).

When/if status sync is approved and built, the dashboard-trigger can be extended to also kick the bridge's status step. BookOrbit would then need a small new gateway subscription on `book.status-changed` (mirroring `scan.gateway.ts:46-52` for `book.progress-changed`), and `BridgeTrigger` would still just call `/sync-now` — the engine would internally know to push both progress and status per the approved configuration. This document treats that as a clean follow-on, not a complication.

---

## 10. Verdict

**Feasible, precedented, additive.**

- The existing real-time chain (§3.1) already covers the "live push, dashboard refreshes" case; this integration just triggers it from a new caller.
- Scroller-row first-load freshness (§3.2) is achievable purely by ordering the bridge commit before the scroller query — Block A alone.
- Widget-row first-load freshness (§3.3) requires also invalidating the two caches after a successful bridge push (Block B). This is precedent-backed by `user-statistics.service.ts:394`'s `clearForScope` pattern.
- The webhook-with-custom-guard pattern (§3.4) is well established via `KoreaderAuthGuard` and `KoboTokenGuard`; adding a static-secret loopback variant is a small new variant on an existing shape.
- Failure isolation (§6) keeps the dashboard non-regressing — a down bridge is a no-op, not a hang.
- Bridge-side work is a new tiny `internal/api` package and a config field — no business-logic change.

**Three concrete blockers the operator/team should confirm before implementation:**
1. **Block B (widget cache invalidation) is a BookOrbit-side change**, not a bridge change. BookOrbit must add a `BridgeTrigger` service and wire its `dashboard.controller.ts:16-24` controller plus a new `invalidateUserCache` method. This repo cannot make that change; the feasibility doc records what is *needed*, not what is done.
2. **Block A (webhook fan-out) imposes a ~2–3s add-on dashboard response budget** when the bridge is healthy. If the operator's bridge poll interval is already short (e.g., 15min), this 2s is paid only on dashboard *loads*, not every poll. Acceptable for casual reading cadences; might bother download-on-mobile users — worth a quick load-time budget conversation.
3. **Bridge must keep its `device`/`deviceId` distinct from `bookorbit-web`** (`koreader.repository.ts:559`'s deliberate `updatedAt` preservation in `ON CONFLICT`). Already the case per the shipped bridge (`internal/sync/state/state.go:39` and PHASE 6 ADR Addendum).

---

## 11. Open questions (not resolved here — flagged for the operator)

1. **Where does the bridge listen?** Loopback only (recommended), or a UNIX domain socket (cleaner privilege separation but more plumbing on BookOrbit's fetch side)?
2. **Should Block A also be wired into the per-widget `@Get('widgets/:type')` routes**, not just the scroller route? If widgets refresh on Socket.IO (which they don't today) there's no need; if they don't, then a user who never scrolls but only looks at widgets would never trigger the bridge. Trade-off between "trigger on every dashboard-related endpoint" vs "trigger on the scroller endpoint only" is a product call, not a feasibility one.
3. **Multi-user BookOrbit deployments** — the bridge only knows about *one* Readest account, but BookOrbit may have many users. The webhook's `{ userId }` lets BookOrbit tell the bridge "this is for user N", which today is a no-op (the bridge syncs its one account to BookOrbit regardless of target user). If the operator runs multi-user BookOrbit, the bridge would need a per-BookOrbit-user config slot, or BookOrbit needs to constrain the trigger to only one configured user. Real edge case for self-hosted single-operator; real design question for shared deployments.

---

*Prepared from source inspection of `reference/bookorbit/server/` and `reference/bookorbit/client/src/`.*
