// Package sync holds the orchestration layer and the persistent state store.
// The engine is the only place that knows about both Readest and BookOrbit;
// the state store is the only persistence the bridge uses.
//
// Engine.RunOnce implements the full pull -> classify -> match-check ->
// bulk-progress pipeline described in docs/phase-6-design.md. Neither
// internal/readest.Client nor internal/bookorbit.Client retries on its own —
// both document this explicitly — so all retry/backoff policy, all batching,
// all watermark math, and the bulk-progress -> UpdateProgress fallback policy
// live here.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/bookorbit"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/config"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/readest"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/sync/state"
	"github.com/Riffsmith/bookorbit-readest-sync/internal/util"
)

// percentTolerance is the unchanged-percentage skip tolerance, matching the
// reference plugin's bookorbit_book_sync.lua:stepProgress
// (math.abs(pct - pushed) <= 0.001), verified in docs/phase-6-design.md §2.
const percentTolerance = 0.001

// mapReadingStatus is the Go port of the reference plugin's
// library/readingstatus.lua READEST_TO_KO mapping collapsed to the one-way,
// Channel-B-only shape the Phase 9 design defines. It returns the BookOrbit
// Channel-B token to push and whether to push it at all:
//
//	"finished"   → ("read", true)         — exact semantic match
//	"abandoned"  → ("abandoned", true)    — token-identical
//	"unread"     → ("", false)            — decisive, but a documented no-op:
//	                                          Channel B has no settable "unread"
//	                                          token; unread is BookOrbit's
//	                                          server-side baseline.
//	"" / "reading" → ("", false)          — non-decisive: never synced, because
//	                                          KOReader auto-sets "reading" on
//	                                          first open and pushing it would
//	                                          downgrade a finished book
//	                                          (readingstatus.lua:5-9).
//	anything else (incl. a future "on_hold") → ("", false) — fail-safe skip.
//
// The function is deliberately pure and state-free; the per-process
// warn-once-per-unrecognized-value logging (Decision D) lives in the engine's
// status step, not here.
func mapReadingStatus(readestStatus string) (token string, push bool) {
	switch readestStatus {
	case "finished":
		return "read", true
	case "abandoned":
		return "abandoned", true
	default:
		// "unread", "" / "reading", and any unrecognized future value all map
		// to no-push. The caller distinguishes the warn-worthy case from the
		// known non-decisive cases for logging.
		return "", false
	}
}

// Engine coordinates a one-way sync from Readest to BookOrbit.
type Engine struct {
	cfg    config.Config
	rdAuth readest.Authenticator
	rd     readest.SyncClient
	bo     bookorbit.API
	st     state.Store
	log    *slog.Logger

	// bulkUnsupported is set once BulkProgress returns
	// bookorbit.ErrUnsupportedEndpoint. It is in-memory only and deliberately
	// not persisted (Decision F): rediscovering it once per process restart
	// is cheap and avoids a state.Data schema change for a condition expected
	// to be rare.
	bulkUnsupported bool

	// warnedStatusValues is the per-process dedup set for unrecognized Readest
	// reading_status tokens (Phase 9, Decision D): each distinct unrecognized
	// value is logged at WARN once, then silently skipped on subsequent
	// appearances. Process-lifetime only (a restart re-warns once), matching
	// the run-scoped bulkUnsupported field above rather than the persisted
	// state store. Initialized in NewEngine; never nil.
	warnedStatusValues map[string]bool

	// now and sleep are injectable for deterministic tests, mirroring the
	// convention already used by readest.Auth.now and bookorbit.Client.now.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// NewEngine constructs an Engine from its dependencies. All parameters are
// required except rdAuth, which the engine does not call directly:
// readest.Client.PullBooks already owns its own token freshness and
// downstream-401 handling internally, so the engine has no independent use
// for it, but the field is kept because the constructor's public signature is
// unchanged from the foundation phase.
func NewEngine(
	cfg config.Config,
	rdAuth readest.Authenticator,
	rd readest.SyncClient,
	bo bookorbit.API,
	st state.Store,
	log *slog.Logger,
) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:    cfg,
		rdAuth: rdAuth,
		rd:     rd,
		bo:     bo,
		st:     st,
		log:    log,
		now:    time.Now,
		sleep:  defaultSleep,
		// Initialized empty; the status step populates it lazily on the first
		// unrecognized Readest reading_status token it encounters.
		warnedStatusValues: make(map[string]bool),
	}
}

