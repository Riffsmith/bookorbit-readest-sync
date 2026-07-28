// Package logger builds the process-wide structured logger.
//
// The bridge is a headless daemon: it has no UI, so every operational signal
// the KOReader plugins surfaced through InfoMessage/Notification becomes a
// structured log line instead. We use the standard library's log/slog so the
// rest of the code depends only on a stable, dependency-free API.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format selects the log encoding.
type Format string

const (
	// FormatText emits human-readable key=value lines (development default).
	FormatText Format = "text"
	// FormatJSON emits one JSON object per line (production/daemon default).
	FormatJSON Format = "json"
)

// New returns a slog.Logger writing to w at the given level and format.
//
// levelName is case-insensitive and accepts debug/info/warn/error; an unknown
// value falls back to info. format defaults to JSON for any unrecognized value
// so daemon output stays machine-parseable unless text is explicitly chosen.
func New(w io.Writer, levelName, format string) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	level := parseLevel(levelName)
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch Format(strings.ToLower(strings.TrimSpace(format))) {
	case FormatText:
		handler = slog.NewTextHandler(w, opts)
	case FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}

// parseLevel maps a level name to slog.Level, defaulting to info.
func parseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LevelName returns the canonical name of a configured level, for logging the
// effective configuration at startup.
func LevelName(name string) string {
	return fmt.Sprintf("%s", parseLevel(name))
}
