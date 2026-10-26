package statementwire_test

import (
	"math"
	"strings"
	"testing"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/adapter/statementwire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func richResult() importing.Result {
	zone := time.FixedZone("profile", 3*3600)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	end := start.AddDate(0, 1, 0)
	first, last := start.Add(time.Hour), end.Add(-time.Hour)
	mask, short := "1234", "x"
	full, withData := 3, 2
	return importing.Result{
		OperationCount: 3, NewCount: 2, DuplicateCount: 1, UnreadCount: 4,
		FirstOperationAt: &first, LastOperationAt: &last, FirstImport: true,
		Totals: &importing.Totals{Scope: "all", ExpenseMinor: -101, IncomeMinor: 500},
		Accounts: []importing.Account{
			{Key: "private-account-checking", Type: "checking", Mask: &mask},
			{Key: "private-account-savings", Type: "savings", Mask: &mask},
			{Key: "private-account-other", Type: "checking", OtherBank: true, Mask: &mask},
			{Key: "private-account-future", Type: "future_kind", Mask: &short},
		},
		CategorizedCount: 2, OwnTransferCount: 1,
		Coverage: importing.Coverage{FullMonths: 3, FullMonthsBefore: 2, NewlyFullMonths: []string{"2026-08"}, IncompleteMonths: []importing.IncompleteMonth{{Month: "2026-09", Range: importing.TimeRange{From: start, To: end}, DataFrom: start, DataTo: last, Gaps: []importing.TimeRange{{From: start.AddDate(0, 0, 5), To: start.AddDate(0, 0, 8)}}, InProgress: true}}, Features: []importing.FeatureAvailability{{Feature: "comparison", Open: true, OpenedNow: true}, {Feature: "notable_spending", RequiredFullMonths: &full}, {Feature: "monthly_chart", RequiredMonthsWithData: &withData}}},
		Notices:  []string{"no_expenses"},
	}
}

func TestUnitResultPublicProtoJSONRoundTrip(t *testing.T) {
	r := richResult()
	p, err := statementwire.ToResult(r)
	if err != nil {
		t.Fatal(err)
	}
	data, err := protojson.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-account") || strings.Contains(string(data), "FITID") {
		t.Fatal("source identities leaked into persisted/public protobuf")
	}
	var restoredPB statementdatav1.ImportResult
	if err := protojson.Unmarshal(data, &restoredPB); err != nil {
		t.Fatal(err)
	}
	restored, err := statementwire.FromResult(&restoredPB)
	if err != nil {
		t.Fatal(err)
	}
	again, err := statementwire.ToResult(restored)
	if err != nil || !proto.Equal(p, again) {
		t.Fatal("public snapshot did not survive protojson roundtrip", err)
	}
	for _, a := range restored.Accounts {
		if a.Key != "" {
			t.Fatal("reverse projection restored private account identity")
		}
	}
	if p.Accounts[0].GetMask() != "··1234" || p.Accounts[1].TypeName != "Накопительный" || p.Accounts[2].Mask != nil || p.Accounts[2].TypeName != "Счёт другого банка" || p.Accounts[3].TypeName != "Счёт" || p.Accounts[3].GetMask() != "··x" {
		t.Fatal("account presentation mapping incorrect")
	}
	if p.Totals.ExpenseMinor != -101 || p.UnreadCount != 4 || len(p.Coverage.Features) != 3 || p.Coverage.IncompleteMonths[0].DataFrom.AsTime().Unix() != r.Coverage.IncompleteMonths[0].DataFrom.Unix() {
		t.Fatal("result values were recalculated rather than projected")
	}
	*p.Accounts[0].Mask = "changed"
	p.Coverage.NewlyFullMonths[0] = "changed"
	if *r.Accounts[0].Mask != "1234" || r.Coverage.NewlyFullMonths[0] != "2026-08" {
		t.Fatal("projection aliased input pointers/slices")
	}
}

func TestUnitEmptyAndDuplicateResults(t *testing.T) {
	for _, r := range []importing.Result{{Notices: []string{"empty_statement"}}, {OperationCount: 3, DuplicateCount: 3, FirstOperationAt: richResult().FirstOperationAt, LastOperationAt: richResult().LastOperationAt}} {
		p, err := statementwire.ToResult(r)
		if err != nil || p.Totals != nil || p.Coverage == nil {
			t.Fatal("empty/duplicate-only result invalid", err)
		}
		restored, err := statementwire.FromResult(p)
		if err != nil {
			t.Fatal(err)
		}
		again, err := statementwire.ToResult(restored)
		if err != nil || !proto.Equal(p, again) {
			t.Fatal("empty result roundtrip lost presence", err)
		}
	}
}

