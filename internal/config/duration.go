package config

import (
	"strconv"
	"strings"
	"time"
)

// time_ParseDuration parses a duration from a string, accepting Go duration
// syntax ("15m", "1h30m") as well as bare integer seconds ("900") and a plain
// number of minutes for convenience in config files. It exists as a named
// function so env.go and the YAML applicator share one implementation.
func time_ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errEmptyDuration
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// Fall back to a bare number, interpreted as seconds.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, &durationParseError{value: s}
}

type emptyDurationError struct{}

func (e *emptyDurationError) Error() string { return "config: empty duration" }

var errEmptyDuration error = &emptyDurationError{}

type durationParseError struct{ value string }

func (e *durationParseError) Error() string {
	return "config: cannot parse duration " + strconv.Quote(e.value)
}
