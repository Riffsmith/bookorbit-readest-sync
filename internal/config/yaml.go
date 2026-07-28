package config

import (
	"fmt"
	"strings"
)

// parseSimpleYAML parses the small, strict subset of YAML used by
// configs/bridge.yaml into a flat map of dotted section keys to string values.
//
// The bridge config is deliberately flat — one level of sections (`readest`,
// `bookorbit`, `bridge`) with scalar leaf keys — so we intentionally avoid a
// third-party YAML dependency and parse the subset ourselves. Supported:
//
//	# comments and blank lines
//	section:                # a top-level key with no inline value
//	  key: value            # an indented scalar under the current section
//	key: value              # a top-level scalar (stored without a section)
//	key: "quoted" | 'quoted'
//
// Not supported (and rejected with an error): nested maps beyond one level,
// sequences/lists, anchors, multi-line scalars, or tabs for indentation.
// Keeping the grammar this small makes misconfiguration fail loudly rather
// than silently mis-parse.
func parseSimpleYAML(data []byte) (map[string]string, error) {
	out := make(map[string]string)
	section := ""

	lines := strings.Split(string(data), "\n")
	for i, rawLine := range lines {
		lineNo := i + 1

		// Reject tabs in indentation up front — they are a common YAML footgun.
		if strings.HasPrefix(rawLine, "\t") || strings.Contains(rawLine[:leadingSpaces(rawLine)], "\t") {
			return nil, fmt.Errorf("config: line %d: tab indentation is not allowed", lineNo)
		}

		// Strip a trailing comment that is not inside quotes.
		line := stripComment(rawLine)
		if strings.TrimSpace(line) == "" {
			continue
		}

		indent := leadingSpaces(line)
		content := strings.TrimSpace(line)

		key, value, hasValue := splitKeyValue(content)
		if key == "" {
			return nil, fmt.Errorf("config: line %d: expected \"key: value\"", lineNo)
		}

		if indent == 0 {
			// Top-level. A key with no inline value opens a section.
			if !hasValue {
				section = key
				continue
			}
			section = ""
			out[key] = value
			continue
		}

		// Indented scalar: must belong to a section.
		if section == "" {
			return nil, fmt.Errorf("config: line %d: indented key %q has no section", lineNo, key)
		}
		if !hasValue {
			return nil, fmt.Errorf("config: line %d: nested maps are not supported (under %q)", lineNo, key)
		}
		out[section+"."+key] = value
	}

	return out, nil
}

// leadingSpaces returns the count of leading space characters.
func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

// splitKeyValue splits "key: value" into its parts. hasValue reports whether a
// value followed the colon (even if empty after unquoting).
func splitKeyValue(s string) (key, value string, hasValue bool) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:idx])
	rest := strings.TrimSpace(s[idx+1:])
	if rest == "" {
		return key, "", false
	}
	return key, unquote(rest), true
}

// unquote removes matching surrounding single or double quotes from a scalar.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// stripComment removes a trailing " # comment" while respecting quotes.
func stripComment(s string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			// A '#' starts a comment only if preceded by whitespace or start.
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return s
}
