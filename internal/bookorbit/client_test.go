package bookorbit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubDoer is a scriptable httpclient.Doer, mirroring the pattern already
// established in internal/readest (auth_test.go/client_test.go). Each queued
// step is invoked in order; a step either returns a response or an error. It
// records every request it sees so tests can assert on headers/bodies/paths.
type stubDoer struct {
	mu       sync.Mutex
	steps    []stubStep
	calls    int
	requests []*http.Request
	bodies   []string
}

type stubStep struct {
	status int
	body   string
	err    error
	delay  time.Duration
}

func (d *stubDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.calls++
	d.requests = append(d.requests, req)
	var bodyStr string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		bodyStr = string(b)
	}
	d.bodies = append(d.bodies, bodyStr)
	idx := d.calls
	d.mu.Unlock()

	if idx > len(d.steps) {
		return nil, errors.New("stubDoer: unexpected extra request")
	}
	step := d.steps[idx-1]
	if step.err != nil {
		return nil, step.err
	}
	if step.delay > 0 {
		timer := time.NewTimer(step.delay)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
	return &http.Response{
		StatusCode: step.status,
		Body:       io.NopCloser(strings.NewReader(step.body)),
		Header:     make(http.Header),
	}, nil
}

func (d *stubDoer) requestCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestClientWithClock(doer *stubDoer, now func() time.Time) *Client {
	c := NewClient("https://bookorbit.test/api/v1", "reader", "authkey123", DeviceInfo{
		DeviceID:      "dev-1",
		DeviceModel:   "readest-bridge",
		PluginVersion: "0.1.0",
	}, doer, testLogger(), 900*1024, 0)
	if now != nil {
		c.now = now
	}
	return c
}

func newTestClient(doer *stubDoer) *Client {
	return newTestClientWithClock(doer, nil)
}

// --- Auth() ---

func TestAuthSuccess(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	if err := c.Auth(context.Background()); err != nil {
		t.Fatalf("Auth: %v", err)
	}
}