func TestUnitOpaqueUncategorizedSelectionSurvivesPersistence(t *testing.T) {
	r := richResult()
	r.UncategorizedCount, r.CategorizedCount = 1, 1
	rangeValue := r.Coverage.IncompleteMonths[0].Range
	r.UncategorizedRange = &rangeValue
	r.UncategorizedSelection, r.UncategorizedSelectionName = "opaque-actual-selector", "Actual stored label"
	p, err := statementwire.ToResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if p.UncategorizedFilters.GetSelection() != r.UncategorizedSelection || p.UncategorizedFilters.GetSelectionName() != r.UncategorizedSelectionName || p.UncategorizedFilters.CategoryId != nil {
		t.Fatal("uncategorized selection replaced with category/default")
	}
	data, err := protojson.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var stored statementdatav1.ImportResult
	if err := protojson.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	restored, err := statementwire.FromResult(&stored)
	if err != nil || restored.UncategorizedSelection != r.UncategorizedSelection || restored.UncategorizedSelectionName != r.UncategorizedSelectionName {
		t.Fatal("persisted opaque selection not retained", err)
	}
	again, err := statementwire.ToResult(restored)
	if err != nil || !proto.Equal(p, again) {
		t.Fatal("selection payload changed on roundtrip", err)
	}
	query := "unsupported query"
	p.UncategorizedFilters.Q = &query
	if _, err := statementwire.FromResult(p); err == nil {
		t.Fatal("unsupported persisted filter silently lost")
	}
	r.UncategorizedSelection = ""
	if _, err := statementwire.ToResult(r); err == nil {
		t.Fatal("missing opaque selection inferred from range")
	}
}

func TestUnitInvalidResultFailsWithoutSourceDiagnostics(t *testing.T) {
	for name, change := range map[string]func(*importing.Result){
		"negative":    func(r *importing.Result) { r.OperationCount = -1 },
		"sum":         func(r *importing.Result) { r.DuplicateCount = 2 },
		"new":         func(r *importing.Result) { r.NewCount = 4 },
		"categorized": func(r *importing.Result) { r.CategorizedCount = 1 },
		"own":         func(r *importing.Result) { r.OwnTransferCount = 3 },
		"first":       func(r *importing.Result) { r.FirstOperationAt = nil },
		"order": func(r *importing.Result) {
			r.FirstOperationAt, r.LastOperationAt = r.LastOperationAt, r.FirstOperationAt
		},
		"totals":    func(r *importing.Result) { r.Totals = nil },
		"scope":     func(r *importing.Result) { r.Totals.Scope = "private-unknown" },
		"mask":      func(r *importing.Result) { secret := "private-full-account"; r.Accounts[0].Mask = &secret },
		"feature":   func(r *importing.Result) { r.Coverage.Features[0].Feature = "private-future" },
		"month":     func(r *importing.Result) { r.Coverage.NewlyFullMonths[0] = "2026-13" },
		"overflow":  func(r *importing.Result) { r.Coverage.FullMonths = math.MaxInt32 + 1 },
		"threshold": func(r *importing.Result) { n := -1; r.Coverage.Features[0].RequiredFullMonths = &n },
		"range":     func(r *importing.Result) { r.Coverage.IncompleteMonths[0].Range.To = time.Time{} },
		"notice":    func(r *importing.Result) { r.Notices = []string{"private-future"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := richResult()
			change(&r)
			_, err := statementwire.ToResult(r)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid projection accepted or leaked diagnostics")
			}
		})
	}
}

func TestUnitMalformedPersistedProjectionFailsClosed(t *testing.T) {
	for name, change := range map[string]func(*statementdatav1.ImportResult){
		"nil-account":     func(p *statementdatav1.ImportResult) { p.Accounts[0] = nil },
		"account-name":    func(p *statementdatav1.ImportResult) { p.Accounts[0].TypeName = "private-bank-account" },
		"other-bank-type": func(p *statementdatav1.ImportResult) { p.Accounts[2].TypeName = "Текущий" },
		"mask":            func(p *statementdatav1.ImportResult) { s := "1234"; p.Accounts[0].Mask = &s },
		"nil-coverage":    func(p *statementdatav1.ImportResult) { p.Coverage = nil },
		"nil-month":       func(p *statementdatav1.ImportResult) { p.Coverage.IncompleteMonths[0] = nil },
		"bad-timestamp": func(p *statementdatav1.ImportResult) {
			p.FirstOperationAt = &timestamppb.Timestamp{Seconds: math.MaxInt64}
		},
		"nil-feature":     func(p *statementdatav1.ImportResult) { p.Coverage.Features[0] = nil },
		"unknown-feature": func(p *statementdatav1.ImportResult) { p.Coverage.Features[0].Feature = 999 },
		"unknown-notice":  func(p *statementdatav1.ImportResult) { p.Notices = []statementdatav1.ImportNotice{999} },
		"unknown-scope":   func(p *statementdatav1.ImportResult) { p.Totals.Scope = 999 },
		"missing-range":   func(p *statementdatav1.ImportResult) { p.Coverage.IncompleteMonths[0].Range = nil },
		"missing-date":    func(p *statementdatav1.ImportResult) { p.Coverage.IncompleteMonths[0].DataTo = nil },
		"invalid-gap":     func(p *statementdatav1.ImportResult) { p.Coverage.IncompleteMonths[0].Gaps = []*commonv1.TimeRange{{}} },
	} {
		t.Run(name, func(t *testing.T) {
			p, err := statementwire.ToResult(richResult())
			if err != nil {
				t.Fatal(err)
			}
			change(p)
			if _, err := statementwire.FromResult(p); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid persisted projection accepted or leaked source")
			}
		})
	}
	if _, err := statementwire.FromResult(nil); err == nil {
		t.Fatal("nil result accepted")
	}
}

