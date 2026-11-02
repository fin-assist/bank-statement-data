package importing_test

import (
	"errors"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitConfigSelectedCalendarDaysAndTrustedURL(t *testing.T) {
	for _, tc := range []struct{ zone, from, to, start, end string }{
		{"Europe/Moscow", "2026-10-01", "2026-10-31", "2026-09-30T21:00:00Z", "2026-10-31T21:00:00Z"},
		{"America/Toronto", "1919-03-31", "1919-03-31", "1919-03-31T04:30:00Z", "1919-04-01T04:00:00Z"},
		{"Pacific/Apia", "2011-12-30", "2011-12-31", "2011-12-30T10:00:00Z", "2011-12-31T10:00:00Z"},
		{"Europe/Moscow", "1900-01-01", "1900-01-01", "1899-12-31T21:29:43Z", "1900-01-01T21:29:43Z"},
		{"America/New_York", "2024-03-10", "2024-03-10", "2024-03-10T05:00:00Z", "2024-03-11T04:00:00Z"},
		{"UTC", "0001-01-01", "0001-01-01", "0001-01-01T00:00:00Z", "0001-01-02T00:00:00Z"},
	} {
		t.Run(tc.zone+tc.from, func(t *testing.T) {
			f := newImportReadFixture(t, importing.Snapshot{TimeZone: tc.zone, Operations: []importing.Operation{{At: readAt(t, "2026-10-01T12:00:00Z"), Currency: "RUB"}}}, readAt(t, "2026-11-01T12:00:00Z"))
			got, err := f.service.Config(t.Context(), "user", &importing.ConfigDates{From: &tc.from, To: &tc.to})
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeZone != tc.zone || !got.From.Equal(readAt(t, tc.start)) || !got.To.Equal(readAt(t, tc.end)) || got.Loaded == nil {
				t.Fatalf("wrong selected period %+v", got)
			}
			u, err := url.Parse(got.DownloadURL)
			if err != nil {
				t.Fatal(err)
			}
			if u.Scheme != "https" || u.Host != "www.tbank-online.com" || u.Query().Get("start") != strconv.FormatInt(got.From.UnixMilli(), 10) || u.Query().Get("end") != strconv.FormatInt(got.To.UnixMilli(), 10) {
				t.Fatalf("untrusted or wrong URL %s", got.DownloadURL)
			}
		})
	}
}

func TestUnitConfigDateErrors(t *testing.T) {
	ptr := func(s string) *string { return &s }
	for _, tc := range []struct {
		zone        string
		from, to    *string
		field, code string
	}{
		{"UTC", ptr("2026-01-01"), nil, "period_to", "REQUIRED"},
		{"UTC", nil, ptr("2026-01-01"), "period_from", "REQUIRED"},
		{"UTC", ptr(""), nil, "period_from", "INVALID_FORMAT"},
		{"UTC", ptr("2026-02-29"), ptr("2026-03-01"), "period_from", "INVALID_FORMAT"},
		{"UTC", ptr("0000-01-01"), ptr("2026-01-01"), "period_from", "INVALID_FORMAT"},
		{"UTC", ptr("2026-1-01"), ptr("2026-01-01"), "period_from", "INVALID_FORMAT"},
		{"UTC", ptr("2026-02-02"), ptr("2026-02-01"), "period_from", "OUT_OF_RANGE"},
		{"Pacific/Kiritimati", ptr("0001-01-01"), ptr("9999-12-31"), "period_to", "OUT_OF_RANGE"},
		{"Asia/Tokyo", ptr("0001-01-01"), ptr("2026-01-01"), "period_from", "OUT_OF_RANGE"},
		{"Pacific/Apia", ptr("2011-12-30"), ptr("2011-12-30"), "period_to", "OUT_OF_RANGE"},
		{"America/Los_Angeles", ptr("2026-11-01"), ptr("2026-11-01"), "period_to", "OUT_OF_RANGE"},
	} {
		t.Run(tc.zone+tc.field+tc.code+fmtDate(tc.from)+fmtDate(tc.to), func(t *testing.T) {
			f := newImportReadFixture(t, importing.Snapshot{TimeZone: tc.zone}, time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC))
			_, err := f.service.Config(t.Context(), "user", &importing.ConfigDates{From: tc.from, To: tc.to})
			var input *importing.InputError
			if !errors.As(err, &input) || input.Field != tc.field || input.Reason != tc.code {
				t.Fatalf("got %v want %s %s", err, tc.field, tc.code)
			}
		})
	}
}

func fmtDate(s *string) string {
	if s == nil {
		return "missing"
	}
	return *s
}

func TestUnitConfigCalendarUpperTimestampBoundary(t *testing.T) {
	for _, zone := range []string{"UTC", "Pacific/Kiritimati"} {
		f := newImportReadFixture(t, importing.Snapshot{TimeZone: zone}, time.Date(9999, 12, 31, 1, 0, 0, 0, time.UTC))
		from, to := "9999-12-30", "9999-12-31"
		out, err := f.service.Config(t.Context(), "user", &importing.ConfigDates{From: &from, To: &to})
		if zone == "UTC" {
			var input *importing.InputError
			if !errors.As(err, &input) || input.Field != "period_to" || input.Reason != "OUT_OF_RANGE" {
				t.Fatalf("unrepresentable exclusive end: %v", err)
			}
		} else if err != nil || !out.To.Equal(time.Date(9999, 12, 31, 10, 0, 0, 0, time.UTC)) {
			t.Fatalf("representable exclusive end: %v %v", out.To, err)
		}
	}
}