// PollInterval exposes the configured cadence for callers (e.g., the CLI's
// startup log) without reaching into config internals.
func (e *Engine) PollInterval() time.Duration {
	return e.cfg.Bridge.PollInterval
}

// defaultSleep waits for d, respecting cancellation of ctx.
func defaultSleep(ctx context.Context, d time.Duration) error {
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

// pushItem is a book queued for a BookOrbit progress push: either freshly
// matched (rec.LastPushedAt == 0, always pushed) or previously matched with a
// percentage that has moved beyond percentTolerance.
type pushItem struct {
	hash string
	rec  state.MatchRecord
	pct  float64
	ts   int64 // Unix epoch seconds, the BookOrbit progress timestamp
	wm   int64 // the row's WatermarkMs(), tracked for watermark retreat
}

// RunOnce performs a single sync pass: pull books since the persisted
// watermark, classify each row, resolve unknown hashes via match-check, push
// changed percentages via bulk-progress (or the per-item fallback), and
// advance or retreat the watermark. state.Store.Save() is called exactly
// once, at the end, regardless of outcome (Decision C).
func (e *Engine) RunOnce(ctx context.Context) error {
	since := e.st.Watermark()

	rows, err := e.pullBooks(ctx, since)
	if err != nil {
		return fmt.Errorf("sync: pull books: %w", err)
	}

	var (
		maxWatermark     int64
		failedWatermarks []int64
		errs             []error
		toMatch          []readest.BookRow
		toPush           []pushItem
	)

	now := e.now()

	for _, row := range rows {
		if row.IsDummy() {
			// Excluded from every watermark calculation, matching
			// parseSyncRow's dummy filter (verified, §5.2).
			continue
		}

		wm := row.WatermarkMs()
		if wm > maxWatermark {
			maxWatermark = wm
		}

		if row.IsDeleted() {
			// Decision B: actively reset local tracking rather than merely
			// ignoring the row, so a re-uploaded book starts fresh.
			e.st.DeleteMatch(row.BookHash)
			e.st.ClearUnmatched(row.BookHash)
			continue
		}

		pct, ok := row.Percentage()
		if !ok {
			continue
		}

		rec, merr := e.st.Match(row.BookHash)
		switch {
		case merr == nil:
			if rec.LastPushedAt != 0 && math.Abs(pct-rec.LastPushedPct) <= percentTolerance {
				continue
			}
			toPush = append(toPush, pushItem{
				hash: row.BookHash, rec: rec, pct: pct,
				ts: updatedSeconds(row, now.Unix()), wm: wm,
			})
		case errors.Is(merr, state.ErrNotFound):
			at, inCooldown := e.st.UnmatchedAt(row.BookHash)
			if inCooldown && now.Unix()-at < int64(e.cfg.Bridge.UnmatchedCooldown.Seconds()) {
				continue
			}
			toMatch = append(toMatch, row)
		default:
			e.log.Warn("sync: unexpected state.Match error", "hash", row.BookHash, "error", merr)
		}
	}

	// --- Match-check phase ---
	if len(toMatch) > 0 {
		rowByHash := make(map[string]readest.BookRow, len(toMatch))
		hashes := make([]string, len(toMatch))
		for i, r := range toMatch {
			hashes[i] = r.BookHash
			rowByHash[r.BookHash] = r
		}

		fatal := false
		util.BatchFunc(hashes, e.cfg.Bridge.MatchBatchSize, func(chunk []string) bool {
			candidates := make([]bookorbit.MatchCandidate, len(chunk))
			for i, h := range chunk {
				row := rowByHash[h]
				candidates[i] = bookorbit.MatchCandidate{
					Hash:              h,
					Title:             row.Title,
					Authors:           row.Author,
					LastOpen:          updatedSeconds(row, 0),
					Source:            "file",
					MetadataAmbiguous: false,
				}
			}

			resp, err := e.matchCheck(ctx, bookorbit.MatchCheckRequest{Hashes: chunk, Books: candidates})
			if err != nil {
				errs = append(errs, fmt.Errorf("sync: match-check: %w", err))
				if errors.Is(err, bookorbit.ErrUnauthorized) {
					fatal = true
					return false
				}
				for _, h := range chunk {
					failedWatermarks = append(failedWatermarks, rowByHash[h].WatermarkMs())
				}
				return true
			}

			seen := make(map[string]bool, len(chunk))
			for _, m := range resp.Matches {
				rec := state.MatchRecord{BookFileID: m.BookFileID, BookID: m.BookID}
				e.st.SetMatch(m.Hash, rec)
				e.st.ClearUnmatched(m.Hash)
				seen[m.Hash] = true
				if row, ok := rowByHash[m.Hash]; ok {
					if pct, ok2 := row.Percentage(); ok2 {
						toPush = append(toPush, pushItem{
							hash: m.Hash, rec: rec, pct: pct,
							ts: updatedSeconds(row, now.Unix()), wm: row.WatermarkMs(),
						})
					}
				}
			}
			// Per docs/adr/phase-6-decision-record.md Addendum 2: the live
			// BookOrbit server does not echo unknown hashes in
			// resp.Unmatched — it omits them from both lists. Any hash in
			// the request that didn't appear in resp.Matches is therefore
			// unmatched, exactly as the reference plugin treats it
			// (bookorbit_sweep.lua:328-332: "if not matched[md5] then
			// setUnmatched(md5)"). Hashes the server does return in
			// resp.Unmatched get the same treatment; the two cases are
			// semantically indistinguishable, so they share one loop that
			// populates seen first, then marks any remaining chunk hash
			// unmatched.
			for _, h := range resp.Unmatched {
				seen[h] = true
				e.st.SetUnmatched(h, now.Unix())
				e.st.DeleteMatch(h)
			}
			for _, h := range chunk {
				if !seen[h] {
					seen[h] = true
					e.st.SetUnmatched(h, now.Unix())
					e.st.DeleteMatch(h)
					e.log.Debug("sync: hash unmatched by bookorbit", "hash", h)
				}
			}
			return true
		})

		if fatal {
			return e.finish(maxWatermark, failedWatermarks, errs)
		}
	}

	// --- Push phase ---
	if len(toPush) > 0 {
		fatal := false
		util.BatchFunc(toPush, e.cfg.Bridge.ProgressBatchSize, func(chunk []pushItem) bool {
			if e.pushChunk(ctx, chunk, &failedWatermarks, &errs) {
				fatal = true
				return false
			}
			return true
		})
		if fatal {
			return e.finish(maxWatermark, failedWatermarks, errs)
		}
	}

	// --- Status-push phase (Phase 9) ---
	// Gated opt-in per Decision F. Status is decoupled from the progress
	// watermark (Decision E): it never appends to failedWatermarks and never
	// touches LastPushedPct/LastPushedAt, so a status failure cannot stall
	// progress. A BookOrbit auth failure aborts the rest of RunOnce the same
	// way the match-check and push phases do (Decision E / Phase 6 §11).
	if e.cfg.Bridge.SyncStatus {
		fatal := e.pushStatuses(ctx, rows, now, &errs)
		if fatal {
			return e.finish(maxWatermark, failedWatermarks, errs)
		}
	}

	return e.finish(maxWatermark, failedWatermarks, errs)
}

// pushStatuses walks every pulled row and, for each already-matched book,
// pushes a changed decisive Readest reading_status to BookOrbit via the
// per-book Channel B endpoint. It returns true to signal RunOnce should abort
// immediately on a BookOrbit auth failure; every other outcome (skip, retryable
// exhaustion, ErrBookGone drop) is per-book and non-fatal.
//
// Status push never triggers its own match-check and never touches the
// progress watermark (Decision E): it rides on whatever match the match-check
// phase resolved this poll or a prior one, skipping rows that are dummy,
// deleted, unmatched, or have an unusable progress tuple — a row whose
// percentage could not be computed also has no business asserting a status.
func (e *Engine) pushStatuses(ctx context.Context, rows []readest.BookRow, now time.Time, errs *[]error) (fatal bool) {
	for _, row := range rows {
		if row.IsDummy() || row.IsDeleted() {
			continue
		}
		if _, ok := row.Percentage(); !ok {
			continue
		}
		rec, merr := e.st.Match(row.BookHash)
		if merr != nil {
			// Unmatched (or state-lookup failure): status push requires a
			// resolved BookID, so the row is skipped. Never panics on a zero
			// BookID because a present BookID is exactly what a resolved match
			// supplies.
			continue
		}
		if e.pushStatus(ctx, row, rec, now, errs) {
			return true
		}
	}
	return false
}

// pushStatus handles one row's status decision. The bool return is true when a
// BookOrbit auth failure requires aborting the whole RunOnce.
func (e *Engine) pushStatus(ctx context.Context, row readest.BookRow, rec state.MatchRecord, now time.Time, errs *[]error) (fatal bool) {
	token, push := mapReadingStatus(row.ReadingStatus)
	if !push {
		// A non-push value is one of three kinds. The decisive-no-op "unread"
		// and the known non-decisive values ("", "reading") are worth
		// recording in the seen bookkeeping (so a later transition — e.g.
		// unread→finished — diffs against the right baseline, Decision A). An
		// unrecognized value (including a future on_hold token, Decision G) is
		// warned about once per distinct value and recorded not at all: a token
		// we do not understand is not a status we can claim to have processed.
		switch classifyStatusValue(row.ReadingStatus) {
		case statusValueKnown:
			e.recordSeenStatus(row.BookHash, rec, row.ReadingStatus, now)
		case statusValueUnrecognized:
			if !e.warnedStatusValues[row.ReadingStatus] {
				e.warnedStatusValues[row.ReadingStatus] = true
				e.log.Warn("sync: unrecognized readest reading_status value",
					"hash", row.BookHash, "reading_status", row.ReadingStatus)
			}
		}
		return false
	}

	if token == rec.LastPushedStatus {
		// Already current on BookOrbit: no SetReadStatus call, but still fold
		// the seen bookkeeping forward so a later transition diffs against what
		// Readest actually reported, not a stale baseline (Decision A).
		e.recordSeenStatus(row.BookHash, rec, row.ReadingStatus, now)
		return false
	}

	err := e.pushReadStatus(ctx, rec.BookID, token)
	switch {
	case err == nil:
		rec.LastSeenStatus = row.ReadingStatus
		rec.LastSeenStatusAt = now.Unix()
		rec.LastPushedStatus = token
		rec.LastPushedStatusAt = now.Unix()
		e.st.SetMatch(row.BookHash, rec)
		return false
	case errors.Is(err, bookorbit.ErrBookGone):
		// The cached bookId is stale (deleted from the library or filtered by
		// content filters). Drop the book from the sync set exactly as the
		// absent-from-match-check path does (Phase 6 ADR Addendum 2), and do
		// NOT retreat the progress watermark (Decision E).
		e.st.DeleteMatch(row.BookHash)
		e.st.SetUnmatched(row.BookHash, now.Unix())
		return false
	case errors.Is(err, bookorbit.ErrUnauthorized):
		*errs = append(*errs, fmt.Errorf("sync: set-read-status: %w", err))
		return true
	default:
		// ErrBadRequest is a bridge logic bug; a retryable error exhausted its
		// backoff. Either way: skip this book this poll. LastPushedStatus stays
		// put (not advanced), so next poll the mapped value still differs and is
		// retried (Decision E). The progress watermark is untouched.
		*errs = append(*errs, fmt.Errorf("sync: set-read-status: %w", err))
		return false
	}
}

// recordSeenStatus advances the Readest-side seen bookkeeping for a row that
// produced no BookOrbit call (decisive-no-op or unchanged), preserving the
// pushed-side fields unchanged. It writes back via SetMatch only when the seen
// value actually changed, so a steady-state poll is a no-op.
func (e *Engine) recordSeenStatus(hash string, rec state.MatchRecord, seen string, now time.Time) {
	if rec.LastSeenStatus == seen {
		return
	}
	rec.LastSeenStatus = seen
	rec.LastSeenStatusAt = now.Unix()
	e.st.SetMatch(hash, rec)
}

// statusValueKind distinguishes the three ways a non-push Readest
// reading_status value is handled in the status step.
type statusValueKind int

const (
	// statusValueKnown is a recognized non-push value ("" / "reading" /
	// "unread"): skip the push but record the seen bookkeeping.
	statusValueKnown statusValueKind = iota
	// statusValueUnrecognized is any other value (a future schema-drift token
	// such as a hypothetical on_hold): warn once and record nothing.
	statusValueUnrecognized
)

// classifyStatusValue reports how a non-push Readest status value is handled.
// The decisive-push values never reach here (mapReadingStatus already returned
// push=true for them); this splits the remaining space into "record the seen
// bookkeeping" versus "warn once and record nothing".
func classifyStatusValue(s string) statusValueKind {
	switch s {
	case "", "reading", "unread":
		return statusValueKnown
	default:
		return statusValueUnrecognized
	}
}

// finish computes the final watermark, persists state exactly once, and
// returns the joined batch errors (nil if there were none).
func (e *Engine) finish(maxWatermark int64, failedWatermarks []int64, errs []error) error {
	e.st.SetWatermark(computeWatermark(e.st.Watermark(), maxWatermark, failedWatermarks))
	if err := e.st.Save(); err != nil {
		errs = append(errs, fmt.Errorf("sync: save state: %w", err))
	}
	return errors.Join(errs...)
}

// computeWatermark implements the retreat rule (Decision A): if any row's
// batch ultimately failed, retreat to min(failed) - 1 (a millisecond analogue
// of bookorbit_state.lua:applyStatsAck's one-second back-off) so those rows
// are re-pulled next run; otherwise advance to the max across all non-dummy
// rows. The result never regresses below floor (the pre-existing watermark).
func computeWatermark(floor, maxWatermark int64, failedWatermarks []int64) int64 {
	if len(failedWatermarks) == 0 {
		if maxWatermark > floor {
			return maxWatermark
		}
		return floor
	}
	min := failedWatermarks[0]
	for _, w := range failedWatermarks[1:] {
		if w < min {
			min = w
		}
	}
	if retreat := min - 1; retreat > floor {
		return retreat
	}
	return floor
}

// pushChunk pushes one batch of items, using BulkProgress unless the bulk
// endpoint has already been discovered unsupported this process lifetime
// (Decision F), in which case it falls back to UpdateProgress per item. It
// returns true if the batch hit a fatal (auth) error, signaling RunOnce to
// abort immediately.
func (e *Engine) pushChunk(ctx context.Context, chunk []pushItem, failedWatermarks *[]int64, errs *[]error) (fatal bool) {
	if e.bulkUnsupported {
		for _, p := range chunk {
			e.pushSingle(ctx, p, errs)
		}
		return false
	}

	items := make([]bookorbit.ProgressItem, len(chunk))
	for i, p := range chunk {
		items[i] = bookorbit.ProgressItem{Hash: p.hash, Percentage: p.pct, Progress: "", Timestamp: p.ts}
	}

	resp, err := e.bulkProgress(ctx, bookorbit.BulkProgressRequest{Items: items})
	if err != nil {
		if errors.Is(err, bookorbit.ErrUnauthorized) {
			*errs = append(*errs, fmt.Errorf("sync: bulk-progress: %w", err))
			return true
		}
		if errors.Is(err, bookorbit.ErrUnsupportedEndpoint) {
			e.bulkUnsupported = true
			e.log.Warn("sync: bulk-progress endpoint unsupported; falling back to per-item update-progress for the remainder of this process")
			for _, p := range chunk {
				e.pushSingle(ctx, p, errs)
			}
			return false
		}
		*errs = append(*errs, fmt.Errorf("sync: bulk-progress: %w", err))
		for _, p := range chunk {
			*failedWatermarks = append(*failedWatermarks, p.wm)
		}
		return false
	}

	unmatched := make(map[string]bool, len(resp.Unmatched))
	for _, h := range resp.Unmatched {
		unmatched[h] = true
	}
	for _, p := range chunk {
		if unmatched[p.hash] {
			e.st.SetUnmatched(p.hash, e.now().Unix())
			e.st.DeleteMatch(p.hash)
			continue
		}
		// Preserve the Phase 9 status bookkeeping (LastSeenStatus*,
		// LastPushedStatus*) already on p.rec: a progress push updates only the
		// progress fields and must not reset the status baseline.
		rec := p.rec
		rec.LastPushedAt = e.now().Unix()
		rec.LastPushedPct = p.pct
		e.st.SetMatch(p.hash, rec)
	}
	return false
}

// pushSingle pushes one item via the kosync-compatible fallback endpoint.
// Unlike BulkProgress, UpdateProgress has no response body and so cannot
// report a per-hash "unmatched" signal (docs/phase-5-design-temp.md §16,
// items 2/3/6) — this degradation is accepted, per Decision F.
func (e *Engine) pushSingle(ctx context.Context, p pushItem, errs *[]error) {
	err := e.updateProgress(ctx, bookorbit.UpdateProgressRequest{
		Document:   p.hash,
		Percentage: p.pct,
		Progress:   "",
		Timestamp:  p.ts,
	})
	if err != nil {
		*errs = append(*errs, fmt.Errorf("sync: update-progress: %w", err))
		return
	}
	// Preserve the Phase 9 status bookkeeping on p.rec, exactly as pushChunk
	// does: a progress push updates only the progress fields.
	rec := p.rec
	rec.LastPushedAt = e.now().Unix()
	rec.LastPushedPct = p.pct
	e.st.SetMatch(p.hash, rec)
}

// updatedSeconds returns row.UpdatedMs() converted to Unix epoch seconds, or
// fallback if the timestamp cannot be parsed.
func updatedSeconds(row readest.BookRow, fallback int64) int64 {
	ms, err := row.UpdatedMs()
	if err != nil {
		return fallback
	}
	return util.MsToSeconds(ms)
}

// Run drives RunOnce on the configured poll interval until ctx is cancelled.
// A RunOnce error is logged, never propagated — a daemon keeps trying on its
// configured cadence through transient outages; a supervisor (systemd,
// Docker) decides whether persistent failure warrants a restart. Run returns
// only when ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	for {
		if err := e.RunOnce(ctx); err != nil {
			e.log.Warn("sync: run once failed", "error", err)
		}
		if err := e.sleep(ctx, e.cfg.Bridge.PollInterval); err != nil {
			return err
		}
	}
}

