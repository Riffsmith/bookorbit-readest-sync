package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestLevelParsing(t *testing.T) {
	cases := []struct {
		in       string
		wantInfo bool // whether an info message is emitted at this level
	}{
		{"debug", true},
		{"info", true},
		{"INFO", true},
		{"warn", false},
		{"error", false},
		{"bogus", true}, // unknown falls back to info
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
	// Should not panic with a nil writer.
	_ = New(nil, "info", "json")
}
