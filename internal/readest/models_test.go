package readest

import (
	"encoding/json"
	"testing"

	"github.com/Riffsmith/bookorbit-readest-sync/internal/util"
)

func TestBookRowPredicates(t *testing.T) {
	dummy := BookRow{BookHash: DummyHash}
	if !dummy.IsDummy() {
		t.Error("IsDummy should be true for the sentinel hash")
	}
	if (BookRow{BookHash: "abc"}).IsDummy() {
		t.Error("IsDummy should be false for a real hash")
	}

	if !((BookRow{DeletedAt: "2026-01-02T03:04:05Z"}).IsDeleted()) {
		t.Error("IsDeleted should be true when deleted_at set")
	}
	if (BookRow{DeletedAt: ""}).IsDeleted() || (BookRow{DeletedAt: "null"}).IsDeleted() {
		t.Error("IsDeleted should be false for empty/null deleted_at")
	}
}

func TestProgressTupleDecoding(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantCur   float64
		wantTotal float64
		wantOK    bool
	}{
		{name: "array", raw: `[42, 250]`, wantCur: 42, wantTotal: 250, wantOK: true},
		{name: "stringified array", raw: `"[42, 250]"`, wantCur: 42, wantTotal: 250, wantOK: true},
		{name: "null", raw: `null`, wantOK: false},
		{name: "empty string", raw: `""`, wantOK: false},
		{name: "wrong length", raw: `[1, 2, 3]`, wantOK: false},
		{name: "not a tuple", raw: `{"a":1}`, wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := BookRow{Progress: json.RawMessage(c.raw)}
			cur, tot, ok := r.ProgressTuple()
			if ok != c.wantOK {
				t.Fatalf("ProgressTuple ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (cur != c.wantCur || tot != c.wantTotal) {
				t.Fatalf("ProgressTuple = (%v,%v), want (%v,%v)", cur, tot, c.wantCur, c.wantTotal)
			}
		})
	}
}

func TestPercentage(t *testing.T) {
	r := BookRow{Progress: json.RawMessage(`[125, 250]`)}
	pct, ok := r.Percentage()
	if !ok || pct != 0.5 {
		t.Errorf("Percentage = %v,%v; want 0.5,true", pct, ok)
	}

	// Zero total must not be ok.
	r = BookRow{Progress: json.RawMessage(`[5, 0]`)}
	if _, ok := r.Percentage(); ok {
		t.Error("Percentage with zero total should be ok=false")
	}
}

func TestWatermarkMs(t *testing.T) {
	r := BookRow{
		SyncedAt:  "2026-01-03T00:00:00Z", // newest
		UpdatedAt: "2026-01-02T00:00:00Z",
		DeletedAt: "",
	}
	got := r.WatermarkMs()
	want := util.MustISOToMs("2026-01-03T00:00:00Z")
	if got != want {
		t.Errorf("WatermarkMs = %d, want synced_at %d", got, want)
	}

	// deleted_at newest should win too.
	r = BookRow{UpdatedAt: "2026-01-02T00:00:00Z", DeletedAt: "2026-01-05T00:00:00Z"}
	if got := r.WatermarkMs(); got != util.MustISOToMs("2026-01-05T00:00:00Z") {
		t.Errorf("WatermarkMs with delete = %d, want deleted_at", got)
	}
}

func TestBooksResponseDecoding(t *testing.T) {
	body := `{"books":[{"book_hash":"h1","title":"T","progress":[10,100],"updated_at":"2026-01-02T00:00:00Z"}]}`
	var resp BooksResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode BooksResponse: %v", err)
	}
	if len(resp.Books) != 1 || resp.Books[0].BookHash != "h1" {
		t.Fatalf("decoded books = %+v", resp.Books)
	}
}

func TestBookRowReadingStatusFieldsDecode(t *testing.T) {
	// A row carrying reading_status / reading_status_updated_at decodes both
	// fields straight off the bulk-pull wire (Phase 9 design §3.1). The
	// reading-status timestamp arrives as an ISO-8601 string and is stored
	// raw, matching the convention of updated_at/deleted_at/synced_at.
	body := `{"books":[{"book_hash":"h1","reading_status":"finished","reading_status_updated_at":"2026-06-18T00:00:00+00:00"}]}`
	var resp BooksResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode BooksResponse: %v", err)
	}
	if len(resp.Books) != 1 {
		t.Fatalf("decoded books len = %d, want 1", len(resp.Books))
	}
	row := resp.Books[0]
	if row.ReadingStatus != "finished" {
		t.Errorf("ReadingStatus = %q, want finished", row.ReadingStatus)
	}
	if row.ReadingStatusUpdatedAt != "2026-06-18T00:00:00+00:00" {
		t.Errorf("ReadingStatusUpdatedAt = %q, want raw ISO string", row.ReadingStatusUpdatedAt)
	}
	// The raw ISO string must parse via the existing ISOToMs port, pinning the
	// exact conversion the engine would apply if it ever needed the ms value.
	if _, err := util.ISOToMs(row.ReadingStatusUpdatedAt); err != nil {
		t.Errorf("ReadingStatusUpdatedAt must parse via ISOToMs: %v", err)
	}
}

func TestBookRowReadingStatusFieldsAbsentDecodeEmpty(t *testing.T) {
	// Rows for books that have never had a reading status set carry
	// absent (or null) reading_status / reading_status_updated_at. Both must
	// decode to "" without error — the same tolerance discipline already
	// applied to a null progress tuple (TestProgressTupleDecoding).
	for _, body := range []string{
		`{"books":[{"book_hash":"h1"}]}`,
		`{"books":[{"book_hash":"h1","reading_status":null,"reading_status_updated_at":null}]}`,
	} {
		var resp BooksResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		row := resp.Books[0]
		if row.ReadingStatus != "" {
			t.Errorf("ReadingStatus = %q, want empty", row.ReadingStatus)
		}
		if row.ReadingStatusUpdatedAt != "" {
			t.Errorf("ReadingStatusUpdatedAt = %q, want empty", row.ReadingStatusUpdatedAt)
		}
	}
}

func TestTokenFreshnessRules(t *testing.T) {
	tok := Token{ExpiresAt: 1000, ExpiresIn: 600}

	// 50% TTL rule: halfway is 1000-300=700. At now=800 (>700) it should refresh.
	if !tok.ShouldRefresh(800) {
		t.Error("ShouldRefresh should be true past the 50% TTL point")
	}
	// At now=100 (<700) it should not refresh yet.
	if tok.ShouldRefresh(100) {
		t.Error("ShouldRefresh should be false before the 50% TTL point")
	}
	// Zero TTL forces refresh.
	if !(Token{ExpiresAt: 1000, ExpiresIn: 0}).ShouldRefresh(0) {
		t.Error("ShouldRefresh with zero TTL should be true")
	}

	// 60-second expiry guard.
	if !tok.ExpiresWithin(950, 60) {
		t.Error("ExpiresWithin should be true when expiry is within 60s")
	}
	if tok.ExpiresWithin(100, 60) {
		t.Error("ExpiresWithin should be false when expiry is far off")
	}
}