// --- Retry / classification (§7) ---

// outcome is the three-way verdict withRetry needs from a call's error.
type outcome int

const (
	outcomeSuccess outcome = iota
	outcomeRetry
	outcomeFatal
	outcomeSkip
)

// classifyReadestErr maps a readest.Client error to a retry verdict. Only the
// engine is allowed to know both packages' sentinel sets (the boundary
// Phase 4's design insisted on), so these classifiers live here.
func classifyReadestErr(err error) outcome {
	switch {
	case err == nil:
		return outcomeSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return outcomeFatal
	case errors.Is(err, readest.ErrUnauthorized):
		return outcomeFatal
	case errors.Is(err, readest.ErrBadRequest), errors.Is(err, readest.ErrSyncMalformed):
		return outcomeSkip
	case errors.Is(err, readest.ErrSyncRateLimited), errors.Is(err, readest.ErrSyncServer), errors.Is(err, readest.ErrSyncNetwork):
		return outcomeRetry
	default:
		return outcomeRetry
	}
}

// classifyBookOrbitErr maps a bookorbit.Client error to a retry verdict.
func classifyBookOrbitErr(err error) outcome {
	switch {
	case err == nil:
		return outcomeSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return outcomeFatal
	case errors.Is(err, bookorbit.ErrUnauthorized):
		return outcomeFatal
	case errors.Is(err, bookorbit.ErrBadRequest),
		errors.Is(err, bookorbit.ErrMalformedResponse),
		errors.Is(err, bookorbit.ErrUnsupportedEndpoint),
		errors.Is(err, bookorbit.ErrBodyTooLarge):
		return outcomeSkip
	case errors.Is(err, bookorbit.ErrRateLimited), errors.Is(err, bookorbit.ErrServer), errors.Is(err, bookorbit.ErrNetwork):
		return outcomeRetry
	default:
		return outcomeRetry
	}
}

