package bookorbit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/util/httpclient"
)

// maxErrorBodyBytes bounds how much of a non-2xx response body is read into a
// wrapped error message, matching the bound already established in
// internal/readest (auth.go, client.go).
const maxErrorBodyBytes = 512

// maxResponseBodyBytes bounds how much of a 2xx response body is read into
// memory. Every response this client decodes (auth check, match-check,
// bulk-progress, the singular PUT) is a small acknowledgment, never a full
// library page, so this cap is far smaller than the Readest pull client's
// (4 MiB) — 256 KiB is generous headroom while still guarding against a
// misbehaving server flooding memory.
const maxResponseBodyBytes = 256 << 10 // 256 KiB

// Sentinel errors for the BookOrbit client. Following the pattern already
// established in internal/readest (Phase 4): each is errors.Is-matchable and
// wrapped via %w with status + a bounded body snippet for diagnostics.
var (
	// ErrUnauthorized is a 401/403 response. Unlike the Readest client, there
	// is no re-auth-and-retry dance here: BookOrbit authentication is static
	// (x-auth-user/x-auth-key, no expiry), so a 401/403 means the configured
	// credentials are wrong and is terminal for the call.
	ErrUnauthorized = errors.New("bookorbit: unauthorized")
	// ErrBadRequest is a 400 response: a client-side bug, non-retryable.
	ErrBadRequest = errors.New("bookorbit: bad request")
	// ErrUnsupportedEndpoint is a 404/405 response, distinct from
	// ErrBadRequest so Phase 6 can key a bulk-endpoint-unsupported fallback
	// decision off it without conflating it with a generic client bug.
	ErrUnsupportedEndpoint = errors.New("bookorbit: endpoint not supported")
	// ErrRateLimited is a 429 response. Not documented for BookOrbit's
	// self-hosted servers, but handled defensively; retryable by the engine.
	ErrRateLimited = errors.New("bookorbit: rate limited")
	// ErrServer is a 5xx response; retryable by the engine.
	ErrServer = errors.New("bookorbit: server error")
	// ErrNetwork is a transport-level failure (DNS, connection, timeout);
	// retryable by the engine.
	ErrNetwork = errors.New("bookorbit: network error")
	// ErrMalformedResponse is a 2xx response whose body cannot be decoded;
	// non-retryable.
	ErrMalformedResponse = errors.New("bookorbit: malformed response")
	// ErrBodyTooLarge is a client-side check: the encoded request body exceeds
	// maxBody. The request is never dispatched. It is deliberately
	// non-retryable — a request that is too large stays too large on retry,
	// unlike the reference plugin's own "body_too_large" string error, which
	// its caller-side isTransportError classifies (apparently by accident) as
	// transport-retryable.
	ErrBodyTooLarge = errors.New("bookorbit: request body too large")
)

// API is the BookOrbit surface the sync engine depends on.
type API interface {
	// Auth validates the configured credentials against the server; it doubles
	// as a startup health check.
	Auth(ctx context.Context) error
	// MatchCheck resolves a set of hashes to BookOrbit library entries.
	MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error)
	// BulkProgress uploads a batch of progress items in a single request.
	BulkProgress(ctx context.Context, req BulkProgressRequest) (BulkProgressResponse, error)
	// UpdateProgress pushes a single book's progress via the kosync-compatible
	// PUT endpoint. It exists as a version-compatibility fallback for servers
	// that do not support the bulk endpoint; deciding when to use it instead
	// of BulkProgress is the sync engine's job (Phase 6), not this client's.
	UpdateProgress(ctx context.Context, req UpdateProgressRequest) error
}

// Client is the concrete BookOrbit API client. It turns domain-level sync
// facts into BookOrbit HTTP calls and BookOrbit's JSON responses into typed Go
// values or classified sentinel errors. Authentication is static (no token
// lifecycle); the client holds no cache and performs no retries or batching —
// those are the sync engine's responsibility. Every field is immutable after
// construction (now is a function value, not shared mutable state), so Client
// is safe for concurrent use by construction.
type Client struct {
	baseURL  string
	username string
	authKey  string
	device   DeviceInfo
	http     httpclient.Doer
	log      *slog.Logger
	maxBody  int64
	timeout  time.Duration
	// now returns the current wall-clock time; injected for deterministic
	// tests of the DeviceTime freshness-stamping behavior. Defaults to
	// time.Now, mirroring internal/readest's Auth.now convention.
	now func() time.Time
}

