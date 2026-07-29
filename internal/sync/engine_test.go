package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/bookorbit"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/config"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/readest"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// noSleep never actually waits, so retries in tests are instantaneous.
func noSleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// realSleep is used only by the cancellation test, where an actual
// interruptible wait is the thing under test.
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func mkRow(hash string, cur, total float64, updatedAt string) readest.BookRow {
	return readest.BookRow{
		BookHash:  hash,
		Progress:  json.RawMessage(fmt.Sprintf("[%v,%v]", cur, total)),
		UpdatedAt: updatedAt,
	}
}

type fakeReadest struct {
	rows []readest.BookRow
	err  error
}

func (f *fakeReadest) PullBooks(ctx context.Context, since int64) ([]readest.BookRow, error) {
	return f.rows, f.err
}

type fakeBookOrbit struct {
	matchResp  bookorbit.MatchCheckResponse
	matchErr   error
	matchCalls int
	// matchReq is the last MatchCheckRequest passed to MatchCheck, kept so
	// tests can assert on candidate field values (design §12 case 3).
	matchReq bookorbit.MatchCheckRequest

	bulkResp  bookorbit.BulkProgressResponse
	bulkErr   error
	bulkCalls int
	// bulkItems records the items of every successful BulkProgress call, in
	// dispatch order, so tests can assert that all queued items were pushed.
	bulkItems []bookorbit.ProgressItem

	updateErr   error
	updateCalls int
	// updateReqs records every UpdateProgress request, in dispatch order, so
	// the fallback tests can assert per-item push shape and order.
	updateReqs []bookorbit.UpdateProgressRequest
}

func (f *fakeBookOrbit) Auth(ctx context.Context) error { return nil }

func (f *fakeBookOrbit) MatchCheck(ctx context.Context, req bookorbit.MatchCheckRequest) (bookorbit.MatchCheckResponse, error) {
	f.matchCalls++
	f.matchReq = req
	return f.matchResp, f.matchErr
}

func (f *fakeBookOrbit) BulkProgress(ctx context.Context, req bookorbit.BulkProgressRequest) (bookorbit.BulkProgressResponse, error) {
	f.bulkCalls++
	f.bulkItems = append(f.bulkItems, req.Items...)
	return f.bulkResp, f.bulkErr
}

func (f *fakeBookOrbit) UpdateProgress(ctx context.Context, req bookorbit.UpdateProgressRequest) error {
	f.updateCalls++
	f.updateReqs = append(f.updateReqs, req)
	return f.updateErr
}

// saveCountingStore wraps a state.Store, counting Save() calls so design §12
// case 20 ("Save is called exactly once per RunOnce") can be asserted without
// any engine-side test hook. Every other method is forwarded transparently.
type saveCountingStore struct {
	state.Store
	saves int
}

func (s *saveCountingStore) Save() error {
	s.saves++
	return s.Store.Save()
}

// scriptedBookOrbit is a bookorbit.API fake whose MatchCheck response/error
// vary by call index, so a single RunOnce can exercise "first chunk
// succeeds, second chunk fails". Other methods behave like the plain fake.
// Entries are read sequentially; running past the end of either slice returns
// a zero value / nil so an undert-sized script fails cleanly rather than
// panicking.
type scriptedBookOrbit struct {
	matchScript []bookorbit.MatchCheckResponse
	matchErrs   []error
	matchCalls  int
}

func (s *scriptedBookOrbit) Auth(ctx context.Context) error { return nil }

func (s *scriptedBookOrbit) MatchCheck(ctx context.Context, req bookorbit.MatchCheckRequest) (bookorbit.MatchCheckResponse, error) {
	idx := s.matchCalls
	s.matchCalls++
	var resp bookorbit.MatchCheckResponse
	if idx < len(s.matchScript) {
		resp = s.matchScript[idx]
	}
	var err error
	if idx < len(s.matchErrs) {
		err = s.matchErrs[idx]
	}
	return resp, err
}

func (s *scriptedBookOrbit) BulkProgress(ctx context.Context, req bookorbit.BulkProgressRequest) (bookorbit.BulkProgressResponse, error) {
	return bookorbit.BulkProgressResponse{}, nil
}

func (s *scriptedBookOrbit) UpdateProgress(ctx context.Context, req bookorbit.UpdateProgressRequest) error {
	return nil
}