func TestAuthUnauthorized(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			doer := &stubDoer{steps: []stubStep{{status: status, body: `{"error":"bad creds"}`}}}
			c := newTestClient(doer)

			err := c.Auth(context.Background())
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("err = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestAuthNetworkError(t *testing.T) {
	netErr := errors.New("connection refused")
	doer := &stubDoer{steps: []stubStep{{err: netErr}}}
	c := newTestClient(doer)

	err := c.Auth(context.Background())
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("err = %v, want ErrNetwork", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err should carry underlying detail: %v", err)
	}
}

func TestAuthRequestShape(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	if err := c.Auth(context.Background()); err != nil {
		t.Fatalf("Auth: %v", err)
	}
	req := doer.requests[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.URL.Path != "/api/v1/koreader/users/auth" {
		t.Errorf("path = %q", req.URL.Path)
	}
	if got := req.Header.Get("accept"); got != "application/json" {
		t.Errorf("accept header = %q", got)
	}
	if got := req.Header.Get("x-auth-user"); got != "reader" {
		t.Errorf("x-auth-user = %q", got)
	}
	if got := req.Header.Get("x-auth-key"); got != "authkey123" {
		t.Errorf("x-auth-key = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type should be absent on a bodyless GET, got %q", got)
	}
}

// --- MatchCheck() ---

func TestMatchCheckSuccess(t *testing.T) {
	body := `{"matches":[{"hash":"h1","bookFileId":42,"bookId":7}],"unmatched":["h2"],"libraryVersion":"v1"}`
	doer := &stubDoer{steps: []stubStep{{status: 200, body: body}}}
	c := newTestClient(doer)

	resp, err := c.MatchCheck(context.Background(), MatchCheckRequest{Hashes: []string{"h1", "h2"}})
	if err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	if len(resp.Matches) != 1 || resp.Matches[0].BookFileID != 42 || resp.Matches[0].BookID != 7 {
		t.Errorf("Matches = %+v", resp.Matches)
	}
	if len(resp.Unmatched) != 1 || resp.Unmatched[0] != "h2" {
		t.Errorf("Unmatched = %+v", resp.Unmatched)
	}
	if resp.LibraryVersion != "v1" {
		t.Errorf("LibraryVersion = %q", resp.LibraryVersion)
	}
}

func TestMatchCheckNullFieldsNormalizeToEmpty(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"matches":null,"unmatched":null}`}}}
	c := newTestClient(doer)

	resp, err := c.MatchCheck(context.Background(), MatchCheckRequest{})
	if err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	if resp.Matches == nil || len(resp.Matches) != 0 {
		t.Errorf("Matches = %+v, want non-nil empty", resp.Matches)
	}
	if resp.Unmatched == nil || len(resp.Unmatched) != 0 {
		t.Errorf("Unmatched = %+v, want non-nil empty", resp.Unmatched)
	}
}

func TestMatchCheckEmptySlicesEncodeAsArrayNotNull(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	// Caller passes nil slices; the client must still send [] on the wire.
	if _, err := c.MatchCheck(context.Background(), MatchCheckRequest{Hashes: nil, Books: nil}); err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	body := doer.bodies[0]
	if !strings.Contains(body, `"hashes":[]`) {
		t.Errorf("body = %s, want hashes:[]", body)
	}
	if !strings.Contains(body, `"books":[]`) {
		t.Errorf("body = %s, want books:[]", body)
	}
}

func TestMatchCheckDeviceFieldsStampedAtTopLevel(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClientWithClock(doer, func() time.Time { return fixed })

	// Caller supplies stale/garbage device values; the client must overwrite them.
	req := MatchCheckRequest{
		Hashes:        []string{"h1"},
		DeviceID:      "stale-id",
		DeviceModel:   "stale-model",
		PluginVersion: "stale-version",
		DeviceTime:    "stale-time",
	}
	if _, err := c.MatchCheck(context.Background(), req); err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	body := doer.bodies[0]
	for _, want := range []string{
		`"deviceId":"dev-1"`,
		`"deviceModel":"readest-bridge"`,
		`"pluginVersion":"0.1.0"`,
		`"deviceTime":"2026-01-02 03:04:05"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want to contain %s", body, want)
		}
	}
	if strings.Contains(body, "stale-") {
		t.Errorf("body = %s, caller-supplied stale device fields must be overwritten", body)
	}
}

func TestMatchCheckMetadataAmbiguousPassesThrough(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	req := MatchCheckRequest{Books: []MatchCandidate{{Hash: "h1", MetadataAmbiguous: true}}}
	if _, err := c.MatchCheck(context.Background(), req); err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	if !strings.Contains(doer.bodies[0], `"metadataAmbiguous":true`) {
		t.Errorf("body = %s, want metadataAmbiguous:true", doer.bodies[0])
	}
}

func TestMatchCheckHeaders(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	if _, err := c.MatchCheck(context.Background(), MatchCheckRequest{}); err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	req := doer.requests[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.URL.Path != "/api/v1/koreader/plugin/match-check" {
		t.Errorf("path = %q", req.URL.Path)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := req.Header.Get("x-auth-user"); got != "reader" {
		t.Errorf("x-auth-user = %q", got)
	}
	if got := req.Header.Get("x-auth-key"); got != "authkey123" {
		t.Errorf("x-auth-key = %q", got)
	}
	if req.ContentLength != int64(len(doer.bodies[0])) {
		t.Errorf("Content-Length = %d, want %d", req.ContentLength, len(doer.bodies[0]))
	}
}

func TestMatchCheckErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{400, ErrBadRequest},
		{401, ErrUnauthorized},
		{403, ErrUnauthorized},
		{404, ErrUnsupportedEndpoint},
		{405, ErrUnsupportedEndpoint},
		{429, ErrRateLimited},
		{500, ErrServer},
		{502, ErrServer},
		{503, ErrServer},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			doer := &stubDoer{steps: []stubStep{{status: c.status, body: `{"error":"boom"}`}}}
			cl := newTestClient(doer)

			_, err := cl.MatchCheck(context.Background(), MatchCheckRequest{})
			if !errors.Is(err, c.want) {
				t.Errorf("status %d: err = %v, want %v", c.status, err, c.want)
			}
		})
	}
}

