package readest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/util/httpclient"
)

// maxResponseBodyBytes bounds how much of a pull response body is read into
// memory, so a hostile or buggy server cannot flood memory. A full since=0
// library is large but bounded; 4 MiB is a deliberately generous starting
// fence. This is a response-read cap, distinct from the BookOrbit request-body
// cap, and is tuned against a real pull (a live-validation item in the design).
const maxResponseBodyBytes = 4 << 20 // 4 MiB

// Sentinel errors for the sync client. They are deliberately distinct from the
// auth package's transport sentinels so the sync engine (Phase 6) can tell an
// authentication-lifecycle failure apart from a sync-API failure when applying
// its retry policy, even though both are matched with errors.Is. Each is
// returned wrapped with diagnostic detail (HTTP status, truncated body) via %w.
var (
	// ErrUnauthorized is a 401/403 (or an auth-error body) that persists after
	// the single re-auth-and-retry. It is terminal for that call.
	ErrUnauthorized = errors.New("readest: sync: unauthorized")
	// ErrBadRequest is a 400 response: a client-side bug, non-retryable.
	ErrBadRequest = errors.New("readest: sync: bad request")
	// ErrSyncRateLimited is a 429 response from the sync API. Not in the
	// documented status list, but the service can return it under repeated
	// polling; retryable by the engine. It is named distinctly from the auth
	// package's ErrRateLimited so the engine can tell a rate-limited sync call
	// apart from a rate-limited auth call (both live in this package).
	ErrSyncRateLimited = errors.New("readest: sync: rate limited")
	// ErrSyncServer is a 5xx response; retryable by the engine.
	ErrSyncServer = errors.New("readest: sync: server error")
	// ErrSyncNetwork is a transport-level failure (DNS, connection, timeout) on
	// the sync API; retryable by the engine.
	ErrSyncNetwork = errors.New("readest: sync: network error")
	// ErrSyncMalformed is a 2xx response whose body cannot be decoded into the
	// books envelope (bad JSON, or a wrong-shaped `books` field); non-retryable.
	ErrSyncMalformed = errors.New("readest: sync: malformed response")
)

// SyncClient pulls the Readest library. The sync engine consumes only this
// interface, so the real HTTP client and test fakes are interchangeable.
type SyncClient interface {
	// PullBooks returns every book row changed since the given watermark, in
	// epoch milliseconds. A since of 0 requests the full library.
	PullBooks(ctx context.Context, since int64) ([]BookRow, error)
}

// Client is the concrete SyncClient: it turns a millisecond cursor into a
// decoded page of book rows over HTTP. It owns building the request, attaching
// a valid Bearer credential obtained from Auth, classifying the response, and
// driving a single re-auth-and-retry after a downstream 401/403. It performs no
// backoff loop (that policy belongs to the engine) and is faithful to the
// window it is asked for: watermark math and row filtering stay with the
// engine. Its fields are immutable after construction, so it is safe for
// concurrent use; the one shared mutable resource (the token) is serialized
// downstream inside Auth.
type Client struct {
	baseURL string
	auth    Authenticator
	http    httpclient.Doer
	log     *slog.Logger
	timeout time.Duration
}

// NewClient constructs the sync client. baseURL is the Readest sync API base
// (e.g. https://web.readest.com/api); auth supplies and refreshes Bearer
// tokens; hc is the HTTP transport; log receives lifecycle events (nil falls
// back to slog.Default()); timeout is applied as a per-PullBooks deadline so a
// slow full-library pull cannot hang a sync pass (<= 0 disables the per-call
// deadline, deferring to the transport's own timeout).
func NewClient(baseURL string, auth Authenticator, hc httpclient.Doer, log *slog.Logger, timeout time.Duration) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		baseURL: baseURL,
		auth:    auth,
		http:    hc,
		log:     log,
		timeout: timeout,
	}
}