func newTestEngine(rd *fakeReadest, bo bookorbit.API, st state.Store) *Engine {
	e := NewEngine(config.Default(), nil, rd, bo, st, testLogger())
	e.sleep = noSleep
	return e
}

func TestEnginePollInterval(t *testing.T) {
	cfg := config.Default()
	cfg.Bridge.PollInterval = 42 * time.Minute
	e := NewEngine(cfg, nil, nil, nil, state.NewMemStore(), testLogger())
	if got := e.PollInterval(); got != 42*time.Minute {
		t.Errorf("PollInterval = %v, want 42m", got)
	}
}

func TestRunOnceEmptyPull(t *testing.T) {
	rd := &fakeReadest{}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.Watermark() != 0 {
		t.Errorf("watermark = %d, want 0", st.Watermark())
	}
	if bo.matchCalls != 0 || bo.bulkCalls != 0 {
		t.Error("no BookOrbit calls expected for an empty pull")
	}
}

func TestRunOnceDummyRowFiltered(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{{BookHash: readest.DummyHash, UpdatedAt: "2026-01-02T00:00:00Z"}}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.Watermark() != 0 {
		t.Errorf("watermark = %d, want 0 (dummy row excluded)", st.Watermark())
	}
	if bo.matchCalls != 0 {
		t.Error("dummy row must not trigger a match-check")
	}
}

func TestRunOnceFreshMatchAlwaysPushes(t *testing.T) {
	row := mkRow("h1", 0, 100, "2026-01-02T00:00:00Z") // pct == 0, the degenerate case
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{Matches: []bookorbit.Match{{Hash: "h1", BookFileID: 1, BookID: 2}}},
	}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.matchCalls != 1 || bo.bulkCalls != 1 {
		t.Fatalf("matchCalls=%d bulkCalls=%d, want 1,1", bo.matchCalls, bo.bulkCalls)
	}
	// Design §12 case 3: a fresh, unmatched row must build a MatchCandidate
	// with the bridge-specific values from §6.2 (Source="readest",
	// MetadataAmbiguous=false, LastOpen = updated_at in Unix seconds).
	if len(bo.matchReq.Books) != 1 {
		t.Fatalf("matchReq.Books len = %d, want 1", len(bo.matchReq.Books))
	}
	cand := bo.matchReq.Books[0]
	if cand.Hash != "h1" {
		t.Errorf("candidate Hash = %q, want h1", cand.Hash)
	}
	if cand.Source != "readest" {
		t.Errorf("candidate Source = %q, want readest", cand.Source)
	}
	if cand.MetadataAmbiguous {
		t.Error("candidate MetadataAmbiguous should be false for a headless bridge")
	}
	const wantLastOpen int64 = 1767312000 // 2026-01-02T00:00:00Z in Unix seconds
	if cand.LastOpen != wantLastOpen {
		t.Errorf("candidate LastOpen = %d, want %d (updated_at in seconds)", cand.LastOpen, wantLastOpen)
	}
	rec, err := st.Match("h1")
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if rec.LastPushedAt == 0 {
		t.Error("a freshly matched book with pct==0 must still be pushed once")
	}
	if st.Watermark() == 0 {
		t.Error("watermark should have advanced")
	}
}

func TestRunOnceUnchangedPercentageSkipsPush(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 2, LastPushedAt: 1000, LastPushedPct: 0.5})
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.bulkCalls != 0 {
		t.Errorf("bulkCalls = %d, want 0 (unchanged within tolerance)", bo.bulkCalls)
	}
	// Design §12 case 4: even though nothing is pushed, the watermark still
	// advances over the row so future polls do not redeliver this same row.
	if st.Watermark() == 0 {
		t.Error("watermark should still advance for an unchanged-but-seen row")
	}
}

func TestRunOnceChangedPercentagePushes(t *testing.T) {
	row := mkRow("h1", 60, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 2, LastPushedAt: 1000, LastPushedPct: 0.5})
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.bulkCalls != 1 {
		t.Errorf("bulkCalls = %d, want 1", bo.bulkCalls)
	}
	rec, _ := st.Match("h1")
	if rec.LastPushedPct != 0.6 {
		t.Errorf("LastPushedPct = %v, want 0.6", rec.LastPushedPct)
	}
	// Design §12 case 5: LastPushedAt is also refreshed to mark the push.
	if rec.LastPushedAt == 0 {
		t.Error("LastPushedAt should be non-zero after a successful push")
	}
}

