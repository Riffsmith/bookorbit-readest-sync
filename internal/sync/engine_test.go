package sync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/user/bookorbit-readest-sync/internal/config"
	"github.com/user/bookorbit-readest-sync/internal/sync/state"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEngineStubsReturnNotImplemented(t *testing.T) {
	cfg := config.Default()
	st := state.NewMemStore()
	e := NewEngine(cfg, nil, nil, nil, st, testLogger())

	if err := e.RunOnce(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("RunOnce err = %v, want ErrNotImplemented", err)
	}
	if err := e.Run(context.Background()); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Run err = %v, want ErrNotImplemented", err)
	}
}

func TestEnginePollInterval(t *testing.T) {
	cfg := config.Default()
	cfg.Bridge.PollInterval = 42 * time.Minute
	e := NewEngine(cfg, nil, nil, nil, state.NewMemStore(), testLogger())
	if got := e.PollInterval(); got != 42*time.Minute {
		t.Errorf("PollInterval = %v, want 42m", got)
	}
}
