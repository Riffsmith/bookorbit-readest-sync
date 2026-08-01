package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
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

	// statusCalls records every SetReadStatus invocation in dispatch order.
	// statusErrs maps bookID -> the error to return for that book (an absent
	// entry succeeds). Both are Phase 9 additions.
	statusCalls []statusCall
	statusErrs  map[int64]error
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

// statusCall records one SetReadStatus invocation (the BookOrbit book ID and
// the token pushed), in dispatch order. Defined next to the fake that records
// them.
type statusCall struct {
	bookID int64
	token  string
}

func (f *fakeBookOrbit) SetReadStatus(ctx context.Context, bookID int64, token string) error {
	f.statusCalls = append(f.statusCalls, statusCall{bookID: bookID, token: token})
	if err, scripted := f.statusErrs[bookID]; scripted {
		return err
	}
	return nil
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

func (s *scriptedBookOrbit) SetReadStatus(ctx context.Context, bookID int64, token string) error {
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
	// with the bridge-specific values from §6.2 (Source="file",
	// MetadataAmbiguous=false, LastOpen = updated_at in Unix seconds).
	if len(bo.matchReq.Books) != 1 {
		t.Fatalf("matchReq.Books len = %d, want 1", len(bo.matchReq.Books))
	}
	cand := bo.matchReq.Books[0]
	if cand.Hash != "h1" {
		t.Errorf("candidate Hash = %q, want h1", cand.Hash)
	}
	if cand.Source != "file" {
		t.Errorf("candidate Source = %q, want file", cand.Source)
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

func TestRunOnceAbsentFromMatchResponseIsUnmatchedNotFailure(t *testing.T) {
	// Phase 6 ADR Addendum 2 regression guard: a hash the live BookOrbit
	// server returns in neither resp.Matches nor resp.Unmatched (the common
	// case for Readest-owned books BookOrbit's library has never seen) must
	// be treated as unmatched per bookorbit_sweep.lua:328-332, not as a
	// batch-level failure. The pre-fix engine appended the row's
	// WatermarkMs to failedWatermarks, which retreated the watermark to
	// min-1 forever and produced a per-poll WARN storm; the fix routes
	// this case through SetUnmatched + DeleteMatch, lets the watermark
	// advance to the row's own WatermarkMs, and settles the hash into the
	// UnmatchedCooldown recheck gate.
	row := mkRow("h1", 50, 100, "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	// matchResp left zero-valued: both Matches and Unmatched are empty, so
	// "h1" is absent from both response lists. This is the live-server
	// behavior the original §6.3 contract mischaracterized as "defensive."
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	// No prior MatchRecord, no prior UnmatchedAt — a brand-new Readest
	// row the engine has never classified. This is the canonical entry
	// point to the match-check phase (engine.go case state.ErrNotFound).
	e := newTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.matchCalls != 1 {
		t.Errorf("matchCalls = %d, want 1 (fresh hash, no prior UnmatchedAt)", bo.matchCalls)
	}
	if bo.bulkCalls != 0 {
		t.Errorf("bulkCalls = %d, want 0 (no push for unmatched)", bo.bulkCalls)
	}
	// The hash must now be in the unmatched cache with a fresh timestamp.
	at, ok := st.UnmatchedAt("h1")
	if !ok {
		t.Fatal("absent-from-both hash should be SetUnmatched, got no UnmatchedAt")
	}
	if at == 0 {
		t.Error("fresh Unmatched timestamp should be non-zero")
	}
	// Defensive: no MatchRecord should exist for this hash. (It never did,
	// but this asserts DeleteMatch was a safe no-op, not a skipped call.)
	if _, merr := st.Match("h1"); !errors.Is(merr, state.ErrNotFound) {
		t.Errorf("absent-from-both path must leave no MatchRecord, got Match err = %v", merr)
	}
	// Watermark advances to the row's own WatermarkMs (no retreat). The
	// pre-fix bug retreated to WatermarkMs-1 via failedWatermarks; asserting
	// equality catches any regression by exactly 1ms.
	want := row.WatermarkMs()
	if got := st.Watermark(); got != want {
		t.Errorf("watermark = %d, want %d (advance, not retreat; old bug retreated to %d)",
			got, want, want-1)
	}

	// Second RunOnce within the freshly established cooldown must not
	// resubmit the same hash to match-check — it settles into the
	// UnmatchedCooldown recheck gate (24h default), eliminating the
	// per-poll re-pull/re-match storm the old contract produced.
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

// --- Phase 9: reading-status sync -------------------------------------------
// The status step rides on the resolve the match-check phase already produced
// (no status-triggered match-check) and is decoupled from the progress
// watermark (Decision E). mkStatusRow builds a normal progress-carrying row
// with a reading_status value attached; the tests below pre-seed a MatchRecord
// so a row enters the status step already-matched, the common steady-state.

// mkStatusRow returns a progress-carrying row with a reading_status attached.
// total is fixed so the row always has a usable percentage (a row without one
// is skipped by the status step, per the design's "no business asserting a
// status" rule).
func mkStatusRow(hash, status, updatedAt string) readest.BookRow {
	row := mkRow(hash, 10, 100, updatedAt)
	row.ReadingStatus = status
	return row
}

// mkNoProgressRow constructs a BookRow with null progress (the wire form for a
// book the user has never opened) and an optional reading_status. It is the
// Phase 10 regression-test fixture: the user-reported bug was that a
// download-then-Mark-as-finished book (progress=null, reading_status=
// "finished") was silently dropped before status sync could see it. The
// Progress field is left as nil json.RawMessage (identical to a Server-returned
// JSON null), which decodeProgressTuple correctly treats as no usable tuple.
func mkNoProgressRow(hash, status, updatedAt string) readest.BookRow {
	return readest.BookRow{
		BookHash:      hash,
		ReadingStatus: status,
		UpdatedAt:     updatedAt,
		SyncedAt:      updatedAt, // ensures WatermarkMs picks up the row
		Title:         "Test Book " + hash,
	}
}

// newStatusTestEngine builds an engine with SyncStatus enabled. Tests that
// specifically exercise the gate use newTestEngine (SyncStatus=false default)
// instead.
func newStatusTestEngine(rd *fakeReadest, bo bookorbit.API, st state.Store) *Engine {
	e := newTestEngine(rd, bo, st)
	e.cfg.Bridge.SyncStatus = true
	return e
}

// Table test for the pure mapper, mirroring the existing TestComputeWatermark
// shape. Cases: the two decisive pushes, the decisive-no-op unread, the
// non-decisive reading/empty, and one unrecognized-value case.
func TestMapReadingStatus(t *testing.T) {
	cases := []struct {
		in       string
		wantTok  string
		wantPush bool
	}{
		{"finished", "read", true},
		{"abandoned", "abandoned", true},
		{"unread", "", false},  // decisive no-op: unread is BookOrbit's baseline
		{"reading", "", false}, // non-decisive: never synced
		{"", "", false},        // absent / "New": non-decisive
		{"on_hold", "", false}, // unrecognized future value: fail-safe skip
		{"skimmed", "", false}, // any other schema-drift value: fail-safe skip
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			tok, push := mapReadingStatus(c.in)
			if tok != c.wantTok || push != c.wantPush {
				t.Errorf("mapReadingStatus(%q) = (%q,%v), want (%q,%v)", c.in, tok, push, c.wantTok, c.wantPush)
			}
		})
	}
}

// Gate: with SyncStatus disabled, the status step never runs at all, no matter
// what reading_status the rows carry — zero SetReadStatus calls.
func TestRunOnceStatusGateOffNeverPushes(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{
		mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z"),
		mkStatusRow("h2", "abandoned", "2026-01-02T00:00:00Z"),
	}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	st.SetMatch("h2", state.MatchRecord{BookFileID: 2, BookID: 22})
	e := newTestEngine(rd, bo, st) // SyncStatus defaults to false

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 0 {
		t.Errorf("statusCalls = %d, want 0 (gate off)", len(bo.statusCalls))
	}
}

// Fresh decisive push: a matched book with a decisive status never pushed
// before produces exactly one SetReadStatus call with the mapped token, and
// both the seen and pushed bookkeeping pairs are recorded.
func TestRunOnceStatusFreshFinishedPushes(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 1 {
		t.Fatalf("statusCalls = %d, want 1", len(bo.statusCalls))
	}
	if bo.statusCalls[0].bookID != 11 || bo.statusCalls[0].token != "read" {
		t.Errorf("SetReadStatus(%d,%q), want (11,read)", bo.statusCalls[0].bookID, bo.statusCalls[0].token)
	}
	rec, _ := st.Match("h1")
	if rec.LastSeenStatus != "finished" || rec.LastPushedStatus != "read" {
		t.Errorf("record = %+v, want seen=finished pushed=read", rec)
	}
	if rec.LastSeenStatusAt == 0 || rec.LastPushedStatusAt == 0 {
		t.Error("seen/pushed status timestamps should be non-zero")
	}
}

// Token-identical passthrough: abandoned maps to itself.
func TestRunOnceStatusAbandonedPassthrough(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "abandoned", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 1 || bo.statusCalls[0].token != "abandoned" {
		t.Errorf("statusCalls = %+v, want one (abandoned) call", bo.statusCalls)
	}
}

// Unchanged status: a book whose mapped token already equals LastPushedStatus
// makes no call (a strict no-op per the design; the pushed side is current).
func TestRunOnceStatusUnchangedSkips(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11, LastPushedStatus: "read"})
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 0 {
		t.Errorf("statusCalls = %d, want 0 (unchanged)", len(bo.statusCalls))
	}
}

// Non-decisive values ("reading" and "") never produce a call, regardless of
// what's in LastPushedStatus. The engine must skip, not just the mapper.
func TestRunOnceStatusNonDecisiveNeverPushes(t *testing.T) {
	for _, status := range []string{"reading", ""} {
		t.Run(status, func(t *testing.T) {
			rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", status, "2026-01-02T00:00:00Z")}}
			bo := &fakeBookOrbit{}
			st := state.NewMemStore()
			// LastPushedStatus is "read": a non-decisive value must not push
			// anything even though it differs from the pushed token.
			st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11, LastPushedStatus: "read"})
			e := newStatusTestEngine(rd, bo, st)

			if err := e.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(bo.statusCalls) != 0 {
				t.Errorf("statusCalls = %d, want 0 for non-decisive %q", len(bo.statusCalls), status)
			}
		})
	}
}

