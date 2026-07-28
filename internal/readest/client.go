package readest

import (
	"context"
	"log/slog"
	"time"

	"github.com/user/bookorbit-readest-sync/internal/util/httpclient"
)

// SyncClient pulls the Readest library. The sync engine consumes only this
// interface, so the real HTTP client (Phase 4) and test fakes are
// interchangeable.
type SyncClient interface {
	// PullBooks returns every book row changed since the given watermark, in
	// epoch milliseconds. A since of 0 requests the full library.
	PullBooks(ctx context.Context, since int64) ([]BookRow, error)
}

// Client is the foundation-phase stub SyncClient. It carries the dependencies
// the Phase 4 implementation will need, but PullBooks returns
// ErrNotImplemented so the foundation ships with no network behavior.
type Client struct {
	baseURL string
	auth    Authenticator
	http    httpclient.Doer
	log     *slog.Logger
	timeout time.Duration
}

// NewClient constructs the stub sync client, capturing its dependencies for
// the Phase 4 implementation.
func NewClient(baseURL string, auth Authenticator, hc httpclient.Doer, log *slog.Logger, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		auth:    auth,
		http:    hc,
		log:     log,
		timeout: timeout,
	}
}

// PullBooks is a stub; the GET /sync?type=books call is implemented in Phase 4.
func (c *Client) PullBooks(ctx context.Context, since int64) ([]BookRow, error) {
	return nil, ErrNotImplemented
}
