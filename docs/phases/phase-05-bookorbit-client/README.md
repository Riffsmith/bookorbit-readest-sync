# Phase 5 — BookOrbit Client

**Status:** Implemented and accepted. This is the **authoritative** design doc (`design.md`'s own header) — Phase 5 was built in lock-step with its design and never deviated.
**Design:** [design.md](./design.md) — *(original filename `phase-5-design-temp.md`; the `-temp` suffix was a leftover from authoring and has been dropped at reorganization time)*
**Decision record (ADR):** [decision-record.md](./decision-record.md)
**Implemented in:** [`internal/bookorbit/client.go`](../../../internal/bookorbit/client.go), [`internal/bookorbit/models.go`](../../../internal/bookorbit/models.go), [`internal/bookorbit/client_test.go`](../../../internal/bookorbit/client_test.go), [`internal/bookorbit/models_test.go`](../../../internal/bookorbit/models_test.go)
**Depends on:** Phase 1 config (`Config.AuthKey()`, `util.NormalizeBookOrbitURL`), Phase 2 state, [Phase 4 Readest client](../phase-04-readest-client/README.md)'s "client-faithful, engine-selective" boundary precedent.
**Unblocked by this phase:** [Phase 6 — Sync Engine](../phase-06-engine/README.md)

## What this phase shipped

The BookOrbit-side REST client: four endpoints (`Auth`, `MatchCheck`, `BulkProgress`, `UpdateProgress`), error taxonomy (`ErrUnauthorized`/`ErrBadRequest`/`ErrUnsupportedEndpoint`/`ErrRateLimited`/`ErrServer`/`ErrNetwork`/`ErrMalformedResponse`/`ErrBodyTooLarge`), server-URL normalization, request body size limit (900 KiB), per-call deadlines, and the device-field stamp-at-dispatch pattern (overwrites `deviceId`/`deviceModel`/`pluginVersion`/`deviceTime` immediately before encoding to eliminate staleness across a batched engine loop). The `BulkProgressResponse.Unmatched []string` is the only field the engine consumes; the `Updated` field the design originally proposed was removed (the only field any reference call site reads is `unmatched`).

## What shipped DIFFERENTLY from the design

- `MatchCandidate` gained `MetadataAmbiguous bool` (§4.2a).
- `MatchCheckRequest` gained the device wrapper (`WithMatchCheck` constructor) mirroring `WithDevice`'s pattern (§4.2b).
- `BulkProgressResponse.Updated` removed; replaced with `Unmatched []string` (§4.2c).
- New `UpdateProgressRequest` + `WithUpdateProgress`, deliberately *not* device-wrapped (§4.2d).

All present in the design as §4.2 sub-items; live-server validation in Phase 6 confirmed the request shapes match the BookOrbit server.

## Live-validation items this phase opened

- Exact status code an older BookOrbit server returns for an unsupported endpoint (404/405 → `ErrUnsupportedEndpoint`, confirmed Phases 6–10 never observed this path).
- Whether empty `progress` strings are actually accepted by `bulkProgress`/`updateProgress` — **confirmed accepted** by Phase 8 Tests 2 and 4 (see [`../../live-validation-status.md`](../../live-validation-status.md) row L8/empty-progress).

## Tests

[`internal/bookorbit/client_test.go`](../../../internal/bookorbit/client_test.go): full status matrix per endpoint (400/401/403/404/405/429/500/502/503), body-too-large short-circuit, per-call deadlines, `-race` concurrency, malformed JSON handling. [`internal/bookorbit/models_test.go`](../../../internal/bookorbit/models_test.go): wire-shape round-trips (`Books` encodes as `[]` not `null`, `MetadataAmbiguous` round-trips device-wrapper keys at top level).