// unread is a decisive no-op: no SetReadStatus call, BUT LastSeenStatus still
// updates so a later unread→finished transition diffs against the right
// baseline (Decision A; regression guard for stale-comparison bugs).
func TestRunOnceStatusUnreadRecordsSeenButDoesNotPush(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "unread", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 0 {
		t.Errorf("statusCalls = %d, want 0 (unread is a no-op push)", len(bo.statusCalls))
	}
	rec, _ := st.Match("h1")
	if rec.LastSeenStatus != "unread" {
		t.Errorf("LastSeenStatus = %q, want unread (recorded for later diff)", rec.LastSeenStatus)
	}
	if rec.LastSeenStatusAt == 0 {
		t.Error("LastSeenStatusAt should be non-zero after recording unread")
	}
	// Pushed side stays untouched: unread is never written to BookOrbit.
	if rec.LastPushedStatus != "" {
		t.Errorf("LastPushedStatus = %q, want empty (unread pushed nothing)", rec.LastPushedStatus)
	}
}

// The unread→finished transition: phase 1 records unread (seen only); phase 2
// sees finished, which — because the seen baseline is "unread", not "" — is
// recognized as new and pushed. This is the property Decision A exists for.
func TestRunOnceStatusUnreadThenFinishedTransitions(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "unread", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("phase 1: %v", err)
	}
	if len(bo.statusCalls) != 0 {
		t.Fatalf("phase 1 statusCalls = %d, want 0", len(bo.statusCalls))
	}

	rd.rows = []readest.BookRow{mkStatusRow("h1", "finished", "2026-01-03T00:00:00Z")}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("phase 2: %v", err)
	}
	if len(bo.statusCalls) != 1 || bo.statusCalls[0].token != "read" {
		t.Errorf("phase 2 statusCalls = %+v, want one (read) call", bo.statusCalls)
	}
}

