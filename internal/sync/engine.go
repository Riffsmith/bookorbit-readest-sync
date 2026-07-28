// Package sync holds the orchestration layer and the persistent state store.
// The engine is the only place that knows about both Readest and BookOrbit;
// the state store is the only persistence the bridge uses.
package sync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/user/bookorbit-readest-sync/internal/bookorbit"
	"github.com/user/bookorbit-readest-sync/internal/config"
	"github.com/user/bookorbit-readest-sync/internal/readest"
	"github.com/user/bookorbit-readest-sync/internal/sync/state"
)

// ErrNotImplemented marks the engine's stub orchestration methods, which are
// implemented in Phase 6 of the roadmap.
var ErrNotImplemented = errors.New("sync: not implemented in foundation phase")

// Engine coordinates a one-way sync from Readest to BookOrbit. It depends on
// the two clients, the persistent state store, the config, and a logger — all
// injected so the engine is fully testable. The concrete pull → diff → match →
// push pipeline lands in Phase 6; this type wires the dependencies now.
type Engine struct {
	cfg    config.Config
	rdAuth readest.Authenticator
	rd     readest.SyncClient
	bo     bookorbit.API
	st     state.Store
	log    *slog.Logger
}

// NewEngine constructs an Engine from its dependencies. All parameters are
// required; the constructor does not start any work.
func NewEngine(
	cfg config.Config,
	rdAuth readest.Authenticator,
	rd readest.SyncClient,
	bo bookorbit.API,
	st state.Store,
	log *slog.Logger,
) *Engine {
	return &Engine{
		cfg:    cfg,
		rdAuth: rdAuth,
		rd:     rd,
		bo:     bo,
		st:     st,
		log:    log,
	}
}

// RunOnce performs a single sync pass. It is a stub in the foundation phase
// and returns ErrNotImplemented; the full pipeline (refresh token, pull books,
// diff against watermark, match-check, batch, push, advance state) is
// implemented in Phase 6.
func (e *Engine) RunOnce(ctx context.Context) error {
	return ErrNotImplemented
}

// Run drives RunOnce on the configured poll interval until ctx is cancelled.
// It is a stub in the foundation phase; the interruptible-sleep loop and
// graceful shutdown are implemented in Phase 6.
func (e *Engine) Run(ctx context.Context) error {
	return ErrNotImplemented
}

// PollInterval exposes the configured cadence for callers (e.g., the CLI's
// startup log) without reaching into config internals.
func (e *Engine) PollInterval() time.Duration {
	return e.cfg.Bridge.PollInterval
}