// withRetry calls fn, retrying with exponential backoff while classify
// reports outcomeRetry, up to cfg.RetryMaxAttempts additional attempts.
// outcomeFatal and outcomeSkip return immediately without retrying. The sleep
// between attempts respects ctx cancellation.
func withRetry(ctx context.Context, cfg config.BridgeConfig, sleep func(context.Context, time.Duration) error, call func() error, classify func(error) outcome) error {
	delay := cfg.RetryInitialBackoff
	var err error
	for attempt := 0; ; attempt++ {
		err = call()
		if classify(err) != outcomeRetry {
			return err
		}
		if attempt >= cfg.RetryMaxAttempts {
			return err
		}
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
		delay *= 2
		if delay > cfg.RetryMaxBackoff {
			delay = cfg.RetryMaxBackoff
		}
	}
}

func (e *Engine) pullBooks(ctx context.Context, since int64) ([]readest.BookRow, error) {
	var rows []readest.BookRow
	err := withRetry(ctx, e.cfg.Bridge, e.sleep, func() error {
		var callErr error
		rows, callErr = e.rd.PullBooks(ctx, since)
		return callErr
	}, classifyReadestErr)
	return rows, err
}

func (e *Engine) matchCheck(ctx context.Context, req bookorbit.MatchCheckRequest) (bookorbit.MatchCheckResponse, error) {
	var resp bookorbit.MatchCheckResponse
	err := withRetry(ctx, e.cfg.Bridge, e.sleep, func() error {
		var callErr error
		resp, callErr = e.bo.MatchCheck(ctx, req)
		return callErr
	}, classifyBookOrbitErr)
	return resp, err
}