// Unmatched book (no MatchRecord, no BookID): the status step skips it
// entirely, with no panic and no call.
func TestRunOnceStatusUnmatchedBookSkipped(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z")}}
	// matchResp returns no match for h1 -> it stays unmatched this poll.
	bo := &fakeBookOrbit{matchResp: bookorbit.MatchCheckResponse{Unmatched: []string{"h1"}}}
	st := state.NewMemStore()
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 0 {
		t.Errorf("statusCalls = %d, want 0 (book unmatched)", len(bo.statusCalls))
	}
}

// ErrBookGone: a stale cached bookId (404) drops the book from the sync set —
// DeleteMatch + SetUnmatched — and explicitly does NOT touch the progress
// watermark (Decision E decoupling).
func TestRunOnceStatusBookGoneDropsFromSyncSet(t *testing.T) {
	row := mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{statusErrs: map[int64]error{11: bookorbit.ErrBookGone}}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v (book-gone is not a RunOnce error)", err)
	}
	// Dropped from the sync set.
	if _, merr := st.Match("h1"); !errors.Is(merr, state.ErrNotFound) {
		t.Errorf("book-gone must DeleteMatch, got %v", merr)
	}
	if _, ok := st.UnmatchedAt("h1"); !ok {
		t.Error("book-gone must SetUnmatched (cooldown recheck gate)")
	}
	// Progress watermark advances normally — NOT retreated (Decision E).
	want := row.WatermarkMs()
	if got := st.Watermark(); got != want {
		t.Errorf("watermark = %d, want %d (unaffected by book-gone)", got, want)
	}
}