func TestMatchCheckMalformedJSON(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"matches":`}}}
	c := newTestClient(doer)

	_, err := c.MatchCheck(context.Background(), MatchCheckRequest{})
	if !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("err = %v, want ErrMalformedResponse", err)
	}
}

func TestMatchCheckBodyTooLargeMakesNoRequest(t *testing.T) {
	doer := &stubDoer{} // no steps: any request would error
	c := NewClient("https://bookorbit.test/api/v1", "reader", "authkey123", DeviceInfo{}, doer, testLogger(), 10, 0)

	req := MatchCheckRequest{Books: []MatchCandidate{{Hash: "a-very-long-hash-value-that-exceeds-the-cap"}}}
	_, err := c.MatchCheck(context.Background(), req)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
	if doer.requestCount() != 0 {
		t.Errorf("requests = %d, want 0 (request never dispatched)", doer.requestCount())
	}
}

func TestMatchCheckContextCancelled(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{err: context.Canceled}}}
	c := newTestClient(doer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.MatchCheck(ctx, MatchCheckRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestMatchCheckSingleHash(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"matches":[{"hash":"h1","bookFileId":1,"bookId":1}]}`}}}
	c := newTestClient(doer)

	resp, err := c.MatchCheck(context.Background(), MatchCheckRequest{
		Hashes: []string{"h1"},
		Books:  []MatchCandidate{{Hash: "h1"}},
	})
	if err != nil {
		t.Fatalf("MatchCheck: %v", err)
	}
	if len(resp.Matches) != 1 {
		t.Errorf("Matches = %+v, want 1", resp.Matches)
	}
}

// --- BulkProgress() ---

