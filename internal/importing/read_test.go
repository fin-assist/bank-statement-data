package importing_test

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type importReadContext struct{}

type importReadTransactions struct {
	t      *testing.T
	calls  int
	before error
	after  error
}

func (tx *importReadTransactions) Run(context.Context, func(context.Context) error) error {
	tx.t.Error("read operation attempted a write transaction")
	return errors.New("unexpected write transaction")
}
func (tx *importReadTransactions) Read(ctx context.Context, callback func(context.Context) error) error {
	tx.calls++
	if tx.before != nil {
		return tx.before
	}
	if err := callback(context.WithValue(ctx, importReadContext{}, true)); err != nil {
		return err
	}
	return tx.after
}

type importReadRepository struct {
	importing.Repository
	t      *testing.T
	state  importing.Snapshot
	calls  int
	user   string
	err    error
	onRead func()
}

func (r *importReadRepository) Snapshot(ctx context.Context, user string) (importing.Snapshot, error) {
	r.calls++
	if ctx.Value(importReadContext{}) != true {
		r.t.Error("snapshot escaped the single read transaction")
	}
	r.user = user
	if r.onRead != nil {
		r.onRead()
	}
	return r.state, r.err
}

// Any unexpected remote/key/tombstone use fails through the embedded nil port.
type importReadUnused struct {
	importing.ObjectStore
	importing.ObjectBarrier
	importing.Parser
	importing.Keys
	importing.Tombstones
}

type importReadFixture struct {
	t       *testing.T
	tx      *importReadTransactions
	repo    *importReadRepository
	now     time.Time
	service *importing.Service
}