// Retryable status failure, retries exhausted: LastPushedStatus is NOT
// advanced, the progress watermark/push still advance normally, and the next
// poll picks the status up again because the mapped value still differs. This
// is the direct pin for Decision E's decoupling.
func TestRunOnceStatusFailureDecoupledFromProgress(t *testing.T) {
	row := mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z")
	rd := &fakeReadest{rows: []readest.BookRow{row}}
	bo := &fakeBookOrbit{statusErrs: map[int64]error{11: bookorbit.ErrServer}}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0 // one attempt, exhaust immediately

	err := e.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce should surface the status failure as an error")
	}
	rec, _ := st.Match("h1")
	if rec.LastPushedStatus == "read" {
		t.Error("LastPushedStatus must NOT advance on a failed status push")
	}
	// Progress decoupling: the progress push for this same row succeeded and
	// its watermark advanced to the row's WatermarkMs (not retreated).
	want := row.WatermarkMs()
	if got := st.Watermark(); got != want {
		t.Errorf("watermark = %d, want %d (progress unaffected by status failure)", got, want)
	}
	if rec.LastPushedAt == 0 {
		t.Error("progress LastPushedAt should advance even though status failed")
	}

	// Next poll: the failure retried because the mapped value still differs.
	bo.statusErrs = nil
	bo.statusCalls = nil
	rd.rows = []readest.BookRow{mkStatusRow("h1", "finished", "2026-01-03T00:00:00Z")}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("retry RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 1 || bo.statusCalls[0].token != "read" {
		t.Errorf("retry statusCalls = %+v, want one (read) call", bo.statusCalls)
	}
}

// ErrUnauthorized: aborts the whole RunOnce, preserving prior committed
// mutations, with Save still running once — mirroring the existing auth-abort
// behavior for the progress phases.
func TestRunOnceStatusUnauthorizedAbortsAndSavesOnce(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{
		mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z"),
		mkStatusRow("h2", "finished", "2026-01-02T00:00:00Z"),
	}}
	// h1's push (bookID 11) succeeds; h2's push (bookID 22) is unauthorized.
	bo := &fakeBookOrbit{statusErrs: map[int64]error{22: bookorbit.ErrUnauthorized}}
	inner := state.NewMemStore()
	st := &saveCountingStore{Store: inner}
	inner.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	inner.SetMatch("h2", state.MatchRecord{BookFileID: 2, BookID: 22})
	e := newStatusTestEngine(rd, bo, st)
	e.cfg.Bridge.RetryMaxAttempts = 0

	err := e.RunOnce(context.Background())
	if !errors.Is(err, bookorbit.ErrUnauthorized) {
		t.Errorf("err = %v, want wrapped ErrUnauthorized", err)
	}
	if st.saves != 1 {
		t.Errorf("saves = %d, want 1 (still saved once on auth abort)", st.saves)
	}
	// h1's earlier successful push is preserved.
	if rec, _ := inner.Match("h1"); rec.LastPushedStatus != "read" {
		t.Errorf("h1 LastPushedStatus = %q, want read (preserved before abort)", rec.LastPushedStatus)
	}
	// h2's push never committed.
	if rec, _ := inner.Match("h2"); rec.LastPushedStatus != "" {
		t.Errorf("h2 LastPushedStatus = %q, want empty (aborted before commit)", rec.LastPushedStatus)
	}
}

