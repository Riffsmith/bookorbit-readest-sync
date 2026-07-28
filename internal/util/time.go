package util

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// isoTimeFormats enumerates the ISO-8601 layouts the Readest sync API emits.
// The server returns timestamps with or without fractional seconds and may use
// either a "Z" UTC designator or a numeric offset (e.g. "+00:00").
var isoTimeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00", // tolerate a space separator
	"2006-01-02 15:04:05",       // tolerate no zone, assume UTC
}

// ISOToMs converts an ISO-8601 timestamp string to Unix epoch milliseconds.
//
// This is a port of the reference plugin's `iso_to_ms` helper, which every
// Readest book row passes through before the watermark is computed. An empty
// string returns 0 with no error, matching the plugin's treatment of absent
// timestamps. A non-empty string that cannot be parsed returns an error so the
// caller can decide whether to skip the row or fail.
func ISOToMs(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var lastErr error
	for _, layout := range isoTimeFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), nil
		} else {
			lastErr = err
		}
	}
	return 0, fmt.Errorf("util: cannot parse ISO-8601 time %q: %w", s, lastErr)
}

// MustISOToMs is ISOToMs for contexts (like tests) where a parse error should
// be fatal. It panics on error.
func MustISOToMs(s string) int64 {
	ms, err := ISOToMs(s)
	if err != nil {
		panic(err)
	}
	return ms
}

// MsToSeconds converts Unix epoch milliseconds to Unix epoch seconds, rounding
// toward zero. BookOrbit progress timestamps are in seconds while Readest rows
// carry milliseconds, so this conversion is required before every push.
func MsToSeconds(ms int64) int64 {
	return ms / 1000
}

// MaxMs returns the largest of a set of millisecond timestamps. Used to
// advance the pull watermark to max(synced_at, updated_at, deleted_at) exactly
// as the reference plugin does.
func MaxMs(values ...int64) int64 {
	max := int64(0)
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	return max
}

var errInvalidPercent = errors.New("util: cannot compute percentage")

// Percent computes a 0..1 progress ratio from a Readest progress tuple
// [cur, total], guarded against the degenerate cases the reference plugin
// skips: a missing tuple, a non-positive total, or a negative cursor.
//
// The result is clamped to [0, 1] and rounded to 5 decimal places, matching
// the plugin's rounding so we do not generate floating-point churn between
// otherwise-identical readings. An error is returned when the tuple cannot
// yield a meaningful percentage; callers should treat that as "skip this row".
func Percent(cur, total float64) (float64, error) {
	if total <= 0 {
		return 0, fmt.Errorf("%w: total %v is not positive", errInvalidPercent, total)
	}
	if cur < 0 {
		return 0, fmt.Errorf("%w: cur %v is negative", errInvalidPercent, cur)
	}
	p := cur / total
	if p > 1 {
		p = 1
	}
	return round(p, 5), nil
}

// round rounds v to the given number of decimal places.
func round(v float64, places int) float64 {
	p := 1.0
	for i := 0; i < places; i++ {
		p *= 10
	}
	return float64(int(v*p+0.5)) / p
}