func TestRunOnceDeletedRowResetsState(t *testing.T) {
	row := readest.BookRow{BookHash: "h1", DeletedAt: "2026-01-05T00:00:00Z", UpdatedAt: "2026-01-02T00:00:00Z"}
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 2})
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := st.Match("h1"); !errors.Is(err, state.ErrNotFound) {
		t.Error("deleted row's match record should be cleared")
	}
	if bo.matchCalls != 0 || bo.bulkCalls != 0 {
		t.Error("a deleted row must never contact BookOrbit")
	}
	if st.Watermark() == 0 {
		t.Error("watermark should still advance for a deleted row")
	}
}

func TestRunOnceUnmatchedCooldownSkipsRecheck(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetUnmatched("h1", time.Now().Unix())
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.matchCalls != 0 {
		t.Errorf("matchCalls = %d, want 0 (within cooldown)", bo.matchCalls)
	}
}

func TestRunOnceUnmatchedPastCooldownRechecks(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{matchResp: bookorbit.MatchCheckResponse{Unmatched: []string{"h1"}}}
	// h1 has no MatchRecord (ErrNotFound) — only a stale unmatched entry
	// past the 24h default cooldown. This is the canonical scenario where
	// the engine makes a fresh MatchCheck call and the server replies
	// "unmatched", exercising §6.3's SetUnmatched + DeleteMatch coupling
	// (defensive delete of any prior match; harmless when there is none).
	st := state.NewMemStore()
	st.SetUnmatched("h1", time.Now().Add(-25*time.Hour).Unix()) // past the 24h default cooldown
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.matchCalls != 1 {
		t.Errorf("matchCalls = %d, want 1 (cooldown expired)", bo.matchCalls)
	}
	// Design §12 case 8: the server's "unmatched" reply records a fresh
	// Unmatched cooldown timestamp and ensures no match remains, no push
	// happens, and the watermark still advances for the re-checked row.
	if bo.bulkCalls != 0 {
		t.Errorf("bulkCalls = %d, want 0 (no push for unmatched)", bo.bulkCalls)
	}
	if _, merr := st.Match("h1"); !errors.Is(merr, state.ErrNotFound) {
		t.Errorf("unmatched reply must leave no match, got %v", merr)
	}
	if at, ok := st.UnmatchedAt("h1"); !ok {
		t.Error("unmatched reply should record a fresh Unmatched timestamp")
	} else if at == 0 {
		t.Error("fresh Unmatched timestamp should be non-zero")
	}
	if st.Watermark() == 0 {
		t.Error("watermark should still advance after an unmatched recheck")
	}

	// Design §12 case 8 trailing: the next RunOnce, now within the freshly
	// established cooldown, must not recheck.
	bo.matchCalls = 0
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if bo.matchCalls != 0 {
		t.Errorf("matchCalls = %d after recheck, want 0 (now within cooldown)", bo.matchCalls)
	}
}

func TestRunOnceMatchCheckFailureRetreatsWatermark(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{matchErr: bookorbit.ErrServer}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0 // deterministic: one call, no retries

	err := e.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce should return a non-nil error")
	}
	if !errors.Is(err, bookorbit.ErrServer) {
		t.Errorf("err = %v, want wrapped ErrServer", err)
	}
	want := row.WatermarkMs() - 1
	if got := st.Watermark(); got != want {
		t.Errorf("watermark = %d, want %d (retreated)", got, want)
	}
}

func TestRunOnceAuthFailureAbortsWithoutMutatingState(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{matchErr: bookorbit.ErrUnauthorized}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)

	err := e.RunOnce(context.Background())
	if !errors.Is(err, bookorbit.ErrUnauthorized) {
		t.Errorf("err = %v, want wrapped ErrUnauthorized", err)
	}
	if _, merr := st.Match("h1"); !errors.Is(merr, state.ErrNotFound) {
		t.Error("no match should have been recorded for the failed batch")
	}
}

