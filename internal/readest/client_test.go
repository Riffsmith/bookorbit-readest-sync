package readest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuth is a scriptable Authenticator for client tests. AccessToken and
// ForceRefresh results can be set independently, and call counts are recorded
// so tests can assert exactly when each is invoked.
type fakeAuth struct {
	accessToken string
	accessErr   error
	forceToken  string
	forceErr    error
	accessCalls int32
	forceCalls  int32
}

func (f *fakeAuth) SignIn(ctx context.Context) (Token, error) { return Token{}, nil }
func (f *fakeAuth) Refresh(ctx context.Context) (Token, error) {
	return Token{}, nil
}
func (f *fakeAuth) AccessToken(ctx context.Context) (string, error) {
	atomic.AddInt32(&f.accessCalls, 1)
	return f.accessToken, f.accessErr
}
func (f *fakeAuth) ForceRefresh(ctx context.Context) (string, error) {
	atomic.AddInt32(&f.forceCalls, 1)
	return f.forceToken, f.forceErr
}

// newTestClient builds a Client wired to the given fake auth and stub doer with
// a discard logger and no per-call timeout.
func newTestClient(auth Authenticator, doer *stubDoer) *Client {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClient("https://sync.test/api", auth, doer, log, 0)
}

// booksBody renders a /sync?type=books response body with one row.
func booksBody(rows string) string {
	return `{"books":[` + rows + `]}`
}

const oneRow = `{"book_hash":"h1","title":"T","progress":[10,100],"updated_at":"2026-01-02T00:00:00Z"}`

func TestPullBooksSuccess(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok-1"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: booksBody(oneRow)}}}
	c := newTestClient(auth, doer)

	rows, err := c.PullBooks(context.Background(), 1234)
	if err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if len(rows) != 1 || rows[0].BookHash != "h1" {
		t.Errorf("rows = %+v", rows)
	}
	if got := doer.requests[0].URL.Query().Get("since"); got != "1234" {
		t.Errorf("since query = %q, want 1234", got)
	}
	if auth.accessCalls != 1 {
		t.Errorf("AccessToken calls = %d, want 1", auth.accessCalls)
	}
}

func TestPullBooksEmptyLibrary(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"books":[]}`}}}
	c := newTestClient(auth, doer)

	rows, err := c.PullBooks(context.Background(), 5)
	if err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want empty", rows)
	}
	if rows == nil {
		t.Error("rows should be non-nil empty slice")
	}
}

func TestPullBooksFullPullSendsZeroSince(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"books":[]}`}}}
	c := newTestClient(auth, doer)

	if _, err := c.PullBooks(context.Background(), 0); err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if got := doer.requests[0].URL.Query().Get("since"); got != "0" {
		t.Errorf("since query = %q, want 0", got)
	}
}

func TestPullBooksRequestShape(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"books":[]}`}}}
	c := newTestClient(auth, doer)

	if _, err := c.PullBooks(context.Background(), 42); err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	req := doer.requests[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.URL.Path != "/api/sync" {
		t.Errorf("path = %q, want /api/sync", req.URL.Path)
	}
	if got := req.URL.Query().Get("type"); got != "books" {
		t.Errorf("type query = %q, want books", got)
	}
}

func TestPullBooksHeaders(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok-abc"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"books":[]}`}}}
	c := newTestClient(auth, doer)

	if _, err := c.PullBooks(context.Background(), 0); err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	req := doer.requests[0]
	if got := req.Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want Bearer tok-abc", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

func TestPullBooksAccessTokenError(t *testing.T) {
	sentinel := errors.New("auth unavailable")
	auth := &fakeAuth{accessErr: sentinel}
	doer := &stubDoer{} // no steps: any request errors
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want auth error", err)
	}
	if doer.requestCount() != 0 {
		t.Errorf("no HTTP request should fire when AccessToken fails, got %d", doer.requestCount())
	}
}

func TestPullBooks401ReauthRetrySucceeds(t *testing.T) {
	auth := &fakeAuth{accessToken: "old-tok", forceToken: "new-tok"}
	doer := &stubDoer{steps: []stubStep{
		{status: 401, body: `{"error":"Unauthorized"}`},
		{status: 200, body: booksBody(oneRow)},
	}}
	c := newTestClient(auth, doer)

	rows, err := c.PullBooks(context.Background(), 7)
	if err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %+v, want 1 row", rows)
	}
	if doer.requestCount() != 2 {
		t.Fatalf("requests = %d, want 2 (initial + retry)", doer.requestCount())
	}
	if auth.forceCalls != 1 {
		t.Errorf("ForceRefresh calls = %d, want 1", auth.forceCalls)
	}
	// The retry must carry the new token.
	if got := doer.requests[1].Header.Get("Authorization"); got != "Bearer new-tok" {
		t.Errorf("retry Authorization = %q, want Bearer new-tok", got)
	}
}