// Mixed multi-book batch: one push (fresh finished), one skip (unchanged
// abandoned), one drop (ErrBookGone), one non-decisive skip (reading), all in
// one pass, processed independently.
func TestRunOnceStatusMixedBatch(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{
		mkStatusRow("push", "finished", "2026-01-02T00:00:00Z"),
		mkStatusRow("skip", "abandoned", "2026-01-02T00:00:00Z"),
		mkStatusRow("drop", "finished", "2026-01-02T00:00:00Z"),
		mkStatusRow("nodecisive", "reading", "2026-01-02T00:00:00Z"),
	}}
	bo := &fakeBookOrbit{statusErrs: map[int64]error{33: bookorbit.ErrBookGone}}
	st := state.NewMemStore()
	st.SetMatch("push", state.MatchRecord{BookFileID: 1, BookID: 11})                                // fresh: push
	st.SetMatch("skip", state.MatchRecord{BookFileID: 2, BookID: 22, LastPushedStatus: "abandoned"}) // unchanged: skip
	st.SetMatch("drop", state.MatchRecord{BookFileID: 3, BookID: 33})                                // gone: drop
	st.SetMatch("nodecisive", state.MatchRecord{BookFileID: 4, BookID: 44})                          // reading: no-op
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Two SetReadStatus calls are made: the fresh "push" book (11, read) and
	// the "drop" book (33, read) — the drop book's call fails with ErrBookGone
	// and the book is then removed from the sync set. The unchanged "skip" and
	// non-decisive "reading" books make no call.
	wantBookIDs := map[int64]string{11: "read", 33: "read"}
	if len(bo.statusCalls) != len(wantBookIDs) {
		t.Fatalf("statusCalls = %+v, want %d calls", bo.statusCalls, len(wantBookIDs))
	}
	for _, c := range bo.statusCalls {
		if wantToken, ok := wantBookIDs[c.bookID]; !ok || c.token != wantToken {
			t.Errorf("unexpected SetReadStatus(%d,%q)", c.bookID, c.token)
		}
	}
	if _, merr := st.Match("drop"); !errors.Is(merr, state.ErrNotFound) {
		t.Error("drop book should be removed from the sync set")
	}
	if rec, _ := st.Match("push"); rec.LastPushedStatus != "read" {
		t.Errorf("push LastPushedStatus = %q, want read", rec.LastPushedStatus)
	}
	if rec, _ := st.Match("skip"); rec.LastPushedStatus != "abandoned" {
		t.Errorf("skip LastPushedStatus = %q, want unchanged abandoned", rec.LastPushedStatus)
	}
	if rec, _ := st.Match("nodecisive"); rec.LastPushedStatus != "" {
		t.Errorf("nodecisive LastPushedStatus = %q, want empty", rec.LastPushedStatus)
	}
}

// Save() is still called exactly once per RunOnce with the status step present.
func TestRunOnceStatusSavesExactlyOnce(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "finished", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	inner := state.NewMemStore()
	inner.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	st := &saveCountingStore{Store: inner}
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.saves != 1 {
		t.Errorf("saves = %d, want 1 (still exactly once with the status step)", st.saves)
	}
	if len(bo.statusCalls) != 1 {
		t.Errorf("statusCalls = %d, want 1", len(bo.statusCalls))
	}
}

// Unrecognized status value: warn exactly once per distinct value, silent on
// repeat, and no bookkeeping update (a token we don't understand isn't a
// status we can claim to have processed). Captures logs via a slog test
// handler on a dedicated buffer.
func TestRunOnceStatusUnrecognizedWarnsOnce(t *testing.T) {
	var buf statusLogBuf
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})

	rd := &fakeReadest{rows: []readest.BookRow{mkStatusRow("h1", "on_hold", "2026-01-02T00:00:00Z")}}
	bo := &fakeBookOrbit{}
	st := state.NewMemStore()
	st.SetMatch("h1", state.MatchRecord{BookFileID: 1, BookID: 11})
	e := newStatusTestEngine(rd, bo, st)
	e.log = slog.New(handler)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce: %v", err)
	}
	if n := buf.countWarnsAbout("on_hold"); n != 1 {
		t.Errorf("first run warns about on_hold %d times, want exactly 1", n)
	}
	if len(bo.statusCalls) != 0 {
		t.Errorf("statusCalls = %d, want 0 (unrecognized value never pushes)", len(bo.statusCalls))
	}
	// No bookkeeping recorded for a value we don't understand.
	if rec, _ := st.Match("h1"); rec.LastSeenStatus != "" || rec.LastPushedStatus != "" {
		t.Errorf("bookkeeping = %+v, want untouched for an unrecognized value", rec)
	}

	// Second occurrence of the same value is silent.
	before := buf.len()
	rd.rows = []readest.BookRow{mkStatusRow("h1", "on_hold", "2026-01-03T00:00:00Z")}
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if buf.len() != before {
		t.Error("second occurrence of the same unrecognized value must not re-warn")
	}
}

// statusLogBuf is a bytes.Buffer-like sink for the warn-once test. The engine
// logs through slog; a *slog.TextHandler writing here lets the test count how
// many WARN lines mention a given token without parsing structured records.
type statusLogBuf struct {
	mu sync.Mutex
	b  []byte
}

