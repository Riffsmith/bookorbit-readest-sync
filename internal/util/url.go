package util

import (
	"strings"
)

// NormalizeBookOrbitURL canonicalizes a user-supplied BookOrbit server base
// URL so that the API prefix "/api/v1" appears exactly once.
//
// It mirrors the reference plugin's normalizeServerUrl:
//   - trims whitespace and strips trailing slashes
//   - collapses a trailing "/api/v1/koreader" to "/api/v1"
//   - appends "/api/v1" when the path does not already end with it
//
// The scheme is preserved as given; callers are expected to supply an http or
// https URL. An empty input returns an empty string so validation can flag it.
func NormalizeBookOrbitURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	// Strip trailing slashes.
	u = strings.TrimRight(u, "/")

	// Collapse a redundant koreader suffix that some users copy in.
	for _, suffix := range []string{"/api/v1/koreader"} {
		if strings.HasSuffix(u, suffix) {
			u = strings.TrimSuffix(u, suffix)
		}
	}

	// Ensure the API prefix is present exactly once.
	if !strings.HasSuffix(u, "/api/v1") {
		// If the path already contains "/api/v1" mid-string (unusual), leave it.
		if !strings.Contains(u, "/api/v1") {
			u += "/api/v1"
		}
	}
	return u
}
