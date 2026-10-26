package importing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func closureDay(day int) time.Time { return time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC) }
func closureUpload(id string, day int) Upload {
	return Upload{ID: id, Status: "done", UploadedAt: closureDay(day), AccountIDs: []int64{1}, Period: &Period{From: closureDay(1), To: closureDay(30)}}
}
func closureLine(upload, fitid string, day int) Unread {
	account := int64(1)
	line := Unread{UploadID: upload, Number: 10, AccountID: &account, FITID: fitid, Reason: "bad_amount"}
	if day != 0 {
		at := closureDay(day)
		line.OccurredAt = &at
	}
	return line
}

func TestUnitUnreadRequiresSameUploadSourceAndChoosesEarliestLater(t *testing.T) {
	old, middle, later := closureUpload("old", 1), closureUpload("middle", 2), closureUpload("later", 3)
	line := closureLine(old.ID, "fitid", 5)
	sources := []Source{{UploadID: later.ID, AccountID: 1, FITID: "fitid"}, {UploadID: middle.ID, AccountID: 1, FITID: "fitid"}}
	got, err := ResolveUnread(t.Context(), []Upload{later, old, middle}, []Unread{line}, sources, time.UTC)
	if err != nil || got[0].ResolvedByUpload != middle.ID || got[0].Resolution != "read" {
		t.Fatal(got, err)
	}
	if line.ResolvedByUpload != "" || line.Resolution != "" {
		t.Fatal("input mutated")
	}
	// A globally existing operation does not close a row unless its source is in
	// the newer upload. A same FITID on another account is a different operation.
	for _, wrong := range [][]Source{{{UploadID: old.ID, AccountID: 1, FITID: "fitid"}}, {{UploadID: later.ID, AccountID: 2, FITID: "fitid"}}, {{UploadID: later.ID, AccountID: 1, FITID: "another"}}} {
		got, err = ResolveUnread(t.Context(), []Upload{old, middle, later}, []Unread{line}, wrong, time.UTC)
		if err != nil || got[0].Resolution != "" {
			t.Fatal("unrelated source closed row", got, err)
		}
	}
	// Equal uploaded_at is ordered by ID, independently of completion/input order.
	a, b := closureUpload("a", 1), closureUpload("b", 1)
	got, err = ResolveUnread(t.Context(), []Upload{b, a}, []Unread{closureLine("a", "fitid", 5)}, []Source{{UploadID: "b", AccountID: 1, FITID: "fitid"}}, time.UTC)
	if err != nil || got[0].ResolvedByUpload != "b" {
		t.Fatal("lost equal-time tie-break", got, err)
	}
}

func TestUnitUnreadRepeatedBrokenFileReplacesAndDeleteReopens(t *testing.T) {
	a, b, c := closureUpload("a", 1), closureUpload("b", 2), closureUpload("c", 3)
	lines := []Unread{closureLine("a", "fitid", 5), closureLine("b", "fitid", 5)}
	got, err := ResolveUnread(t.Context(), []Upload{c, b, a}, lines, []Source{{UploadID: "c", AccountID: 1, FITID: "fitid"}}, time.UTC)
	if err != nil || got[0].ResolvedByUpload != "b" || got[0].Resolution != "replaced" || got[1].ResolvedByUpload != "c" || got[1].Resolution != "read" {
		t.Fatal("broken-file chain incorrect", got, err)
	}
	b.Status = "deleted"
	got, err = ResolveUnread(t.Context(), []Upload{a, b, c}, got[:1], []Source{{UploadID: "c", AccountID: 1, FITID: "fitid"}}, time.UTC)
	if err != nil || got[0].ResolvedByUpload != "c" || got[0].Resolution != "read" {
		t.Fatal("deletion did not recompute earliest closer", got, err)
	}
	c.Status = "deleted"
	got, err = ResolveUnread(t.Context(), []Upload{a, b, c}, got, nil, time.UTC)
	if err != nil || got[0].ResolvedByUpload != "" || got[0].Resolution != "" {
		t.Fatal("deleted closer left stale resolution", got, err)
	}
	// Completion order does not matter: a newly completed earlier row sees an
	// already completed later source during the same recomputation.
	b.Status = "done"
	got, err = ResolveUnread(t.Context(), []Upload{b, a}, lines[:1], []Source{{UploadID: "b", AccountID: 1, FITID: "fitid"}}, time.UTC)
	if err != nil || got[0].Resolution != "read" {
		t.Fatal(got, err)
	}
}