func (b *statusLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b = append(b.b, p...)
	return len(p), nil
}

func (b *statusLogBuf) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.b)
}

func (b *statusLogBuf) countWarnsAbout(substr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines := 0
	for _, line := range strings.Split(string(b.b), "\n") {
		if strings.Contains(line, "on_hold") && strings.Contains(line, substr) && strings.Contains(line, "unrecognized") {
			lines++
		}
	}
	return lines
}

// --- Phase 10 regression guards: null-progress decisive-status rows ---
//
// Phase 10 fixes a two-layer bug surfaced by the operator against a live
// Readest library: a book downloaded from BookOrbit's OPDS and then marked
// "finished" in Readest without ever being opened renders on the wire as
// {progress: null, reading_status: "finished"}. The shipped Phase 9 engine
// skipped such rows at the row-classification phase (before match-check)
// AND inside pushStatuses, so the decisive status was never matched, never
// pushed, and — once the watermark advanced past the row's synced_at — never
// visible to incremental pulls again. These three tests pin the post-Phase-10
// contract: status sync is decoupled from progress; a null-progress decisive
// row reaches match-check through the status path; the status step accepts
// it without re-imposing the progress gate.

// TestRunOnceNullProgressFinishedBookStatusPushes is the regression guard
// for the operator's exact reported scenario. A row with progress=null and
// reading_status="finished" must, in one RunOnce with SyncStatus enabled:
//
//   - reach MatchCheck exactly once (via the statusEligible path; the
//     progress-eligible path correctly does nothing because there is no
//     progress tuple to push),
//   - produce exactly one SetReadStatus call with the mapped (bookID, "read"),
//   - record LastSeenStatus="finished" and LastPushedStatus="read" on the
//     MatchRecord, with LastPushedPct left at zero (no progress was ever
//     pushed for this book),
//   - make zero BulkProgress / UpdateProgress calls (no progress data to
//     push), and
//   - on a second RunOnce with the same rows, make zero further calls
//     (the unchanged-skip in the status step fires because LastPushedStatus
//     already equals "read").
func TestRunOnceNullProgressFinishedBookStatusPushes(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{
		mkNoProgressRow("h1", "finished", "2026-08-01T08:44:39Z"),
	}}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{
			Matches: []bookorbit.Match{
				{Hash: "h1", BookFileID: 1, BookID: 11},
			},
		},
	}
	st := state.NewMemStore()
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if bo.matchCalls != 1 {
		t.Errorf("matchCalls = %d, want 1 (null-progress decisive row must reach MatchCheck via status path)", bo.matchCalls)
	}
	if bo.bulkCalls != 0 || bo.updateCalls != 0 {
		t.Errorf("progress calls = bulk:%d update:%d, want 0/0 (no progress tuple means no progress push)", bo.bulkCalls, bo.updateCalls)
	}
	if len(bo.statusCalls) != 1 {
		t.Fatalf("statusCalls = %d, want 1", len(bo.statusCalls))
	}
	if bo.statusCalls[0].bookID != 11 || bo.statusCalls[0].token != "read" {
		t.Errorf("SetReadStatus(%d,%q), want (11,read)", bo.statusCalls[0].bookID, bo.statusCalls[0].token)
	}
	rec, merr := st.Match("h1")
	if merr != nil {
		t.Fatalf("Match(h1) after RunOnce: %v", merr)
	}
	if rec.LastSeenStatus != "finished" {
		t.Errorf("LastSeenStatus = %q, want finished", rec.LastSeenStatus)
	}
	if rec.LastPushedStatus != "read" {
		t.Errorf("LastPushedStatus = %q, want read", rec.LastPushedStatus)
	}
	if rec.LastPushedPct != 0 {
		t.Errorf("LastPushedPct = %v, want 0 (no progress ever pushed for this book)", rec.LastPushedPct)
	}
	if rec.LastPushedAt != 0 {
		t.Errorf("LastPushedAt = %v, want 0 (no progress ever pushed for this book)", rec.LastPushedAt)
	}

	// Second run: status unchanged → unchanged-skip fires; zero calls of any
	// kind (no progress to push, no status change, no need to recheck match).
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if bo.matchCalls != 1 {
		t.Errorf("matchCalls = %d after second run, want 1 (already-matched, in-cooldown-or-current)", bo.matchCalls)
	}
	if bo.bulkCalls != 0 || bo.updateCalls != 0 {
		t.Errorf("progress calls after second run = bulk:%d update:%d, want 0/0", bo.bulkCalls, bo.updateCalls)
	}
	if len(bo.statusCalls) != 1 {
		t.Errorf("statusCalls = %d after second run, want 1 (unchanged-skip should fire; no new call)", len(bo.statusCalls))
	}
}