func TestBulkProgressSuccess(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{"unmatched":["h1","h2"]}`}}}
	c := newTestClient(doer)

	resp, err := c.BulkProgress(context.Background(), BulkProgressRequest{
		Items: []ProgressItem{{Hash: "h1", Percentage: 0.5}},
	})
	if err != nil {
		t.Fatalf("BulkProgress: %v", err)
	}
	if len(resp.Unmatched) != 2 || resp.Unmatched[0] != "h1" {
		t.Errorf("Unmatched = %+v", resp.Unmatched)
	}
}

func TestBulkProgressRequestShapeDeviceWrapped(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	if _, err := c.BulkProgress(context.Background(), BulkProgressRequest{Items: nil}); err != nil {
		t.Fatalf("BulkProgress: %v", err)
	}
	body := doer.bodies[0]
	if !strings.Contains(body, `"items":[]`) {
		t.Errorf("body = %s, want items:[]", body)
	}
	if !strings.Contains(body, `"deviceId":"dev-1"`) {
		t.Errorf("body = %s, want device-wrapped", body)
	}
	req := doer.requests[0]
	if req.URL.Path != "/api/v1/koreader/plugin/progress" {
		t.Errorf("path = %q", req.URL.Path)
	}
}

func TestBulkProgressUnmatchedAbsentNormalizesToEmpty(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	resp, err := c.BulkProgress(context.Background(), BulkProgressRequest{})
	if err != nil {
		t.Fatalf("BulkProgress: %v", err)
	}
	if resp.Unmatched == nil || len(resp.Unmatched) != 0 {
		t.Errorf("Unmatched = %+v, want non-nil empty", resp.Unmatched)
	}
}

func TestBulkProgressErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{401, ErrUnauthorized},
		{403, ErrUnauthorized},
		{400, ErrBadRequest},
		{404, ErrUnsupportedEndpoint},
		{405, ErrUnsupportedEndpoint},
		{429, ErrRateLimited},
		{500, ErrServer},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			doer := &stubDoer{steps: []stubStep{{status: c.status, body: `{}`}}}
			cl := newTestClient(doer)

			_, err := cl.BulkProgress(context.Background(), BulkProgressRequest{})
			if !errors.Is(err, c.want) {
				t.Errorf("status %d: err = %v, want %v", c.status, err, c.want)
			}
		})
	}
}

func TestBulkProgressNetworkError(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{err: errors.New("timeout")}}}
	c := newTestClient(doer)

	_, err := c.BulkProgress(context.Background(), BulkProgressRequest{})
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("err = %v, want ErrNetwork", err)
	}
}

func TestBulkProgressMalformedJSON(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `not json`}}}
	c := newTestClient(doer)

	_, err := c.BulkProgress(context.Background(), BulkProgressRequest{})
	if !errors.Is(err, ErrMalformedResponse) {
		t.Errorf("err = %v, want ErrMalformedResponse", err)
	}
}

func TestBulkProgressBodyTooLargeMakesNoRequest(t *testing.T) {
	doer := &stubDoer{}
	c := NewClient("https://bookorbit.test/api/v1", "reader", "authkey123", DeviceInfo{}, doer, testLogger(), 5, 0)

	_, err := c.BulkProgress(context.Background(), BulkProgressRequest{
		Items: []ProgressItem{{Hash: "h1", Progress: "somewhat long opaque position string"}},
	})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
	if doer.requestCount() != 0 {
		t.Errorf("requests = %d, want 0", doer.requestCount())
	}
}

func TestBulkProgressEmptyItemsSentFaithfully(t *testing.T) {
	// The client does not special-case zero items; the engine is responsible
	// for not calling with nothing to push.
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`}}}
	c := newTestClient(doer)

	if _, err := c.BulkProgress(context.Background(), BulkProgressRequest{Items: []ProgressItem{}}); err != nil {
		t.Fatalf("BulkProgress: %v", err)
	}
	if doer.requestCount() != 1 {
		t.Errorf("requests = %d, want 1 (client sends faithfully)", doer.requestCount())
	}
}

// --- UpdateProgress() ---

func TestUpdateProgressSuccess(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: ``}}}
	c := newTestClient(doer)

	err := c.UpdateProgress(context.Background(), UpdateProgressRequest{
		Document: "h1", Percentage: 0.5, Timestamp: 1700000000,
	})
	if err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
}