func TestPullBooks403ReauthRetrySucceeds(t *testing.T) {
	auth := &fakeAuth{accessToken: "old", forceToken: "new"}
	doer := &stubDoer{steps: []stubStep{
		{status: 403, body: `{"error":"Not authenticated"}`},
		{status: 200, body: booksBody(oneRow)},
	}}
	c := newTestClient(auth, doer)

	if _, err := c.PullBooks(context.Background(), 0); err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if doer.requestCount() != 2 || auth.forceCalls != 1 {
		t.Errorf("requests = %d, forceCalls = %d; want 2 and 1", doer.requestCount(), auth.forceCalls)
	}
}

func TestPullBooksAuthErrorBodyTriggersRetry(t *testing.T) {
	// A non-401/403 status carrying {"error":"Not authenticated"} must also
	// trigger the re-auth-and-retry path (the plugin's body fallback).
	auth := &fakeAuth{accessToken: "old", forceToken: "new"}
	doer := &stubDoer{steps: []stubStep{
		{status: 400, body: `{"error":"Not authenticated"}`},
		{status: 200, body: booksBody(oneRow)},
	}}
	c := newTestClient(auth, doer)

	if _, err := c.PullBooks(context.Background(), 0); err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if doer.requestCount() != 2 || auth.forceCalls != 1 {
		t.Errorf("requests = %d, forceCalls = %d; want 2 and 1", doer.requestCount(), auth.forceCalls)
	}
}

func TestPullBooks401ReauthFails(t *testing.T) {
	auth := &fakeAuth{accessToken: "old", forceErr: ErrInvalidRefreshToken}
	doer := &stubDoer{steps: []stubStep{{status: 401, body: `{"error":"Unauthorized"}`}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrInvalidRefreshToken) {
		t.Errorf("err = %v, want wrapped re-auth error", err)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 (no retry after failed re-auth)", doer.requestCount())
	}
}

func TestPullBooks401RetryStillUnauthorized(t *testing.T) {
	auth := &fakeAuth{accessToken: "old", forceToken: "new"}
	doer := &stubDoer{steps: []stubStep{
		{status: 401, body: `{"error":"Unauthorized"}`},
		{status: 401, body: `{"error":"Unauthorized"}`},
	}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	// Exactly two attempts, never a third.
	if doer.requestCount() != 2 {
		t.Errorf("requests = %d, want exactly 2", doer.requestCount())
	}
	if auth.forceCalls != 1 {
		t.Errorf("ForceRefresh calls = %d, want 1", auth.forceCalls)
	}
}

func TestPullBooksBadRequest(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 400, body: `{"error":"since must be an integer"}`}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 (no retry on 400)", doer.requestCount())
	}
}

func TestPullBooksRateLimited(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 429, body: `{"error":"slow down"}`}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrSyncRateLimited) {
		t.Errorf("err = %v, want ErrSyncRateLimited", err)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 (client performs no transient retry)", doer.requestCount())
	}
}

func TestPullBooksServerError(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			auth := &fakeAuth{accessToken: "tok"}
			doer := &stubDoer{steps: []stubStep{{status: status, body: `{"error":"boom"}`}}}
			c := newTestClient(auth, doer)

			_, err := c.PullBooks(context.Background(), 0)
			if !errors.Is(err, ErrSyncServer) {
				t.Errorf("err = %v, want ErrSyncServer", err)
			}
			if doer.requestCount() != 1 {
				t.Errorf("requests = %d, want 1 (no client retry)", doer.requestCount())
			}
		})
	}
}

func TestPullBooksNetworkError(t *testing.T) {
	netErr := errors.New("connection refused")
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{err: netErr}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrSyncNetwork) {
		t.Errorf("err = %v, want ErrSyncNetwork", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err should carry underlying detail: %v", err)
	}
}

