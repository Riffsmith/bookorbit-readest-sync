package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestLevelParsing(t *testing.T) {
	cases := []struct {
		in       string
		wantInfo bool
	}{
		{"debug", true},
		{"info", true},
		{"INFO", true},
		{"warn", false},
		{"error", false},
		{"bogus", true},
		{"", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			var buf bytes.Buffer
			log := New(&buf, c.in, "text")
			log.Info("hello")
			emitted := strings.Contains(buf.String(), "hello")
			if emitted != c.wantInfo {
				t.Errorf("level %q: info emitted = %v, want %v", c.in, emitted, c.wantInfo)
			}
		})
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "info", "json")
	log.Info("msg", "key", "value")
	out := buf.String()
	if !strings.Contains(out, `"msg":"msg"`) || !strings.Contains(out, `"key":"value"`) {
		t.Errorf("json output missing fields: %s", out)
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "info", "text")
	log.Info("msg", "key", "value")
	if !strings.Contains(buf.String(), "key=value") {
		t.Errorf("text output missing key=value: %s", buf.String())
	}
}

func TestNilWriterDefaultsToStderr(t *testing.T) {
	_ = New(nil, "info", "json")
}

func TestLevelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"debug", "DEBUG"},
		{"DEBUG", "DEBUG"},
		{"info", "INFO"},
		{"INFO", "INFO"},
		{"", "INFO"},
		{"warn", "WARN"},
		{"warning", "WARN"},
		{"error", "ERROR"},
		{"bogus", "INFO"},
	}
	for _, c := range cases {
		if got := LevelName(c.in); got != c.want {
			t.Errorf("LevelName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
