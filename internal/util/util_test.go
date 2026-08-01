package util

import "testing"

func TestISOToMs(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{name: "empty", in: "", want: 0},
		{name: "rfc3339 zulu", in: "2026-01-02T03:04:05Z", want: 1767323045000},
		{name: "rfc3339 offset", in: "2026-01-02T03:04:05+00:00", want: 1767323045000},
		{name: "fractional seconds", in: "2026-01-02T03:04:05.5Z", want: 1767323045500},
		{name: "space separator no zone", in: "2026-01-02 03:04:05", want: 1767323045000},
		{name: "invalid", in: "not-a-time", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ISOToMs(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ISOToMs(%q) expected error, got %d", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ISOToMs(%q) unexpected error: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ISOToMs(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

func TestMsToSeconds(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{1767323045500, 1767323045},
		{0, 0},
		{999, 0},
		{1000, 1},
	}
	for _, c := range cases {
		if got := MsToSeconds(c.in); got != c.want {
			t.Errorf("MsToSeconds(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestMaxMs(t *testing.T) {
	if got := MaxMs(); got != 0 {
		t.Errorf("MaxMs() = %d, want 0", got)
	}
	if got := MaxMs(5, 2, 9, 3); got != 9 {
		t.Errorf("MaxMs(5,2,9,3) = %d, want 9", got)
	}
	if got := MaxMs(-5, -2); got != 0 {
		t.Errorf("MaxMs(-5,-2) = %d, want 0", got)
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		name     string
		cur, tot float64
		want     float64
		wantErr  bool
	}{
		{name: "half", cur: 125, tot: 250, want: 0.5},
		{name: "zero cur", cur: 0, tot: 250, want: 0},
		{name: "full", cur: 250, tot: 250, want: 1},
		{name: "over 1 clamps", cur: 300, tot: 250, want: 1},
		{name: "rounding", cur: 1, tot: 3, want: 0.33333},
		{name: "zero total", cur: 1, tot: 0, wantErr: true},
		{name: "negative total", cur: 1, tot: -5, wantErr: true},
		{name: "negative cur", cur: -1, tot: 250, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Percent(c.cur, c.tot)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Percent(%v,%v) expected error, got %v", c.cur, c.tot, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Percent(%v,%v) unexpected error: %v", c.cur, c.tot, err)
			}
			if got != c.want {
				t.Fatalf("Percent(%v,%v) = %v, want %v", c.cur, c.tot, got, c.want)
			}
		})
	}
}

func TestNormalizeBookOrbitURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://nas:8080", "http://nas:8080/api/v1"},
		{"http://nas:8080/", "http://nas:8080/api/v1"},
		{"http://nas:8080/api/v1", "http://nas:8080/api/v1"},
		{"http://nas:8080/api/v1/", "http://nas:8080/api/v1"},
		{"http://nas:8080/api/v1/koreader", "http://nas:8080/api/v1"},
		{"https://books.example.com///", "https://books.example.com/api/v1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeBookOrbitURL(c.in); got != c.want {
			t.Errorf("NormalizeBookOrbitURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMD5HexAndIsMD5Hex(t *testing.T) {
	if got := MD5Hex("password"); got != "5f4dcc3b5aa765d61d8327deb882cf99" {
		t.Errorf("MD5Hex(password) = %q, want known digest", got)
	}
	if !IsMD5Hex("5f4dcc3b5aa765d61d8327deb882cf99") {
		t.Error("IsMD5Hex should accept a valid lowercase digest")
	}
	if !IsMD5Hex("5F4DCC3B5AA765D61D8327DEB882CF99") {
		t.Error("IsMD5Hex should accept uppercase hex")
	}
	if IsMD5Hex("xyz") || IsMD5Hex("5f4dcc3b5aa765d61d8327deb882cf9g") {
		t.Error("IsMD5Hex should reject invalid input")
	}
}

func TestLowerNormal(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ABC123", "abc123"},
		{"  MixedCase  ", "mixedcase"},
		{"already-normal", "already-normal"},
		{"", ""},
		{"\tTabbed\n", "tabbed"},
		{"5F4DCC3B5AA765D61D8327DEB882CF99", "5f4dcc3b5aa765d61d8327deb882cf99"},
	}
	for _, c := range cases {
		if got := LowerNormal(c.in); got != c.want {
			t.Errorf("LowerNormal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBatch(t *testing.T) {
	in := []string{"a", "b", "c", "d", "e"}
	got := Batch(in, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[2]) != 1 {
		t.Fatalf("Batch size 2 = %v, want 3 chunks (2,2,1)", got)
	}
	if Batch(nil, 2) != nil {
		t.Error("Batch(nil) should be nil")
	}
	if got := Batch(in, 0); len(got) != 1 {
		t.Errorf("Batch size 0 should be a single chunk, got %d", len(got))
	}
}

func TestBatchFunc(t *testing.T) {
	in := []int{1, 2, 3, 4, 5}
	var chunks int
	var seen []int
	BatchFunc(in, 2, func(c []int) bool {
		chunks++
		seen = append(seen, c...)
		return true
	})
	if chunks != 3 || len(seen) != 5 {
		t.Fatalf("BatchFunc visited %d chunks / %d items, want 3/5", chunks, len(seen))
	}

	chunks = 0
	BatchFunc(in, 2, func(c []int) bool {
		chunks++
		return false
	})
	if chunks != 1 {
		t.Errorf("BatchFunc early stop visited %d chunks, want 1", chunks)
	}
}

func TestSchemeOf(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "http", in: "http://x", want: "http"},
		{name: "https upper preserved lowercase", in: "HTTPS://x", want: "https"},
		{name: "gopher", in: "gopher://x", want: "gopher"},
		{name: "empty input", in: "", wantErr: true},
		{name: "no scheme", in: "//noscheme", wantErr: true},
		{name: "bare words parse with no scheme", in: "not a url at all", wantErr: true},
		{name: "host only path", in: "example.com", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SchemeOf(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("SchemeOf(%q) expected error, got %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("SchemeOf(%q) unexpected error: %v", c.in, err)
			}
			if c.wantErr {
				return
			}
			if got != c.want {
				t.Errorf("SchemeOf(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestIsLoopbackOrLinkLocal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{name: "localhost", in: "localhost", want: true},
		{name: "localhost trailing dot", in: "localhost.", want: true},
		{name: "ipv4 loopback", in: "127.0.0.1", want: true},
		{name: "ipv4 loopback upper bound", in: "127.255.255.255", want: true},
		{name: "ipv4 link-local low", in: "169.254.0.0", want: true},
		{name: "ipv4 link-local mid", in: "169.254.10.20", want: true},
		{name: "ipv6 loopback bare", in: "::1", want: true},
		{name: "ipv6 loopback bracketed", in: "[::1]", want: true},
		{name: "ipv6 loopback with port", in: "[::1]:8080", want: true},
		{name: "ipv6 link-local", in: "fe80::1", want: true},
		{name: "ipv6 link-local bracketed", in: "[fe80::1]", want: true},
		{name: "ipv4 loopback with port", in: "127.0.0.1:3000", want: true},

		{name: "ipv4 lan", in: "192.168.1.10", want: false},
		{name: "ipv4 lan with port", in: "192.168.1.10:3000", want: false},
		{name: "ipv4 private 10", in: "10.0.0.1", want: false},
		{name: "ipv4 public", in: "8.8.8.8", want: false},
		{name: "dns name", in: "example.com", want: false},
		{name: "private-looking dns", in: "nas.local", want: false},
		{name: "empty", in: "", want: false},
		{name: "junk", in: "not an ip or host", want: false},
		{name: "bracketed ipv4", in: "[127.0.0.1]", want: true},
		{name: "bracketed ipv4 with port", in: "[127.0.0.1]:3000", want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsLoopbackOrLinkLocal(c.in); got != c.want {
				t.Errorf("IsLoopbackOrLinkLocal(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestHostOf(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://books.example.com/api/v1", "books.example.com"},
		{"http://192.168.1.10:3000", "192.168.1.10:3000"},
		{"https://[::1]:8443/api/v1", "[::1]:8443"},
		{"http://localhost:8080/api/v1", "localhost:8080"},
	}
	for _, c := range cases {
		if got := HostOf(c.in); got != c.want {
			t.Errorf("HostOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// On an unparseable input HostOf returns the input verbatim so a caller
	// logging a cleartext warning still names something.
	if got := HostOf("not a url at all"); got != "not a url at all" {
		t.Errorf("HostOf(unparseable) = %q, want the input string back", got)
	}
}

func TestNormalizeBookOrbitURLRejectsInvalid(t *testing.T) {
	cases := []string{
		"http://user:pass@nas/api/v1", // userinfo baked into URL is a footgun the BookOrbit protocol does not use
		"http://nas/api/v1#frag",      // a fragment on an API base URL is meaningless (paste error)
		"not a url",                   // unparseable / no scheme
		"",                            // empty input is the existing "missing URL" sentinel
		"//noscheme",                  // scheme is required
	}
	for _, c := range cases {
		if got := NormalizeBookOrbitURL(c); got != "" {
			t.Errorf("NormalizeBookOrbitURL(%q) = %q, want empty (rejected)", c, got)
		}
	}
	// Note: a bad scheme (e.g. gopher://) parses cleanly and is normalized
	// normally here — the scheme allowlist is enforced in config.Validate via
	// util.SchemeOf, which is the cleaner place to surface "must use http or
	// https" messages. Pinning that policy here would duplicate the check and
	// risk the two code paths disagreeing on what a valid URL is; the
	// design (security-hardening-bookorbit-url-validation-design.md §4.6,
	// §5.5) deliberately keeps the normalizer a pure normalizer.
}