func TestUnitUnreadDateCoverageIsCalendarDayAndAccountScoped(t *testing.T) {
	zone := time.FixedZone("profile", 3*3600)
	a, b := closureUpload("a", 1), closureUpload("b", 2)
	b.Period = &Period{From: closureDay(5), To: closureDay(5)}
	line := closureLine("a", "", 5)
	// 21:30 UTC belongs to September 5 in the selected profile zone.
	at := closureDay(4).Add(21*time.Hour + 30*time.Minute)
	line.OccurredAt = &at
	got, err := ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, zone)
	if err != nil || got[0].Resolution != "read" {
		t.Fatal("ignored calendar zone or whole DTEND day", got, err)
	}
	broken := closureLine("b", "new-fitid", 5)
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line, broken}, nil, zone)
	if err != nil || got[0].Resolution != "replaced" {
		t.Fatal("new unread on same day should replace", got, err)
	}
	other := int64(2)
	broken.AccountID = &other
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line, broken}, nil, zone)
	if err != nil || got[0].Resolution != "read" {
		t.Fatal("another account blocked closure", got, err)
	}
	b.Period = nil
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, zone)
	if err != nil || got[0].Resolution != "" {
		t.Fatal("missing coverage invented", got, err)
	}
}

func TestUnitUnreadNoDateRequiresWholeCommonUploadRange(t *testing.T) {
	a, b := closureUpload("a", 1), closureUpload("b", 2)
	line := closureLine("a", "", 0)
	b.Period = &Period{From: closureDay(2), To: closureDay(30)}
	got, err := ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, time.UTC)
	if err != nil || got[0].Resolution != "" {
		t.Fatal("partial period closed no-date row", got, err)
	}
	b.Period = &Period{From: closureDay(1), To: closureDay(30)}
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, time.UTC)
	if err != nil || got[0].Resolution != "read" {
		t.Fatal(got, err)
	}
	for _, broken := range []Unread{closureLine("b", "", 0), closureLine("b", "f", 15)} {
		got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line, broken}, nil, time.UTC)
		if err != nil || got[0].Resolution != "replaced" {
			t.Fatal("broken replacement lost", got, err)
		}
	}
	// The shared period applies to every account in the upload, rather than
	// inventing individual account bounds from that account's operations.
	account := int64(2)
	line.AccountID = &account
	b.AccountIDs = []int64{1, 2}
	a.AccountIDs = []int64{1, 2}
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, time.UTC)
	if err != nil || got[0].Resolution != "read" {
		t.Fatal("common period was not applied to every account", got, err)
	}
	a.Period = nil
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, time.UTC)
	if err != nil || got[0].Resolution != "" {
		t.Fatal("missing old period inferred", got, err)
	}
}

func TestUnitUnreadUnknownOwnershipAndInvalidState(t *testing.T) {
	a, b := closureUpload("a", 1), closureUpload("b", 2)
	line := closureLine("a", "f", 5)
	line.Resolution = "read"
	line.ResolvedByUpload = "deleted"
	line.AccountID = nil
	got, err := ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, nil, time.UTC)
	if err != nil || got[0].Resolution != "" {
		t.Fatal("unknown account was closed", got, err)
	}
	line = closureLine("a", "f", 5)
	b.AccountIDs = nil
	got, err = ResolveUnread(t.Context(), []Upload{a, b}, []Unread{line}, []Source{{UploadID: "b", AccountID: 1, FITID: "f"}}, time.UTC)
	if err != nil || got[0].Resolution != "" {
		t.Fatal("missing candidate account ignored", got, err)
	}
	for _, uploads := range [][]Upload{{a, a}, {{ID: "", Status: "done", UploadedAt: a.UploadedAt}}, {{ID: "a", Status: "done"}}, {{ID: "a", Status: "done", UploadedAt: a.UploadedAt, Period: &Period{From: closureDay(5), To: closureDay(1)}}}} {
		if _, err = ResolveUnread(t.Context(), uploads, nil, nil, time.UTC); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
	if _, err = ResolveUnread(t.Context(), nil, nil, nil, nil); err == nil {
		t.Fatal("nil profile zone accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = ResolveUnread(ctx, nil, nil, nil, time.UTC); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
