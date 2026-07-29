Phase 5 is implemented exactly per the approved design. 

## What changed

**`internal/bookorbit/models.go`**

- `MatchCandidate` gained `MetadataAmbiguous bool` (§4.2a).

- `MatchCheckRequest` gained the device wrapper (`DeviceID`/`DeviceModel`/`PluginVersion`/`DeviceTime`) plus a `WithMatchCheck` convenience constructor, mirroring the existing `WithDevice` pattern (§4.2b).

- `BulkProgressResponse.Updated` removed, replaced with `Unmatched []string` — the only field any reference call site reads (§4.2c).

- New `UpdateProgressRequest` + `WithUpdateProgress`, using the kosync snake_case-ish shape, deliberately *not* device-wrapped (§4.2d).

**`internal/bookorbit/client.go`** — full rewrite of the stub:

- `API.BulkProgress` now returns `(BulkProgressResponse, error)`; `UpdateProgress` added to the interface.

- `NewClient` gained `timeout time.Duration`, applied per-call via `context.WithTimeout` (mirrors `readest.Client`'s Phase 4 precedent). `now func() time.Time` is an internal field (default `time.Now`), not a constructor param, same convention as `Auth.now`.

- `MatchCheck`/`BulkProgress`/`UpdateProgress` all **overwrite** the device-identity fields on the request immediately before encoding, ignoring whatever the caller set — reproducing the plugin's stamp-at-dispatch behavior and eliminating staleness across a batched engine loop.

- `Hashes`/`Books`/`Items` are normalized to non-nil empty slices before marshaling so they always encode as `[]`, never `null`.

- Body-size check (`maxBody`) happens after encoding, before any HTTP call — `ErrBodyTooLarge` is returned with zero requests dispatched.

- One error classifier (`classifyErrorResponse`) used uniformly across all four endpoints: 401/403→`ErrUnauthorized`, 400→`ErrBadRequest`, 404/405→`ErrUnsupportedEndpoint`, 429→`ErrRateLimited`, 5xx→`ErrServer`, else→`ErrNetwork` (unclassified = transient, engine's call). Transport errors map to `ErrNetwork` except `context.Canceled`/`DeadlineExceeded`, which propagate untouched.

- No token lifecycle, no retries, no batching, no `state.Store` access — the client sends exactly what it's given, once.

**`internal/bookorbit/models_test.go` / `client_test.go`** — removed the obsolete `TestClientStubReturnsNotImplemented`; added coverage for device-field stamping/overriding, `MetadataAmbiguous` round-tripping, empty-array encoding, all four endpoints' error classification, body-too-large short-circuiting, per-call deadlines, and a `-race` concurrency check (the client holds no mutable state, so this is a cheap guarantee to pin).

**`doc.go`** — updated to drop the "stub" language now that the package is implemented.

## Edge cases worth double-checking against a live server (already flagged in the design's §16, not resolved by this phase)

- Whether a real bulk-progress response ever carries fields beyond `unmatched`.

- Exact status code an older BookOrbit server returns for an unsupported endpoint (confirms 404/405 is the right `ErrUnsupportedEndpoint` trigger for Phase 6).

- Whether empty `progress` strings are actually accepted by `bulkProgress`/`updateProgress`.