func TestRunOnceBulkUnsupportedFallsBackAndStaysfallenBack(t *testing.T) {
	row1 := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row1}}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{Matches: []bookorbit.Match{{Hash: "h1", BookFileID: 1, BookID: 2}}},
		bulkErr:   bookorbit.ErrUnsupportedEndpoint,
	}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if !e.bulkUnsupported {
		t.Fatal("bulkUnsupported should be set after ErrUnsupportedEndpoint")
	}
	if bo.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (fallback used immediately)", bo.updateCalls)
	}
	rec, err := st.Match("h1")
	if err != nil || rec.LastPushedPct != 0.5 {
		t.Errorf("Match = %+v, %v; want pushed pct 0.5", rec, err)
	}

	// A second pass with a changed percentage must go straight to
	// UpdateProgress without ever retrying BulkProgress.
	rd.rows = []readest.BookRow{mkRow("h1", 60, 100, "2026-01-03T00:00:00Z")}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if bo.bulkCalls != 1 {
		t.Errorf("bulkCalls = %d, want still 1 (never retried once unsupported)", bo.bulkCalls)
	}
	if bo.updateCalls != 2 {
		t.Errorf("updateCalls = %d, want 2", bo.updateCalls)
	}
}

func TestComputeWatermark(t *testing.T) {
	cases := []struct {
		name         string
		floor, maxWM int64
		failed       []int64
		want         int64
	}{
		{"advance, no failures", 100, 500, nil, 500},
		{"floor wins when max is behind", 500, 100, nil, 500},
		{"retreat on failure", 100, 500, []int64{300, 250}, 249},
		{"retreat clamped to floor", 100, 500, []int64{50}, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeWatermark(c.floor, c.maxWM, c.failed); got != c.want {
				t.Errorf("computeWatermark(%d,%d,%v) = %d, want %d", c.floor, c.maxWM, c.failed, got, c.want)
			}
		})
	}
}

func TestRunRespectsCancellationDuringPollSleep(t *testing.T) {
	rd := &fakeReadest{}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)
	e.sleep = realSleep
	e.cfg.Bridge.PollInterval = time.Hour // only cancellation should stop it

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancellation")
	}
}

func TestRunContinuesAfterRunOnceError(t *testing.T) {
	rd := &fakeReadest{err: readest.ErrSyncServer}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0
	e.cfg.Bridge.PollInterval = time.Millisecond
	e.sleep = realSleep

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = e.Run(ctx) // should not panic or exit early despite RunOnce always failing
}

// --- Design §12 cases beyond the originals above. These complete the approved
// testing strategy: a row with an unusable progress tuple (case 7), a
// BulkProgress chunk failure retreating the watermark and holding
// LastPushedPct (case 12), a Readest auth-failure aborting the pull without
// mutating state (case 14), the withRetry helper's own contract (case 17),
// Save called exactly once per RunOnce (case 20), the unchanged-percentage
// tolerance boundary at exactly 0.001 vs 0.0011 (case 21), and a hash that
// reappears after deletion being treated as brand-new (case 22). They assert
// behavior that already holds in the engine — they only guard it against
// future regressions.

// case 7: a row whose progress tuple is unusable (zero total) is skipped, but
// the watermark still advances over that row's timestamp.
func TestRunOnceUnusableProgressSkippedButWatermarkAdvances(t *testing.T) {
	// total == 0 -> Percentage() returns ok=false.
	row := readest.BookRow{
		BookHash:  "h1",
		Progress:  json.RawMessage("[3,0]"),
		UpdatedAt: "2026-01-02T00:00:00Z",
	}
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.matchCalls != 0 || bo.bulkCalls != 0 {
		t.Errorf("BookOrbit calls = match:%d bulk:%d, want 0,0 (row skipped)", bo.matchCalls, bo.bulkCalls)
	}
	if st.Watermark() == 0 {
		t.Error("watermark should still advance over the unusable-progress row")
	}
}

// case 12: a BulkProgress chunk that fails every retry leaves
// MatchRecord.LastPushedPct unchanged and retreats the watermark to
// min(failed rows) - 1.
func TestRunOnceBulkProgressFailureHoldsPctAndRetreatsWatermark(t *testing.T) {
	row := mkRow("h1", 60, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{bulkErr: bookorbit.ErrServer}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 2, LastPushedAt: 1000, LastPushedPct: 0.5})
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0 // one call, no retries

	err := e.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce should return a non-nil error")
	}
	if !errors.Is(err, bookorbit.ErrServer) {
		t.Errorf("err = %v, want wrapped ErrServer", err)
	}
	rec, _ := st.Match("h1")
	if rec.LastPushedPct != 0.5 {
		t.Errorf("LastPushedPct = %v, want 0.5 (held on failure)", rec.LastPushedPct)
	}
	want := row.WatermarkMs() - 1
	if got := st.Watermark(); got != want {
		t.Errorf("watermark = %d, want %d (retreated)", got, want)
	}
}