// PullBooks fetches the book rows changed since the given epoch-millisecond
// watermark from GET /sync?type=books. A since of 0 requests the full library.
// It returns the decoded rows for the window unfiltered — dummy and deleted
// rows are returned faithfully and left for the engine to act on.
//
// The request carries a Bearer token from Auth. On a 401/403 (or an auth-error
// body) it forces one token refresh and retries exactly once; a still-rejected
// retry yields ErrUnauthorized. Transient failures (network, 5xx, 429) are
// classified and returned without a client-side retry so the engine can apply
// its own backoff policy.
func (c *Client) PullBooks(ctx context.Context, since int64) ([]BookRow, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	token, err := c.auth.AccessToken(ctx)
	if err != nil {
		return nil, err
	}

	rows, retry, err := c.pull(ctx, since, token)
	if err == nil {
		return rows, nil
	}
	// Only an auth rejection triggers the single re-auth-and-retry. Every other
	// failure is already classified and propagates to the engine as-is.
	if !retry {
		return nil, err
	}

	c.log.Warn("readest: sync: token rejected by sync API; forcing refresh and retrying once", "since", since)
	token, ferr := c.auth.ForceRefresh(ctx)
	if ferr != nil {
		return nil, ferr
	}
	rows, _, err = c.pull(ctx, since, token)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// pull performs one GET /sync?type=books round trip with the given token. On
// success it returns the decoded rows. On failure it returns a classified
// sentinel error; the second return value reports whether that failure is an
// auth rejection that warrants a single re-auth-and-retry by the caller.
func (c *Client) pull(ctx context.Context, since int64, token string) (rows []BookRow, authRetry bool, err error) {
	req, err := c.buildRequest(ctx, since, token)
	if err != nil {
		return nil, false, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A context cancellation or deadline must propagate transparently (not be
		// masked as a generic network error) so the engine's graceful shutdown
		// sees context.Canceled / context.DeadlineExceeded cleanly.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("%w: pull books since %d: %v", ErrSyncNetwork, since, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		rows, err := decodeBooks(resp.Body)
		if err != nil {
			return nil, false, err
		}
		return rows, false, nil
	}

	// Read the error body once so it can serve both auth-failure detection and
	// the diagnostic detail attached to the returned error.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if isAuthFailure(resp.StatusCode, body) {
		return nil, true, fmt.Errorf("%w: pull books since %d: status %d: %s",
			ErrUnauthorized, since, resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil, false, classifySyncStatus(since, resp.StatusCode, body)
}

// buildRequest constructs the GET /sync?type=books request with its query and
// headers. It is separated from the round trip so the request shape (URL, query
// encoding, headers) is unit-testable without network I/O.
func (c *Client) buildRequest(ctx context.Context, since int64, token string) (*http.Request, error) {
	endpoint, err := url.Parse(c.baseURL + "/sync")
	if err != nil {
		return nil, fmt.Errorf("readest: sync: invalid base URL %q: %w", c.baseURL, err)
	}
	q := endpoint.Query()
	q.Set("type", "books")
	q.Set("since", fmt.Sprintf("%d", since))
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("readest: sync: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// decodeBooks reads the bounded response body and unmarshals the books
// envelope, returning the row slice. A 200 body that is not valid JSON, or
// whose `books` field is not an array, is a malformed response. Individual rows
// are never rejected here (BookRow tolerates odd progress/timestamp forms), so
// a single bad row cannot fail the whole page.
func decodeBooks(body io.Reader) ([]BookRow, error) {
	// Read one byte past the cap so an over-limit body is detected rather than
	// silently truncated into a JSON syntax error.
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read books body: %v", ErrSyncMalformed, err)
	}
	if len(raw) > maxResponseBodyBytes {
		return nil, fmt.Errorf("%w: books body exceeds %d bytes", ErrSyncMalformed, maxResponseBodyBytes)
	}

	var envelope BooksResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode books: %v", ErrSyncMalformed, err)
	}
	if envelope.Books == nil {
		// A null or absent `books` decodes to nil; normalize to an empty slice so
		// callers can rely on a usable value. A wrong type fails Unmarshal above.
		envelope.Books = []BookRow{}
	}
	return envelope.Books, nil
}

// isAuthFailure mirrors the reference plugin's downstream-auth detection: a
// 401/403 status, or a body carrying {"error":"Not authenticated"} regardless
// of status (readest_syncconfig.lua checks the status primarily and the body as
// a fallback for endpoints with differing shapes).
func isAuthFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return false
	}
	return e.Error == "Not authenticated"
}

// classifySyncStatus maps a non-200, non-auth sync response to the appropriate
// sentinel error, attaching the status code and a bounded body snippet for
// diagnostics. It is named distinctly from auth.go's classifyStatus (both live
// in this package).
func classifySyncStatus(since int64, status int, body []byte) error {
	var sentinel error
	switch {
	case status == http.StatusBadRequest:
		sentinel = ErrBadRequest
	case status == http.StatusTooManyRequests:
		sentinel = ErrSyncRateLimited
	case status >= 500 && status <= 599:
		sentinel = ErrSyncServer
	default:
		// An unclassified status is treated as transient so the engine retries.
		sentinel = ErrSyncNetwork
	}
	return fmt.Errorf("%w: pull books since %d: status %d: %s", sentinel, since, status, bytes.TrimSpace(body))
}