// TestRunOnceNullProgressThenOpensBookPushesProgressToo tests the decoupling
// "future-tense" direction: a book that was status-pushed on a null-progress
// poll, then later opened by the reader and progressed, should have BOTH
// (a) its progress pushed (LastPushedPct was 0 → real pct differs by more
// than the tolerance, so the progress path enqueues a push), AND (b) its
// status unchanged-skipped (LastPushedStatus already equals "read"). The
// state set during Run 1's status phase must NOT be reset by Run 2's progress
// push — the Phase 9 ADR's "preserve status bookkeeping on progress push"
// invariant continues to hold for null-progress-then-progressed rows.
func TestRunOnceNullProgressThenOpensBookPushesProgressToo(t *testing.T) {
	// Run 1: null progress, reading_status="finished"
	rd := &fakeReadest{rows: []readest.BookRow{
		mkNoProgressRow("h1", "finished", "2026-08-01T08:44:39Z"),
	}}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{
			Matches: []bookorbit.Match{
				{Hash: "h1", BookFileID: 1, BookID: 11},
			},
		},
	}
	st := state.NewMemStore()
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("phase 1 RunOnce: %v", err)
	}
	if len(bo.statusCalls) != 1 || bo.statusCalls[0].token != "read" {
		t.Errorf("phase 1 statusCalls = %+v, want one (read) call", bo.statusCalls)
	}
	if bo.bulkCalls != 0 || bo.updateCalls != 0 {
		t.Errorf("phase 1 progress pushes = bulk:%d update:%d, want 0/0 (no progress yet)", bo.bulkCalls, bo.updateCalls)
	}

	// Run 2: same book, now the reader turned pages → progress=[3, 5167].
	// reading_status="finished" unchanged. The match is cached, so no
	// match-check should run; only the progress-push path should fire.
	matchCallsBeforeRun2 := bo.matchCalls
	rd.rows = []readest.BookRow{
		{
			BookHash:      "h1",
			Progress:      json.RawMessage("[3,5167]"),
			ReadingStatus: "finished",
			UpdatedAt:     "2026-08-01T10:00:00Z",
			SyncedAt:      "2026-08-01T10:00:01Z",
			Title:         "Test Book h1",
		},
	}
	bo.statusCalls = nil // reset for run 2 assertions
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("phase 2 RunOnce: %v", err)
	}
	if bo.matchCalls != matchCallsBeforeRun2 {
		t.Errorf("phase 2 matchCalls = %d, want %d (match cached, no recheck)", bo.matchCalls, matchCallsBeforeRun2)
	}
	if len(bo.statusCalls) != 0 {
		t.Errorf("phase 2 statusCalls = %d, want 0 (LastPushedStatus==read → unchanged-skip)", len(bo.statusCalls))
	}
	// Progress must have been pushed: Phase 6 path, BulkProgress with one item.
	if bo.bulkCalls != 1 {
		t.Errorf("phase 2 bulkCalls = %d, want 1 (pct moved from 0 to ~0.00058)", bo.bulkCalls)
	}
	if bo.updateCalls != 0 {
		t.Errorf("phase 2 updateCalls = %d, want 0 (bulk endpoint is supported in this fake)", bo.updateCalls)
	}
	if len(bo.bulkItems) != 1 || bo.bulkItems[0].Hash != "h1" {
		t.Errorf("phase 2 bulkItems = %+v, want one item for h1", bo.bulkItems)
	}
	// The crucial Phase 9 invariant (preserved by Phase 10): the progress
	// push must not have reset the status bookkeeping. LastPushedStatus must
	// still be "read"; only LastPushedPct/LastPushedAt advanced.
	rec, merr := st.Match("h1")
	if merr != nil {
		t.Fatalf("Match(h1) after phase 2: %v", merr)
	}
	if rec.LastPushedStatus != "read" {
		t.Errorf("phase 2 LastPushedStatus = %q, want read (progress push must not reset status)", rec.LastPushedStatus)
	}
	if rec.LastSeenStatus != "finished" {
		t.Errorf("phase 2 LastSeenStatus = %q, want finished (progress push must not reset seen)", rec.LastSeenStatus)
	}
	if rec.LastPushedPct == 0 {
		t.Error("phase 2 LastPushedPct = 0, want non-zero (progress was just pushed)")
	}
}

// TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch pins
// Decision II from the Phase 10 design: a row eligible for EITHER progress OR
// status sync is enqueued into a single unified MatchCheck batch — never two
// batches, never one row missing the queue.
//
//   - Row A: progress-bearing, no status → progress-eligible only
//   - Row B: null progress, reading_status="finished" → status-eligible only
//   - Row C: progress-bearing AND reading_status="finished" → both eligible
//     (must be deduped to a single MatchCheck entry)
//
// Expected: exactly one MatchCheck call with all three unique hashes; one
// BulkProgress call containing exactly rows A and C (B has no progress to
// push); one (or two) SetReadStatus calls for B and C (A has no decisive
// status).
func TestRunOnceMixedProgressAndStatusRowsUseSingleMatchCheckBatch(t *testing.T) {
	rd := &fakeReadest{rows: []readest.BookRow{
		{
			BookHash:  "hA",
			Progress:  json.RawMessage("[5,100]"),
			UpdatedAt: "2026-08-01T08:00:00Z",
			SyncedAt:  "2026-08-01T08:00:01Z",
			Title:     "A progress only",
		},
		{
			BookHash:      "hB",
			Progress:      nil, // null progress — the operator's scenario
			ReadingStatus: "finished",
			UpdatedAt:     "2026-08-01T08:00:00Z",
			SyncedAt:      "2026-08-01T08:00:01Z",
			Title:         "B status only",
		},
		{
			BookHash:      "hC",
			Progress:      json.RawMessage("[7,200]"),
			ReadingStatus: "finished",
			UpdatedAt:     "2026-08-01T08:00:00Z",
			SyncedAt:      "2026-08-01T08:00:01Z",
			Title:         "C both",
		},
	}}
	bo := &fakeBookOrbit{
		matchResp: bookorbit.MatchCheckResponse{
			Matches: []bookorbit.Match{
				{Hash: "hA", BookFileID: 1, BookID: 11},
				{Hash: "hB", BookFileID: 2, BookID: 22},
				{Hash: "hC", BookFileID: 3, BookID: 33},
			},
		},
	}
	st := state.NewMemStore()
	e := newStatusTestEngine(rd, bo, st)

	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Decision II: one MatchCheck call carrying all three unique hashes.
	if bo.matchCalls != 1 {
		t.Fatalf("matchCalls = %d, want 1 (unified batch)", bo.matchCalls)
	}
	if len(bo.matchReq.Hashes) != 3 {
		t.Errorf("matchReq.Hashes length = %d, want 3 (A,B,C deduped into one batch)", len(bo.matchReq.Hashes))
	}
	seenHashes := map[string]bool{}
	for _, h := range bo.matchReq.Hashes {
		seenHashes[h] = true
	}
	for _, want := range []string{"hA", "hB", "hC"} {
		if !seenHashes[want] {
			t.Errorf("matchReq.Hashes missing %q (got %+v)", want, bo.matchReq.Hashes)
		}
	}

	// Progress: only A and C pushed (B has no progress tuple).
	if bo.bulkCalls != 1 {
		t.Errorf("bulkCalls = %d, want 1 (single bulk batch with A and C)", bo.bulkCalls)
	}
	if len(bo.bulkItems) != 2 {
		t.Errorf("bulkItems count = %d, want 2 (hA and hC only; hB has no progress)", len(bo.bulkItems))
	}
	pushedHashes := map[string]bool{}
	for _, item := range bo.bulkItems {
		pushedHashes[item.Hash] = true
	}
	if !pushedHashes["hA"] || !pushedHashes["hC"] {
		t.Errorf("bulkItems = %+v, want hA and hC pushed only", bo.bulkItems)
	}
	if pushedHashes["hB"] {
		t.Error("hB (null progress) must not appear in bulkItems at all")
	}

	// Status: only B and C pushed (A has no decisive reading_status).
	statusCallBookIDs := map[int64]bool{}
	for _, sc := range bo.statusCalls {
		statusCallBookIDs[sc.bookID] = true
	}
	if len(bo.statusCalls) != 2 {
		t.Errorf("statusCalls count = %d, want 2 (hB and hC; hA has no decisive status)", len(bo.statusCalls))
	}
	if !statusCallBookIDs[22] || !statusCallBookIDs[33] {
		t.Errorf("statusCall bookIDs = %+v, want bookIDs 22 (hB) and 33 (hC)", statusCallBookIDs)
	}
	if statusCallBookIDs[11] {
		t.Error("bookId 11 (hA) should not have a status call (hA.ReadingStatus is empty)")
	}
}