// NewClient constructs the BookOrbit client. authKey is the value for the
// `x-auth-key` header (the lowercase hex MD5 of the password, or a pre-hashed
// userkey — already derived by config.Config.AuthKey()). baseURL is expected
// to already be normalized to include the "/api/v1" prefix exactly once
// (config.finalize() already does this via util.NormalizeBookOrbitURL). A nil
// logger falls back to slog.Default(). timeout is applied as a per-call
// deadline via context.WithTimeout on every exported method when > 0;
// <= 0 disables the per-call deadline and defers entirely to hc's own timeout.
func NewClient(baseURL, username, authKey string, device DeviceInfo, hc httpclient.Doer, log *slog.Logger, maxBody int64, timeout time.Duration) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		baseURL:  baseURL,
		username: username,
		authKey:  authKey,
		device:   device,
		http:     hc,
		log:      log,
		maxBody:  maxBody,
		timeout:  timeout,
		now:      time.Now,
	}
}

// Auth validates the configured credentials against GET
// /koreader/users/auth. The plugin's own health check (testConnection) treats
// any non-error body as success and does not read a specific field, so this
// method reports only success/failure.
func (c *Client) Auth(ctx context.Context) error {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := c.newRequest(ctx, http.MethodGet, "/koreader/users/auth", nil)
	if err != nil {
		return err
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.classifyErrorResponse(resp, http.MethodGet, "/koreader/users/auth")
	}
	return nil
}

// MatchCheck resolves a set of hashes to BookOrbit library entries via POST
// /koreader/plugin/match-check. It sends exactly the hashes/candidates it is
// given, in one HTTP round trip; batching into chunks of the configured
// MatchBatchSize is the sync engine's job.
//
// The device wrapper fields on req (DeviceID/DeviceModel/PluginVersion/
// DeviceTime) are overwritten with the client's own static device identity and
// the current time immediately before encoding, regardless of what the caller
// populated — this reproduces the reference plugin's "stamp fresh at dispatch
// time" behavior and removes any staleness risk across a batched engine loop.
func (c *Client) MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	req = c.stampMatchCheck(req)
	body, err := c.encodeAndCheckSize(req)
	if err != nil {
		return MatchCheckResponse{}, err
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/koreader/plugin/match-check", bytes.NewReader(body))
	if err != nil {
		return MatchCheckResponse{}, err
	}

	resp, err := c.do(httpReq)
	if err != nil {
		return MatchCheckResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return MatchCheckResponse{}, c.classifyErrorResponse(resp, http.MethodPost, "/koreader/plugin/match-check")
	}

	raw, err := readBoundedBody(resp.Body)
	if err != nil {
		return MatchCheckResponse{}, err
	}
	var out MatchCheckResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return MatchCheckResponse{}, fmt.Errorf("%w: decode match-check: %v", ErrMalformedResponse, err)
	}
	if out.Matches == nil {
		out.Matches = []Match{}
	}
	if out.Unmatched == nil {
		out.Unmatched = []string{}
	}
	return out, nil
}

// BulkProgress uploads a batch of progress items via POST
// /koreader/plugin/progress in one HTTP round trip. It sends exactly the items
// it is given; chunking into the configured ProgressBatchSize is the sync
// engine's job.
//
// The device wrapper fields on req are overwritten immediately before
// encoding, for the same reason documented on MatchCheck.
func (c *Client) BulkProgress(ctx context.Context, req BulkProgressRequest) (BulkProgressResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	req = c.stampBulkProgress(req)
	body, err := c.encodeAndCheckSize(req)
	if err != nil {
		return BulkProgressResponse{}, err
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/koreader/plugin/progress", bytes.NewReader(body))
	if err != nil {
		return BulkProgressResponse{}, err
	}

	resp, err := c.do(httpReq)
	if err != nil {
		return BulkProgressResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BulkProgressResponse{}, c.classifyErrorResponse(resp, http.MethodPost, "/koreader/plugin/progress")
	}

	raw, err := readBoundedBody(resp.Body)
	if err != nil {
		return BulkProgressResponse{}, err
	}
	var out BulkProgressResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return BulkProgressResponse{}, fmt.Errorf("%w: decode bulk-progress: %v", ErrMalformedResponse, err)
	}
	if out.Unmatched == nil {
		out.Unmatched = []string{}
	}
	return out, nil
}

// UpdateProgress pushes a single book's progress via PUT
// /koreader/syncs/progress, the kosync-compatible fallback endpoint. It only
// makes the endpoint callable faithfully; deciding whether/when to use it
// instead of BulkProgress is the sync engine's job (Phase 6), not implemented
// here.
//
// Device/DeviceID are overwritten from the client's static device identity
// immediately before encoding, overriding any caller-supplied values.
// Timestamp is passed through unmodified: it is domain data (the reading
// moment being reported), not a freshness field, and is never stamped by the
// client.
func (c *Client) UpdateProgress(ctx context.Context, req UpdateProgressRequest) error {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	req.Device = c.device.DeviceModel
	req.DeviceID = c.device.DeviceID

	body, err := c.encodeAndCheckSize(req)
	if err != nil {
		return err
	}

	httpReq, err := c.newRequest(ctx, http.MethodPut, "/koreader/syncs/progress", bytes.NewReader(body))
	if err != nil {
		return err
	}

	resp, err := c.do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.classifyErrorResponse(resp, http.MethodPut, "/koreader/syncs/progress")
	}
	return nil
}