// case 14: a Readest auth failure on the pull aborts RunOnce before any
// processing and mutates no state.
func TestRunOnceReadestAuthFailureAbortsBeforeProcessing(t *testing.T) {
	rd := &fakeReadest{err: readest.ErrUnauthorized}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{Matches: []bookorbit.Match{{Hash: "h1", BookFileID: 1, BookID: 2}}},
	}
	st := state.NewMemStore()
	st.SetWatermark(123456789000)
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0

	err := e.RunOnce(context.Background())
	if !errors.Is(err, readest.ErrUnauthorized) {
		t.Errorf("err = %v, want wrapped readest.ErrUnauthorized", err)
	}
	if bo.matchCalls != 0 || bo.bulkCalls != 0 {
		t.Errorf("BookOrbit calls = match:%d bulk:%d, want 0,0 (pull aborted first)", bo.matchCalls, bo.bulkCalls)
	}
	if _, merr := st.Match("h1"); !errors.Is(merr, state.ErrNotFound) {
		t.Error("no match should have been recorded when the pull aborted")
	}
	if st.Watermark() != 123456789000 {
		t.Errorf("watermark = %d, want unchanged 123456789000", st.Watermark())
	}
}

// case 17: withRetry's own contract, tested purely:
//   - outcomeRetry retries up to RetryMaxAttempts extra times;
//   - the delay grows by doubling and caps at RetryMaxBackoff;
//   - outcomeFatal and outcomeSkip never retry (one call each);
//   - a context cancellation during the backoff sleep aborts immediately.
func TestWithRetryRetriesAndGrowsAndCaps(t *testing.T) {
	var sleeps []time.Duration
	sleep := func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	cfg := config.Default()
	cfg.Bridge.RetryInitialBackoff = 100 * time.Millisecond
	cfg.Bridge.RetryMaxBackoff = 400 * time.Millisecond
	cfg.Bridge.RetryMaxAttempts = 5

	calls := 0
	err := withRetry(context.Background(), cfg.Bridge, sleep, func() error {
		calls++
		return fmt.Errorf("%w: boom", bookorbit.ErrServer)
	}, classifyBookOrbitErr)
	if !errors.Is(err, bookorbit.ErrServer) {
		t.Errorf("err = %v, want ErrServer", err)
	}
	if calls != 6 {
		t.Errorf("calls = %d, want 6 (initial + 5 retries)", calls)
	}
	wantSleeps := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond,
		400 * time.Millisecond,
	}
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", sleeps, wantSleeps)
	}
	for i := range wantSleeps {
		if sleeps[i] != wantSleeps[i] {
			t.Errorf("sleeps[%d] = %v, want %v", i, sleeps[i], wantSleeps[i])
		}
	}
}

func TestWithRetryFatalNeverRetries(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), config.Default().Bridge, noSleep, func() error {
		calls++
		return fmt.Errorf("%w", bookorbit.ErrUnauthorized)
	}, classifyBookOrbitErr)
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (fatal never retries)", calls)
	}
	if !errors.Is(err, bookorbit.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestWithRetrySkipNeverRetries(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), config.Default().Bridge, noSleep, func() error {
		calls++
		return fmt.Errorf("%w", bookorbit.ErrBodyTooLarge)
	}, classifyBookOrbitErr)
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (skip never retries)", calls)
	}
	if !errors.Is(err, bookorbit.ErrBodyTooLarge) {
		t.Errorf("err = %v, want ErrBodyTooLarge", err)
	}
}

func TestWithRetryCancelsMidBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	sleep := func(ctx context.Context, d time.Duration) error {
		// Mirror defaultSleep so a pre-cancelled ctx returns immediately.
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	calls := 0
	err := withRetry(ctx, config.Default().Bridge, sleep, func() error {
		calls++
		return bookorbit.ErrServer // retryable
	}, classifyBookOrbitErr)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	// Only the initial call happens; the first backoff sleep aborts before
	// any second call.
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancellation aborted the retry)", calls)
	}
}

