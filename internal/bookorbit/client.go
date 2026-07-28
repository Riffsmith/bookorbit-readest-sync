package bookorbit

import (
	"context"
	"errors"
	"log/slog"

	"github.com/user/bookorbit-readest-sync/internal/util/httpclient"
)

// ErrNotImplemented marks stub methods whose real implementation lands in
// Phase 5 of the roadmap.
var ErrNotImplemented = errors.New("bookorbit: not implemented in foundation phase")

// API is the BookOrbit surface the sync engine depends on. The concrete client
// (Phase 5) and test fakes both satisfy it.
type API interface {
	// Auth validates the configured credentials against the server; it doubles
	// as a startup health check.
	Auth(ctx context.Context) error
	// MatchCheck resolves a set of hashes to BookOrbit library entries.
	MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error)
	// BulkProgress uploads a batch of progress items in a single request.
	BulkProgress(ctx context.Context, req BulkProgressRequest) error
}

// Client is the foundation-phase stub API. It captures the dependencies the
// Phase 5 implementation will need, but every method returns
// ErrNotImplemented so the foundation ships with no network behavior.
type Client struct {
	baseURL  string
	username string
	authKey  string
	device   DeviceInfo
	http     httpclient.Doer
	log      *slog.Logger
	maxBody  int64
}

// NewClient constructs the stub BookOrbit client. authKey is the value for the
// `x-auth-key` header (the lowercase hex MD5 of the password, or a pre-hashed
// userkey). The baseURL is expected to already be normalized to include the
// "/api/v1" prefix exactly once.
func NewClient(baseURL, username, authKey string, device DeviceInfo, hc httpclient.Doer, log *slog.Logger, maxBody int64) *Client {
	return &Client{
		baseURL:  baseURL,
		username: username,
		authKey:  authKey,
		device:   device,
		http:     hc,
		log:      log,
		maxBody:  maxBody,
	}
}

// Auth is a stub; the credential health check is implemented in Phase 5.
func (c *Client) Auth(ctx context.Context) error {
	return ErrNotImplemented
}

// MatchCheck is a stub; the match-check call is implemented in Phase 5.
func (c *Client) MatchCheck(ctx context.Context, req MatchCheckRequest) (MatchCheckResponse, error) {
	return MatchCheckResponse{}, ErrNotImplemented
}

// BulkProgress is a stub; the bulk upload call is implemented in Phase 5.
func (c *Client) BulkProgress(ctx context.Context, req BulkProgressRequest) error {
	return ErrNotImplemented
}