func (e *Engine) bulkProgress(ctx context.Context, req bookorbit.BulkProgressRequest) (bookorbit.BulkProgressResponse, error) {
	var resp bookorbit.BulkProgressResponse
	err := withRetry(ctx, e.cfg.Bridge, e.sleep, func() error {
		var callErr error
		resp, callErr = e.bo.BulkProgress(ctx, req)
		return callErr
	}, classifyBookOrbitErr)
	return resp, err
}

func (e *Engine) updateProgress(ctx context.Context, req bookorbit.UpdateProgressRequest) error {
	return withRetry(ctx, e.cfg.Bridge, e.sleep, func() error {
		return e.bo.UpdateProgress(ctx, req)
	}, classifyBookOrbitErr)
}

// classifyBookOrbitStatusErr maps a SetReadStatus error to a retry verdict for
// the status-push step. It is a distinct classifier from classifyBookOrbitErr
// (Phase 9, Decision H / §3.4): bookorbit.ErrBookGone has no withRetry verdict
// — it is handled inline by pushStatus, which branches on errors.Is(err,
// ErrBookGone) BEFORE withRetry ever runs, so this classifier is only ever
// asked to judge the non-gone cases. Should ErrBookGone ever reach it
// (defense-in-depth), it returns outcomeSkip, which is correct by
// definition: a stale bookId can never succeed on retry.
func classifyBookOrbitStatusErr(err error) outcome {
	switch {
	case err == nil:
		return outcomeSuccess
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return outcomeFatal
	case errors.Is(err, bookorbit.ErrUnauthorized):
		return outcomeFatal
	case errors.Is(err, bookorbit.ErrBookGone),
		errors.Is(err, bookorbit.ErrBadRequest):
		return outcomeSkip
	case errors.Is(err, bookorbit.ErrRateLimited), errors.Is(err, bookorbit.ErrServer), errors.Is(err, bookorbit.ErrNetwork):
		return outcomeRetry
	default:
		return outcomeRetry
	}
}

// pushReadStatus pushes one book's read-status token via the Channel B
// endpoint, with the status-specific retry classification (Decision E /
// Decision H). Like the other gateways, transient errors are retried with the
// configured backoff; the caller (pushStatus) decides what a returned error
// means for the row's bookkeeping.
func (e *Engine) pushReadStatus(ctx context.Context, bookID int64, token string) error {
	return withRetry(ctx, e.cfg.Bridge, e.sleep, func() error {
		return e.bo.SetReadStatus(ctx, bookID, token)
	}, classifyBookOrbitStatusErr)
}