// case 20: state.Store.Save() is called exactly once per RunOnce, regardless
// of whether the pass succeeded, failed partially (batch failures), or
// aborted on an auth error mid-run.
func TestRunOnceSavesExactlyOnceOnSuccess(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{Matches: []bookorbit.Match{{Hash: "h1", BookFileID: 1, BookID: 2}}},
	}
	inner := state.NewMemStore()
	st := &saveCountingStore{Store: inner}
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.saves != 1 {
		t.Errorf("saves = %d, want 1 (exactly once per RunOnce)", st.saves)
	}
}

func TestRunOnceSavesExactlyOnceOnBatchFailure(t *testing.T) {
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{matchErr: bookorbit.ErrServer}
	inner := state.NewMemStore()
	st := &saveCountingStore{Store: inner}
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0

	_ = e.RunOnce(context.Background()) // non-nil, that's expected
	if st.saves != 1 {
		t.Errorf("saves = %d, want 1 (save once even on partial failure)", st.saves)
	}
}

func TestRunOnceSavesExactlyOnceOnBookOrbitAuthAbort(t *testing.T) {
	// A single-chunk MatchCheck that fails with ErrUnauthorized aborts the
	// run mid-pass; Save must still run exactly once. The deeper "prior
	// successful chunk's state is preserved" half of design §12 case 13 is
	// covered separately by TestRunOnceBookOrbitAuthAbortPreservesPriorChunks.
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{matchErr: bookorbit.ErrUnauthorized}
	inner := state.NewMemStore()
	st := &saveCountingStore{Store: inner}
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0

	_ = e.RunOnce(context.Background())
	if st.saves != 1 {
		t.Errorf("saves = %d, want 1 (save once even on auth abort)", st.saves)
	}
}

// case 13 (deeper half): when BookOrbit returns ErrUnauthorized on a later
// match-check chunk, the engine aborts the remainder of RunOnce, but every
// state mutation committed by *prior successful chunks in the same pass* is
// preserved, and Save still runs.
func TestRunOnceBookOrbitAuthAbortPreservesPriorChunks(t *testing.T) {
	rows := []readest.BookRow{
		mkRow("good", 50, 100, "2026-01-02T00:00:00Z"),
		mkRow("bad", 60, 100, "2026-01-02T00:00:00Z"),
	}
	rd := &fakeReadest{rows: rows}
	bo := &scriptedBookOrbit{
		matchScript: []bookorbit.MatchCheckResponse{
			{Matches: []bookorbit.Match{{Hash: "good", BookFileID: 1, BookID: 2}}}, // chunk 1: success
			{}, // chunk 2: failure (error set below)
		},
		matchErrs: []error{nil, bookorbit.ErrUnauthorized},
	}
	inner := state.NewMemStore()
	st := &saveCountingStore{Store: inner}
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.MatchBatchSize = 1 // one hash per chunk -> success then fatal
	e.cfg.Bridge.RetryMaxAttempts = 0

	_ = e.RunOnce(context.Background())
	if st.saves != 1 {
		t.Errorf("saves = %d, want 1 (save once even on auth abort)", st.saves)
	}
	if rec, merr := st.Match("good"); merr != nil || rec.BookFileID != 1 || rec.BookID != 2 {
		t.Errorf("prior chunk's match for 'good' should be preserved, got %+v %v", rec, merr)
	}
	if _, merr := st.Match("bad"); !errors.Is(merr, state.ErrNotFound) {
		t.Errorf("aborted chunk's 'bad' should have no match, got %v", merr)
	}
}