// stampMatchCheck returns a copy of req with the device wrapper fields
// overwritten from c.device/c.now(), and Hashes/Books normalized to non-nil
// empty slices so they always encode as "[]", never "null" (the BookOrbit
// backend rejects an empty object where it expects an array).
func (c *Client) stampMatchCheck(req MatchCheckRequest) MatchCheckRequest {
	req.DeviceID = c.device.DeviceID
	req.DeviceModel = c.device.DeviceModel
	req.PluginVersion = c.device.PluginVersion
	req.DeviceTime = c.now().Format(DeviceTimeFormat)
	if req.Hashes == nil {
		req.Hashes = []string{}
	}
	if req.Books == nil {
		req.Books = []MatchCandidate{}
	}
	return req
}

// stampBulkProgress returns a copy of req with the device wrapper fields
// overwritten from c.device/c.now(), and Items normalized to a non-nil empty
// slice for the same reason as stampMatchCheck.
func (c *Client) stampBulkProgress(req BulkProgressRequest) BulkProgressRequest {
	req.DeviceID = c.device.DeviceID
	req.DeviceModel = c.device.DeviceModel
	req.PluginVersion = c.device.PluginVersion
	req.DeviceTime = c.now().Format(DeviceTimeFormat)
	if req.Items == nil {
		req.Items = []ProgressItem{}
	}
	return req
}

// encodeAndCheckSize marshals v to JSON and enforces the client-side body-size
// cap (c.maxBody) after encoding but before any request is built or
// dispatched, mirroring the reference plugin's exact ordering in
// bookorbit_api.lua:requestBlocking (encode, check length, only then send).
// A cap <= 0 disables the check.
func (c *Client) encodeAndCheckSize(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("bookorbit: encode request: %w", err)
	}
	if c.maxBody > 0 && int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d byte limit", ErrBodyTooLarge, len(body), c.maxBody)
	}
	return body, nil
}

// newRequest builds an HTTP request against the BookOrbit base URL with the
// required headers. body may be nil for a bodyless request (Auth's GET).
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("bookorbit: build request: %w", err)
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("x-auth-user", c.username)
	req.Header.Set("x-auth-key", c.authKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do executes the request, mapping a transport-level failure to ErrNetwork
// while letting context cancellation/deadline propagate transparently (so the
// engine's graceful shutdown sees context.Canceled/context.DeadlineExceeded
// cleanly, matching the precedent set in internal/readest.Client.pull).
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s %s: %v", ErrNetwork, req.Method, req.URL.Path, err)
	}
	return resp, nil
}

// withTimeout applies the client's per-call deadline to ctx when configured,
// mirroring internal/readest.Client.PullBooks. A timeout <= 0 returns ctx
// unchanged with a no-op cancel, deferring entirely to the Doer's own timeout.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout > 0 {
		return context.WithTimeout(ctx, c.timeout)
	}
	return ctx, func() {}
}

// classifyErrorResponse reads a bounded error body and maps a non-2xx status
// to the appropriate sentinel, attaching the status code and a truncated body
// for diagnostics. It is used uniformly across every endpoint in this
// package, matching the single-classifier pattern already established in
// internal/readest. method and path are passed explicitly (rather than read
// from resp.Request) so the classifier does not depend on a Doer test double
// populating that field.
func (c *Client) classifyErrorResponse(resp *http.Response, method, path string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))

	var sentinel error
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		sentinel = ErrUnauthorized
	case resp.StatusCode == http.StatusBadRequest:
		sentinel = ErrBadRequest
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		sentinel = ErrUnsupportedEndpoint
	case resp.StatusCode == http.StatusTooManyRequests:
		sentinel = ErrRateLimited
	case resp.StatusCode >= 500 && resp.StatusCode <= 599:
		sentinel = ErrServer
	default:
		// An unclassified status is treated as transient so the engine retries,
		// matching the fallback classification used in internal/readest.
		sentinel = ErrNetwork
	}
	return fmt.Errorf("%w: %s %s: status %d: %s",
		sentinel, method, path, resp.StatusCode, bytes.TrimSpace(body))
}

// readBoundedBody reads up to maxResponseBodyBytes+1 of a 2xx response body so
// an over-cap body is detected as malformed rather than silently truncated
// into a confusing JSON error, matching internal/readest.decodeBooks.
func readBoundedBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response body: %v", ErrMalformedResponse, err)
	}
	if len(raw) > maxResponseBodyBytes {
		return nil, fmt.Errorf("%w: response body exceeds %d bytes", ErrMalformedResponse, maxResponseBodyBytes)
	}
	return raw, nil
}