func TestUnitHistoryAndUnreadPreservePublicMetadata(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("profile", 3*3600))
	u := importing.Upload{ID: "upload", UserID: "private-user", FileName: strings.Repeat("x", 260) + ".ofx", ObjectKey: "private-object", Status: "done", UploadedAt: now, Period: &importing.Period{From: now.AddDate(0, 0, -29), To: now}, OperationCount: 12, UnreadCount: 2, ResolvedUnreadCount: 3, ResolvedByUploadedAt: &now}
	p, err := statementwire.ToUpload(u)
	if err != nil || p.FileName != u.FileName || p.GetPeriodFrom() != "2026-09-01" || p.GetPeriodTo() != "2026-09-30" || p.SourceName != "Выписка Т-Банка" || p.ResolvedUnreadCount != 3 {
		t.Fatal("history metadata lost", err)
	}
	data, _ := protojson.Marshal(p)
	if strings.Contains(string(data), "private-") {
		t.Fatal("private upload fields leaked")
	}
	u.Status = "processing"
	p, err = statementwire.ToUpload(u)
	if err != nil || p.Status != statementdatav1.UploadStatus_UPLOAD_STATUS_PROCESSING || p.PeriodFrom != nil || p.PeriodTo != nil {
		t.Fatal("processing upload exposed provisional period", err)
	}
	for _, reason := range []string{"bad_amount", "bad_date", "no_date", "truncated"} {
		var date *time.Time
		if reason == "bad_amount" || reason == "truncated" {
			date = &now
		}
		line, err := statementwire.ToUnread(importing.Unread{UploadID: "upload", FileName: "exact.ofx", Number: 412, OccurredAt: date, Reason: reason, FITID: "private-fitid"})
		if err != nil || line.LineNumber != 412 || line.Reason == 0 || line.ReasonName == "" || (line.Date == nil) != (date == nil) || date != nil && line.Date.AsTime().Unix() != now.Unix() {
			t.Fatal("unread metadata mapping failed", err)
		}
		data, _ := protojson.Marshal(line)
		if strings.Contains(string(data), "private-fitid") {
			t.Fatal("private FITID leaked")
		}
	}
}

func TestUnitHistoryUnknownAndInvalidValuesFail(t *testing.T) {
	now := time.Now()
	for _, u := range []importing.Upload{{Status: "failed", UploadedAt: now}, {Status: "deleted", UploadedAt: now}, {Status: "done"}, {Status: "done", UploadedAt: now, UnreadCount: -1}, {Status: "done", UploadedAt: now, ResolvedUnreadCount: 1}, {Status: "done", UploadedAt: now, ResolvedByUploadedAt: &now}} {
		if _, err := statementwire.ToUpload(u); err == nil {
			t.Fatal("invalid history projection accepted")
		}
	}
	for _, u := range []importing.Unread{{Number: 1, Reason: "unknown"}, {Number: 0, Reason: "bad_date"}, {Number: math.MaxInt32 + 1, Reason: "truncated"}} {
		if _, err := statementwire.ToUnread(u); err == nil {
			t.Fatal("invalid unread line silently projected")
		}
	}
}

func TestUnitTerminalFailureCarriesOnlyResultProfile(t *testing.T) {
	for _, tc := range []struct {
		input, reason string
		code          codes.Code
		legacy        statementdatav1.ImportErrorCode
	}{{"cancelled", "CANCELLED", codes.Canceled, statementdatav1.ImportErrorCode_IMPORT_ERROR_CODE_CANCELLED}, {"processing_failed", "PROCESSING_FAILED", codes.FailedPrecondition, statementdatav1.ImportErrorCode_IMPORT_ERROR_CODE_PROCESSING_FAILED}} {
		p, err := statementwire.Failure(tc.input)
		if err != nil || p.Code != tc.legacy || p.Message != tc.reason || p.Status.GetCode() != int32(tc.code) {
			t.Fatal("failure projection mismatch", err)
		}
		parsed, ok := finerr.DecodeResultStatus(p.Status)
		if !ok || parsed.Reason != tc.reason || len(p.Status.Details) != 1 {
			t.Fatal("invalid result profile or unexpected retry details")
		}
	}
	for _, code := range []string{"", "RATE_LIMITED", "private-diagnostic"} {
		if _, err := statementwire.Failure(code); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("unknown failure accepted or diagnostics leaked")
		}
	}
}
