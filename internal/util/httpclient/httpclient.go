// Package httpclient defines the narrow seam between the bridge's API clients
// and the HTTP transport. Depending on the small Doer interface rather than
// *http.Client lets tests substitute a fake transport without spinning up a
// server, and gives Phases 3–5 a single place to hang timeout, retry, and
// backoff policy.
package httpclient

import (
	"net/http"
	"time"
)

// Doer is the minimal behavior the API clients need from an HTTP transport:
// execute one request and return its response. *http.Client satisfies it, as
// does any test double.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// New returns a Doer backed by a standard library http.Client with the given
// per-request timeout. A timeout <= 0 falls back to a sane default so a
// misconfigured zero value cannot hang a request forever.
func New(timeout time.Duration) Doer {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}
