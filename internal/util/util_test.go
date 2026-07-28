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
	// Known MD5 of "password".
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

	// Early stop.
	chunks = 0
	BatchFunc(in, 2, func(c []int) bool {
		chunks++
		return false
	})
	if chunks != 1 {
		t.Errorf("BatchFunc early stop visited %d chunks, want 1", chunks)
	}
}