func newImportReadFixture(t *testing.T, state importing.Snapshot, now time.Time) *importReadFixture {
	t.Helper()
	f := &importReadFixture{t: t, tx: &importReadTransactions{t: t}, repo: &importReadRepository{t: t, state: state}, now: now}
	unused := &importReadUnused{}
	var err error
	f.service, err = importing.New(f.tx, f.repo, unused, unused, unused, unused, unused, func() time.Time { return f.now }, func() (string, error) {
		t.Error("read generated an identity")
		return "", errors.New("unexpected identity")
	}, statementdatav1.StatementsService_UploadStatement_FullMethodName)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func readAt(t *testing.T, raw string) time.Time {
	t.Helper()
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestUnitImportConfigCalendarClampDSTAndBrowserURL(t *testing.T) {
	for _, tc := range []struct {
		name, now, from, to string
		dayHours            int
	}{
		{"leap day", "2024-02-29T12:00:00+01:00", "2023-02-28T00:00:00+01:00", "2024-03-01T00:00:00+01:00", 24},
		{"spring DST", "2024-03-31T12:00:00+02:00", "2023-03-31T00:00:00+02:00", "2024-04-01T00:00:00+02:00", 23},
		{"autumn DST", "2024-10-27T12:00:00+01:00", "2023-10-27T00:00:00+02:00", "2024-10-28T00:00:00+01:00", 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImportReadFixture(t, importing.Snapshot{TimeZone: "Europe/Berlin"}, readAt(t, tc.now))
			got, err := f.service.Config(context.Background(), "verified-user")
			if err != nil || !got.From.Equal(readAt(t, tc.from)) || !got.To.Equal(readAt(t, tc.to)) || got.Loaded != nil || got.TimeZone != "Europe/Berlin" {
				t.Fatal("suggested period changed calendar semantics", err)
			}
			zone, err := time.LoadLocation(got.TimeZone)
			if err != nil {
				t.Fatal(err)
			}
			local := f.now.In(zone)
			today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
			if got.To.Sub(today) != time.Duration(tc.dayHours)*time.Hour {
				t.Fatal("tomorrow was built as fixed 24 hours across DST")
			}
			link, err := url.Parse(got.DownloadURL)
			if err != nil || link.Scheme != "https" || link.Host != "www.tbank-online.com" || link.Path != "/mybank/api/operations/timeline/public/legacy/v1/export_operations" || got.LoginURL != "https://www.tbank.ru/login/" {
				t.Fatal("browser link used an unapproved endpoint")
			}
			q := link.Query()
			if q.Get("appName") != "supreme" || q.Get("appVersion") != "0.0.1" || q.Get("origin") != "web,ib5,platform" || q.Get("format") != "ofx" || q.Get("start") != strconv.FormatInt(got.From.UnixMilli(), 10) || q.Get("end") != strconv.FormatInt(got.To.UnixMilli(), 10) {
				t.Fatal("browser export parameters changed")
			}
			if f.tx.calls != 1 || f.repo.calls != 1 || f.repo.user != "verified-user" {
				t.Fatal("config did not use one user-scoped coherent snapshot")
			}
		})
	}
}

func TestUnitImportLoadedUsesActualNativeOperationsAndProfileDays(t *testing.T) {
	first, middle, last := readAt(t, "2026-09-01T10:00:00Z"), readAt(t, "2026-09-15T10:00:00Z"), readAt(t, "2026-09-28T02:00:00Z")
	state := importing.Snapshot{TimeZone: "America/New_York", Operations: []importing.Operation{{At: last, Currency: "JPY"}, {At: first, Currency: "RUB"}, {At: middle, Currency: "USD"}}}
	f := newImportReadFixture(t, state, readAt(t, "2026-10-01T03:00:00Z"))
	got, err := f.service.Config(context.Background(), "user")
	if err != nil || got.Loaded == nil || got.Loaded.Count != 3 || !got.Loaded.First.Equal(first) || !got.Loaded.Last.Equal(last) || got.From.Format(time.RFC3339) != "2026-09-27T00:00:00-04:00" || got.To.Format(time.RFC3339) != "2026-10-01T00:00:00-04:00" {
		t.Fatal("actual operation summary or profile-local latest day changed", err)
	}
	f = newImportReadFixture(t, importing.Snapshot{TimeZone: "invalid/private-zone"}, readAt(t, "2026-10-01T00:00:00Z"))
	got, err = f.service.Config(context.Background(), "user")
	if err != nil || got.TimeZone != "Europe/Moscow" {
		t.Fatal("unknown profile zone did not use documented Moscow fallback", err)
	}
}

func TestUnitImportHistoryVisibilityCountsAndGlobalCoverage(t *testing.T) {
	base := readAt(t, "2026-07-01T00:00:00Z")
	up := func(id, status string, day int, from, to int) importing.Upload {
		u := importing.Upload{ID: id, FileName: " exact " + id + ".ofx ", Status: status, UploadedAt: base.AddDate(0, 2, day), UnreadCount: 999}
		if from > 0 {
			u.Period = &importing.Period{From: base.AddDate(0, 0, from-1), To: base.AddDate(0, 0, to-1)}
		}
		return u
	}
	state := importing.Snapshot{TimeZone: "UTC", Uploads: []importing.Upload{up("a", "done", 0, 1, 10), up("p", "processing", 7, 13, 19), up("b", "done", 1, 8, 12), up("c", "done", 2, 20, 25), up("d", "done", 6, 0, 0), up("e", "done", 8, 0, 0), up("s", "staging", 10, 13, 19), up("f", "failed", 11, 13, 19), up("x", "deleted", 12, 13, 19)}, Operations: []importing.Operation{{At: base.Add(time.Hour), Currency: "USD"}}, Unread: []importing.Unread{{UploadID: "a", Number: 4, Reason: "bad_amount"}, {UploadID: "a", Number: 2, Reason: "no_date"}, {UploadID: "a", Number: 1, Reason: "bad_date", ResolvedByUpload: "b", Resolution: "read"}, {UploadID: "a", Number: 3, Reason: "truncated", ResolvedByUpload: "d", Resolution: "read"}, {UploadID: "a", Number: 5, Reason: "bad_date", ResolvedByUpload: "e", Resolution: "replaced"}, {UploadID: "p", Number: 1, Reason: "bad_date"}, {UploadID: "f", Number: 1, Reason: "bad_date"}}}
	f := newImportReadFixture(t, state, base.AddDate(0, 3, 0))
	got, err := f.service.History(context.Background(), "user")
	if err != nil || len(got.Uploads) != 6 || got.Loaded == nil || got.Loaded.Count != 1 || len(got.Gaps) != 1 || !got.Gaps[0].From.Equal(base.AddDate(0, 0, 12)) || !got.Gaps[0].To.Equal(base.AddDate(0, 0, 19)) {
		t.Fatal("history visibility, native summary or complete global gap changed", err)
	}
	if got.Uploads[0].ID != "e" || got.Uploads[1].ID != "p" {
		t.Fatal("history not newest first")
	}
	for _, u := range got.Uploads {
		if u.Status == "processing" && (u.Period != nil || u.UnreadCount != 0) {
			t.Fatal("processing row exposed provisional dates/counts")
		}
		if u.ID == "a" && (u.UnreadCount != 2 || u.ResolvedUnreadCount != 2 || u.ResolvedByUploadedAt == nil || !u.ResolvedByUploadedAt.Equal(state.Uploads[4].UploadedAt)) {
			t.Fatal("read/replaced resolution counters or latest read resolver changed")
		}
	}
	if state.Uploads[0].UnreadCount != 999 || state.Uploads[1].Period == nil || f.tx.calls != 1 || f.repo.calls != 1 {
		t.Fatal("history mutated snapshot or combined different read transactions")
	}
}

func TestUnitImportHistoryEmptyAndImmutableFilePeriodFallback(t *testing.T) {
	first, last := readAt(t, "2026-09-02T10:00:00Z"), readAt(t, "2026-09-05T10:00:00Z")
	state := importing.Snapshot{TimeZone: "UTC", Uploads: []importing.Upload{{ID: "empty", Status: "done", UploadedAt: last, Period: &importing.Period{From: first, To: last}}}}
	f := newImportReadFixture(t, state, last)
	got, err := f.service.History(context.Background(), "user")
	if err != nil || got.Loaded != nil || len(got.Uploads) != 1 || got.Uploads[0].Period == nil {
		t.Fatal("empty completed upload invented summary timestamps or disappeared", err)
	}
	state.Uploads[0].Period = nil
	state.Uploads[0].Result = &importing.Result{FirstOperationAt: &first, LastOperationAt: &last}
	later := last.AddDate(0, 0, 10)
	state.Operations = []importing.Operation{{At: later, AccountID: 1, FITID: "same"}}
	state.Sources = []importing.Source{{UploadID: "empty", AccountID: 1, FITID: "same"}}
	f = newImportReadFixture(t, state, later)
	got, err = f.service.History(context.Background(), "user")
	if err != nil || got.Uploads[0].Period.From.Day() != 2 || got.Uploads[0].Period.To.Day() != 5 {
		t.Fatal("current duplicate operation overwrote immutable file date fallback", err)
	}
	state.Uploads[0].Result = nil
	f = newImportReadFixture(t, state, later)
	got, err = f.service.History(context.Background(), "user")
	if err != nil || got.Uploads[0].Period.From.Day() != 15 || got.Uploads[0].Period.To.Day() != 15 {
		t.Fatal("legacy source membership fallback did not use its own operation", err)
	}
}

func TestUnitImportUnreadUsesDoneUnresolvedRowsAndHalfOpenRange(t *testing.T) {
	from, to := readAt(t, "2026-09-01T00:00:00Z"), readAt(t, "2026-09-02T00:00:00Z")
	mid := from.Add(time.Hour)
	state := importing.Snapshot{TimeZone: "UTC", Uploads: []importing.Upload{{ID: "a", Status: "done", FileName: "exact a.ofx", UploadedAt: from}, {ID: "b", Status: "done", FileName: "exact b.ofx", UploadedAt: to}, {ID: "p", Status: "processing"}, {ID: "f", Status: "failed"}, {ID: "x", Status: "deleted"}, {ID: "s", Status: "staging"}}, Unread: []importing.Unread{{UploadID: "a", Number: 5, OccurredAt: &from, Reason: "bad_amount"}, {UploadID: "a", Number: 2, OccurredAt: &to, Reason: "truncated"}, {UploadID: "a", Number: 1, Reason: "no_date"}, {UploadID: "a", Number: 3, Reason: "bad_date", ResolvedByUpload: "b", Resolution: "read"}, {UploadID: "a", Number: 4, Reason: "bad_date", ResolvedByUpload: "b", Resolution: "replaced"}, {UploadID: "b", Number: 7, OccurredAt: &mid, Reason: "bad_amount"}, {UploadID: "p", Number: 8, OccurredAt: &mid, Reason: "bad_amount"}, {UploadID: "f", Number: 9, OccurredAt: &mid, Reason: "bad_amount"}}}
	f := newImportReadFixture(t, state, to)
	rows, zone, err := f.service.Unread(context.Background(), "verified-user", importing.Scope{UploadID: "a"})
	if err != nil || zone != "UTC" || len(rows) != 3 || rows[0].Number != 1 || rows[1].Number != 2 || rows[2].Number != 5 || rows[0].FileName != "exact a.ofx" {
		t.Fatal("per-file unread order/resolution/name changed", err)
	}
	f = newImportReadFixture(t, state, to)
	rows, _, err = f.service.Unread(context.Background(), "verified-user", importing.Scope{Period: &importing.TimeRange{From: from, To: to}})
	if err != nil || len(rows) != 2 || rows[0].Number != 5 || rows[1].Number != 7 || f.tx.calls != 1 || f.repo.calls != 1 || f.repo.user != "verified-user" {
		t.Fatal("period unread included no-date/end/resolved/non-DONE rows or split snapshot", err)
	}
	for _, id := range []string{"foreign-or-missing", "x", "s"} {
		if _, _, err := f.service.Unread(context.Background(), "user", importing.Scope{UploadID: id}); !errors.Is(err, importing.ErrNotFound) {
			t.Fatal("missing/private/deleted upload did not return published404")
		}
	}
	for _, id := range []string{"f", "p"} {
		if rows, _, err := f.service.Unread(context.Background(), "user", importing.Scope{UploadID: id}); err != nil || len(rows) != 0 {
			t.Fatal("existing non-DONE upload exposed unread rows", err)
		}
	}
}

func TestUnitImportReadScopeAndDataFailures(t *testing.T) {
	now := readAt(t, "2026-09-30T00:00:00Z")
	for _, scope := range []importing.Scope{{}, {UploadID: "a", Period: &importing.TimeRange{From: now, To: now.Add(time.Hour)}}, {Period: &importing.TimeRange{}}, {Period: &importing.TimeRange{From: now, To: now}}, {Period: &importing.TimeRange{From: now.Add(time.Hour), To: now}}} {
		f := newImportReadFixture(t, importing.Snapshot{}, now)
		_, _, err := f.service.Unread(context.Background(), "user", scope)
		var input *importing.InputError
		if !errors.As(err, &input) || f.tx.calls != 0 || f.repo.calls != 0 {
			t.Fatal("invalid scope reached storage")
		}
	}
	for name, state := range map[string]importing.Snapshot{
		"unknown status":      {Uploads: []importing.Upload{{ID: "a", Status: "private-unknown"}}},
		"duplicate upload":    {Uploads: []importing.Upload{{ID: "a", Status: "done"}, {ID: "a", Status: "done"}}},
		"zero operation time": {Operations: []importing.Operation{{}}},
		"partial resolution":  {Uploads: []importing.Upload{{ID: "a", Status: "done", UploadedAt: now}}, Unread: []importing.Unread{{UploadID: "a", Number: 1, Reason: "no_date", Resolution: "read"}}},
		"unknown reason":      {Uploads: []importing.Upload{{ID: "a", Status: "done", UploadedAt: now}}, Unread: []importing.Unread{{UploadID: "a", Number: 1, Reason: "private-unknown"}}},
		"missing resolver":    {Uploads: []importing.Upload{{ID: "a", Status: "done", UploadedAt: now}}, Unread: []importing.Unread{{UploadID: "a", Number: 1, Reason: "no_date", ResolvedByUpload: "missing", Resolution: "read"}}},
		"self resolution":     {Uploads: []importing.Upload{{ID: "a", Status: "done", UploadedAt: now}}, Unread: []importing.Unread{{UploadID: "a", Number: 1, Reason: "no_date", ResolvedByUpload: "a", Resolution: "read"}}},
		"unknown resolution":  {Uploads: []importing.Upload{{ID: "a", Status: "done", UploadedAt: now}, {ID: "b", Status: "done", UploadedAt: now.Add(time.Hour)}}, Unread: []importing.Unread{{UploadID: "a", Number: 1, Reason: "no_date", ResolvedByUpload: "b", Resolution: "private-unknown"}}},
		"bad period":          {Uploads: []importing.Upload{{ID: "a", Status: "done", UploadedAt: now, Period: &importing.Period{From: now, To: now.Add(-time.Hour)}}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newImportReadFixture(t, state, now)
			if _, err := f.service.History(context.Background(), "user"); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid read state accepted or exposed stored input")
			}
		})
	}
}

func TestUnitImportReadPropagatesErrorsAndCancellationWithoutPartialData(t *testing.T) {
	now := readAt(t, "2026-09-30T00:00:00Z")
	boom := errors.New("synthetic read failure")
	for _, phase := range []string{"before transaction", "repository", "after snapshot", "canceled before", "canceled during"} {
		t.Run(phase, func(t *testing.T) {
			f := newImportReadFixture(t, importing.Snapshot{TimeZone: "UTC", Operations: []importing.Operation{{At: now}}}, now)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := boom
			switch phase {
			case "before transaction":
				f.tx.before = boom
			case "repository":
				f.repo.err = boom
			case "after snapshot":
				f.tx.after = boom
			case "canceled before":
				cancel()
				want = context.Canceled
			case "canceled during":
				f.repo.onRead = cancel
				want = context.Canceled
			}
			got, err := f.service.Config(ctx, "user")
			if !errors.Is(err, want) || got.Loaded != nil || got.DownloadURL != "" {
				t.Fatal("read failure lost or leaked partial snapshot", err)
			}
		})
	}
}