func TestUpdateProgressRequestShape(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: ``}}}
	c := newTestClient(doer)

	err := c.UpdateProgress(context.Background(), UpdateProgressRequest{
		Document: "h1", Percentage: 0.75, Progress: "", Timestamp: 1700000000,
	})
	if err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	req := doer.requests[0]
	if req.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", req.Method)
	}
	if req.URL.Path != "/api/v1/koreader/syncs/progress" {
		t.Errorf("path = %q", req.URL.Path)
	}
	body := doer.bodies[0]
	for _, want := range []string{`"document":"h1"`, `"percentage":0.75`, `"progress":""`, `"timestamp":1700000000`, `"device":"readest-bridge"`, `"device_id":"dev-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %s, want to contain %s", body, want)
		}
	}
	// Must NOT carry the camelCase device-wrapper shape used by the plugin endpoints.
	for _, notWant := range []string{"deviceId", "pluginVersion", "deviceTime"} {
		if strings.Contains(body, notWant) {
			t.Errorf("body = %s, should not contain device-wrapper key %s", body, notWant)
		}
	}
}

func TestUpdateProgressOverridesCallerDeviceFields(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: ``}}}
	c := newTestClient(doer)

	err := c.UpdateProgress(context.Background(), UpdateProgressRequest{
		Document: "h1", Device: "caller-supplied", DeviceID: "caller-id", Timestamp: 42,
	})
	if err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	body := doer.bodies[0]
	if !strings.Contains(body, `"device":"readest-bridge"`) || !strings.Contains(body, `"device_id":"dev-1"`) {
		t.Errorf("body = %s, want client device identity to override caller values", body)
	}
	if strings.Contains(body, "caller-supplied") || strings.Contains(body, "caller-id") {
		t.Errorf("body = %s, caller-supplied device fields must be overwritten", body)
	}
	// Timestamp is domain data and must pass through unmodified.
	if !strings.Contains(body, `"timestamp":42`) {
		t.Errorf("body = %s, want timestamp passed through unmodified", body)
	}
}

func TestUpdateProgressErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{401, ErrUnauthorized},
		{403, ErrUnauthorized},
		{404, ErrUnsupportedEndpoint},
		{405, ErrUnsupportedEndpoint},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			doer := &stubDoer{steps: []stubStep{{status: c.status, body: `{}`}}}
			cl := newTestClient(doer)

			err := cl.UpdateProgress(context.Background(), UpdateProgressRequest{Document: "h1"})
			if !errors.Is(err, c.want) {
				t.Errorf("status %d: err = %v, want %v", c.status, err, c.want)
			}
		})
	}
}

func TestUpdateProgressNetworkError(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{err: errors.New("dial tcp: no route to host")}}}
	c := newTestClient(doer)

	err := c.UpdateProgress(context.Background(), UpdateProgressRequest{Document: "h1"})
	if !errors.Is(err, ErrNetwork) {
		t.Errorf("err = %v, want ErrNetwork", err)
	}
}

// --- Cross-cutting ---

func TestNewClientNilLoggerDefaultsToSlogDefault(t *testing.T) {
	c := NewClient("https://bookorbit.test/api/v1", "u", "k", DeviceInfo{}, &stubDoer{}, nil, 900*1024, 0)
	if c.log == nil {
		t.Error("log should default to slog.Default(), not remain nil")
	}
}

func TestNewClientZeroTimeoutDisablesPerCallDeadline(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`, delay: 20 * time.Millisecond}}}
	c := NewClient("https://bookorbit.test/api/v1", "u", "k", DeviceInfo{}, doer, testLogger(), 900*1024, 0)

	if err := c.Auth(context.Background()); err != nil {
		t.Fatalf("Auth should succeed with no per-call deadline: %v", err)
	}
}

func TestPerCallDeadlineHonored(t *testing.T) {
	doer := &stubDoer{steps: []stubStep{{status: 200, body: `{}`, delay: 200 * time.Millisecond}}}
	c := NewClient("https://bookorbit.test/api/v1", "u", "k", DeviceInfo{}, doer, testLogger(), 900*1024, 20*time.Millisecond)

	err := c.Auth(context.Background())
	if err == nil {
		t.Fatal("Auth should fail when the per-call deadline is exceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrNetwork) {
		t.Errorf("err = %v, want a deadline/network error", err)
	}
}

func TestErrorMessageBodySnippetBounded(t *testing.T) {
	longBody := `{"error":"` + strings.Repeat("x", 10*1024) + `"}`
	doer := &stubDoer{steps: []stubStep{{status: 500, body: longBody}}}
	c := newTestClient(doer)

	_, err := c.MatchCheck(context.Background(), MatchCheckRequest{})
	if !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v, want ErrServer", err)
	}
	if len(err.Error()) > maxErrorBodyBytes+256 {
		t.Errorf("error message length = %d, want bounded by maxErrorBodyBytes", len(err.Error()))
	}
}

func TestConcurrentCallsNoRace(t *testing.T) {
	doer := &alwaysOKDoer{body: `{"matches":[],"unmatched":[]}`}
	c := newTestClient(nil)
	c.http = doer

	const workers = 16
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.MatchCheck(context.Background(), MatchCheckRequest{Hashes: []string{"h1"}})
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
// number of requests, used for the concurrency test where request counts are
// not otherwise asserted.
type alwaysOKDoer struct{ body string }

func (d *alwaysOKDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(d.body)),
		Header:     make(http.Header),
	}, nil
}