func TestPullBooksContextCancelled(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{err: context.Canceled}}}
	c := newTestClient(auth, doer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.PullBooks(ctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestPullBooksMalformedJSON(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"books":[`}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrSyncMalformed) {
		t.Errorf("err = %v, want ErrSyncMalformed", err)
	}
}

func TestPullBooksBooksNotArray(t *testing.T) {
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"books":{"a":1}}`}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrSyncMalformed) {
		t.Errorf("err = %v, want ErrSyncMalformed", err)
	}
}

func TestPullBooksBodyOverCap(t *testing.T) {
	// A body larger than the read cap must be rejected as malformed rather than
	// silently truncated into a confusing JSON error.
	auth := &fakeAuth{accessToken: "tok"}
	big := `{"books":[` + strings.Repeat(" ", maxResponseBodyBytes) + `]}`
	doer := &stubDoer{steps: []stubStep{{status: 200, body: big}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrSyncMalformed) {
		t.Errorf("err = %v, want ErrSyncMalformed", err)
	}
}

func TestPullBooksWeirdProgressRowReturned(t *testing.T) {
	// A row with a stringified/null progress tuple must not fail the whole page;
	// the client returns rows faithfully.
	auth := &fakeAuth{accessToken: "tok"}
	body := `{"books":[` +
		`{"book_hash":"h1","progress":"[5,50]","updated_at":"2026-01-02T00:00:00Z"},` +
		`{"book_hash":"h2","progress":null,"updated_at":"2026-01-02T00:00:00Z"}` +
		`]}`
	doer := &stubDoer{steps: []stubStep{{status: 200, body: body}}}
	c := newTestClient(auth, doer)

	rows, err := c.PullBooks(context.Background(), 0)
	if err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// The stringified tuple still decodes through the model.
	if _, _, ok := rows[0].ProgressTuple(); !ok {
		t.Error("stringified progress tuple should still decode")
	}
}

func TestPullBooksReturnsRowsUnfiltered(t *testing.T) {
	// The client is faithful: dummy and deleted rows are returned to the caller
	// unfiltered. Filtering is the engine's job (Phase 6). This pins the boundary.
	auth := &fakeAuth{accessToken: "tok"}
	body := `{"books":[` +
		`{"book_hash":"` + DummyHash + `","updated_at":"2026-01-02T00:00:00Z"},` +
		`{"book_hash":"del1","deleted_at":"2026-01-03T00:00:00Z","updated_at":"2026-01-02T00:00:00Z"}` +
		`]}`
	doer := &stubDoer{steps: []stubStep{{status: 200, body: body}}}
	c := newTestClient(auth, doer)

	rows, err := c.PullBooks(context.Background(), 0)
	if err != nil {
		t.Fatalf("PullBooks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want both rows returned unfiltered", len(rows))
	}
	if !rows[0].IsDummy() {
		t.Error("first row should be the dummy sentinel")
	}
	if !rows[1].IsDeleted() {
		t.Error("second row should be flagged deleted")
	}
}

func TestPullBooksHonorsTimeout(t *testing.T) {
	// A per-call deadline must cancel a slow round trip.
	auth := &fakeAuth{accessToken: "tok"}
	doer := &stubDoer{steps: []stubStep{{status: 200, body: booksBody(oneRow), delay: 200 * time.Millisecond}}}
	c := NewClient("https://sync.test/api", auth, doer,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 20*time.Millisecond)

	_, err := c.PullBooks(context.Background(), 0)
	if err == nil {
		t.Fatal("PullBooks should fail when the per-call deadline is exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrSyncNetwork) {
		t.Errorf("err = %v, want a deadline/network error", err)
	}
}

func TestPullBooksConcurrentNoRace(t *testing.T) {
	// Concurrent pulls against a client with a fresh token must be race-free.
	// Use a doer that always succeeds (the stubDoer exhausts its scripted steps).
	auth := &fakeAuth{accessToken: "tok"}
	always := &alwaysOKDoer{body: booksBody(oneRow)}
	c := NewClient("https://sync.test/api", auth, always,
		slog.New(slog.NewTextHandler(io.Discard, nil)), 0)

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.PullBooks(context.Background(), 0)
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("worker %d error: %v", i, errs[i])
		}
	}
}

// alwaysOKDoer is a stubDoer variant that returns a fixed 200 body for any
// number of requests (the stubDoer exhausts its scripted steps).
type alwaysOKDoer struct{ body string }

func (d *alwaysOKDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(d.body)),
		Header:     make(http.Header),
	}, nil
}

func TestPullBooksErrorBodySnippetBounded(t *testing.T) {
	// A very long error body must be truncated in the returned error message.
	auth := &fakeAuth{accessToken: "tok"}
	longBody := `{"error":"` + strings.Repeat("x", 10*1024) + `"}`
	doer := &stubDoer{steps: []stubStep{{status: 500, body: longBody}}}
	c := newTestClient(auth, doer)

	_, err := c.PullBooks(context.Background(), 0)
	if !errors.Is(err, ErrSyncServer) {
		t.Fatalf("err = %v, want ErrSyncServer", err)
	}
	if len(err.Error()) > maxErrorBodyBytes+256 {
		t.Errorf("error message length = %d, want bounded by maxErrorBodyBytes", len(err.Error()))
	}
}