// case 21: the unchanged-percentage tolerance boundary against
// `math.Abs(pct-rec.LastPushedPct) <= percentTolerance` (percentTolerance =
// 0.001, the formula copied verbatim from the reference plugin's stepProgress).
// Because `util.Percent` rounds to 5 decimal places and the comparison is
// FP-exact, the effective boundary on the 5-dp grid is:
//
//	delta rounded to 5dp < 0.001  -> skipped (math.Abs yields < 0.001)
//	delta rounded to 5dp >= 0.001  -> pushed (FP error makes 0.001 itself > 0.001,
//	                                  matching the reference plugin's own
//	                                  behavior with the same double math)
func TestRunOncePercentageToleranceBoundary(t *testing.T) {
	cases := []struct {
		name     string
		cur      float64 // total is fixed at 100000 below
		lastPct  float64
		wantPush bool
	}{
		{"delta well under tolerance (0.0005) is skipped", 50050, 0.5, false},  // pct=0.50050, delta=0.00050
		{"delta just under tolerance (0.00099) is skipped", 50099, 0.5, false}, // pct=0.50099, delta=0.00099
		{"boundary exactly 0.001 is pushed (FP-exact <=)", 50100, 0.5, true},   // pct=0.50100, delta=0.001 (>0.001 in FP)
		{"delta 0.0011 is pushed", 50110, 0.5, true},                           // pct=0.50110, delta=0.00110
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const total = 100000
			row := mkRow("h1", c.cur, total, "2026-01-02T00:00:00Z")
			rd := &fakeReadest{rows: []readest.BookRow{row}}
			bo := &fakeBookOrbit{}
			st := state.NewMemStore()
			// The match record is already set, so the row enters the
			// matched-changed branch directly and never calls MatchCheck; the
			// sole signal of unchanged-vs-changed is whether BulkProgress is
			// called.
			st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 2, LastPushedAt: 1000, LastPushedPct: c.lastPct})
			e := newTestEngine(rd, bo, st)

			if err := e.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if c.wantPush && bo.bulkCalls != 1 {
				t.Errorf("bulkCalls = %d, want 1 (changed)", bo.bulkCalls)
			}
			if !c.wantPush && bo.bulkCalls != 0 {
				t.Errorf("bulkCalls = %d, want 0 (unchanged within tolerance)", bo.bulkCalls)
			}
		})
	}
}

// case 22: a hash that reappears after deletion is treated as brand-new — no
// stale LastPushedPct bleed-through. Pass 1 deletes h1; pass 2 sees h1 again
// as a normal row and re-resolves it, then pushes regardless of the (now-zero)
// stale LastPushedPct.
func TestRunOnceHashReappearsAfterDeletionIsBrandNew(t *testing.T) {
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{Matches: []bookorbit.Match{{Hash: "h1", BookFileID: 1, BookID: 2}}},
	}
	st := state.NewMemStore()
	e := newTestEngine(&fakeReadest{}, bo, st)

	// Pass 1: h1 deleted; the prior match (with a stale LastPushedPct of 0.9)
	// must be cleared. Without this, pass 2's push logic would treat 0.0 vs
	// 0.9 as a real delta rather than a brand-new book.
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 2, LastPushedAt: 1000, LastPushedPct: 0.9})
	rd1 := &fakeReadest{rows: []readest.BookRow{{
		BookHash:  "h1",
		DeletedAt: "2026-01-05T00:00:00Z",
		UpdatedAt: "2026-01-05T00:00:00Z",
	}}}
	e.rd = rd1
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 1 (delete): %v", err)
	}
	if _, merr := st.Match("h1"); !errors.Is(merr, state.ErrNotFound) {
		t.Fatalf("pass 1 should clear the match, got %v", merr)
	}

	// Pass 2: h1 reappears at pct=0.0. Because the stale record was cleared,
	// h1 has no LastPushedPct to compare against — it must enter the
	// match-check path and, once matched, be pushed unconditionally.
	rd2 := &fakeReadest{rows: []readest.BookRow{mkRow("h1", 0, 100, "2026-01-06T00:00:00Z")}}
	e.rd = rd2
	bo.matchCalls, bo.bulkCalls = 0, 0
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass 2 (reappear): %v", err)
	}
	if bo.matchCalls != 1 {
		t.Errorf("pass 2 matchCalls = %d, want 1 (re-resolved as brand-new)", bo.matchCalls)
	}
	if bo.bulkCalls != 1 {
		t.Errorf("pass 2 bulkCalls = %d, want 1 (pushed despite stale pct)", bo.bulkCalls)
	}
	rec, err := st.Match("h1")
	if err != nil {
		t.Fatalf("pass 2 match lookup: %v", err)
	}
	if rec.LastPushedPct != 0.0 {
		t.Errorf("pass 2 LastPushedPct = %v, want 0.0 (fresh)", rec.LastPushedPct)
	}
}
