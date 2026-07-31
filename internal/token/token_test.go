package token

import "testing"

// TestShouldRefresh pins the 50%-TTL proactive refresh rule package-locally.
// This mirrors internal/readest/models_test.go: TestTokenFreshnessRules, but
// exercises the token.Token methods directly so a regression here is caught
// by this package's own tests rather than only as a side effect of
// internal/readest importing token.Token.
func TestShouldRefresh(t *testing.T) {
	cases := []struct {
		name string
		tok  Token
		now  int64
		want bool
	}{
		{name: "well within TTL", tok: Token{ExpiresAt: 1000, ExpiresIn: 600}, now: 100, want: false},
		{name: "past the halfway threshold", tok: Token{ExpiresAt: 1000, ExpiresIn: 600}, now: 800, want: true},
		// Halfway point is ExpiresAt - ExpiresIn/2 = 1000 - 300 = 700. The rule
		// is ExpiresAt < now+ExpiresIn/2, so at now=700 that's 1000 < 1000,
		// which is false: still fresh at the exact boundary.
		{name: "exactly at the halfway boundary", tok: Token{ExpiresAt: 1000, ExpiresIn: 600}, now: 700, want: false},
		{name: "zero TTL forces refresh", tok: Token{ExpiresAt: 1000, ExpiresIn: 0}, now: 0, want: true},
		{name: "negative TTL forces refresh", tok: Token{ExpiresAt: 1000, ExpiresIn: -5}, now: 0, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tok.ShouldRefresh(c.now); got != c.want {
				t.Errorf("ShouldRefresh(%d) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

// TestExpiresWithin pins the pre-request 60-second guard's boundary behavior.
func TestExpiresWithin(t *testing.T) {
	tok := Token{ExpiresAt: 1000}
	cases := []struct {
		name    string
		now     int64
		seconds int64
		want    bool
	}{
		{name: "well before expiry", now: 100, seconds: 60, want: false},
		// Rule is ExpiresAt < now+seconds. At now=940, 1000 < 1000 is false.
		{name: "exactly at the guard boundary", now: 940, seconds: 60, want: false},
		{name: "within the guard window", now: 950, seconds: 60, want: true},
		{name: "already past expiry", now: 1001, seconds: 60, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tok.ExpiresWithin(c.now, c.seconds); got != c.want {
				t.Errorf("ExpiresWithin(%d,%d) = %v, want %v", c.now, c.seconds, got, c.want)
			}
		})
	}
}

// TestIsZero pins the first-run detection: a token is "zero" only when both
// credential fields are empty, matching AccessToken's current.IsZero() branch.
func TestIsZero(t *testing.T) {
	cases := []struct {
		name string
		tok  Token
		want bool
	}{
		{name: "zero value", tok: Token{}, want: true},
		{name: "access token only", tok: Token{AccessToken: "a"}, want: false},
		{name: "refresh token only", tok: Token{RefreshToken: "r"}, want: false},
		{name: "both set", tok: Token{AccessToken: "a", RefreshToken: "r"}, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tok.IsZero(); got != c.want {
				t.Errorf("IsZero() = %v, want %v", got, c.want)
			}
		})
	}
}
