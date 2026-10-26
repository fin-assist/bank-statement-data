package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type importScanRow struct {
	values []any
	err    error
}

func (row importScanRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(dest) != len(row.values) {
		return errors.New("fixture field count")
	}
	for i, value := range row.values {
		target := reflect.ValueOf(dest[i]).Elem()
		if value == nil {
			target.SetZero()
		} else {
			target.Set(reflect.ValueOf(value))
		}
	}
	return nil
}
func projectionRow() []any {
	return []any{"b7a17bfd-cf78-44a3-95f7-09941c59e996", "73ef1f95-14d5-453c-80ec-d0e2ac8c854d", "exact/имя", "private-key", int64(8), make([]byte, 32), "processing", "", 0, time.Now().UTC(), nil, nil, int64(0), []int64{}, nil, nil, nil}
}
func TestUnitStoredUploadProjectionRejectsCorruptFacts(t *testing.T) {
	zone, _ := time.LoadLocation("Europe/Berlin")
	for _, tc := range []struct {
		name  string
		field int
		value any
	}{
		{"short digest", 5, []byte{1}}, {"zero size", 4, int64(0)}, {"negative progress", 8, -1}, {"overflow progress", 8, 101},
		{"one date", 14, ptrDate(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))}, {"malformed result", 16, []byte(`{"private":"malformed"`)},
		{"invalid result enum", 16, []byte(`{"newCount":"-1"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := projectionRow()
			v[tc.field] = tc.value
			if _, err := scanUpload(importScanRow{values: v}, zone); !errors.Is(err, errImportStorage) {
				t.Fatalf("corrupt value accepted: %v", err)
			}
		})
	}
	boom := errors.New("private DB diagnostic")
	if _, err := scanUpload(importScanRow{err: boom}, zone); !errors.Is(err, boom) {
		t.Fatal("scan cause lost before storage boundary")
	}
	v := projectionRow()
	from := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	v[14], v[15] = &from, &to
	got, err := scanUpload(importScanRow{values: v}, zone)
	if err != nil || got.FileName != "exact/имя" || got.Period == nil || got.Period.From.Format("2006-01-02") != "2026-03-29" || got.Period.From.Location() != zone {
		t.Fatalf("profile calendar projection: %+v %v", got, err)
	}
}
func TestUnitStoredEmptyResultIsRestoredAsAnActualResult(t *testing.T) {
	v := projectionRow()
	v[6] = "done"
	v[8] = 100
	v[10] = ptrDate(time.Now().UTC())
	v[16] = []byte(`{"coverage":{},"notices":["IMPORT_NOTICE_EMPTY_STATEMENT"]}`)
	got, err := scanUpload(importScanRow{values: v}, time.UTC)
	if err != nil || got.Result == nil || got.Result.OperationCount != 0 || len(got.Result.Notices) != 1 || got.Result.Notices[0] != "empty_statement" || got.FinishedAt == nil {
		t.Fatalf("stored empty result was discarded: %+v %v", got, err)
	}
}

func ptrDate(t time.Time) *time.Time { return &t }
func TestUnitStorageDiagnosticBoundaryPreservesCancellationOnly(t *testing.T) {
	secret := errors.New("private SQL value")
	if got := importError(t.Context(), secret); !errors.Is(got, errImportStorage) || strings.Contains(got.Error(), "private") {
		t.Fatalf("diagnostic escaped: %v", got)
	}
	if importError(t.Context(), nil) != nil {
		t.Fatal("invented storage error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(importError(ctx, secret), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}
func TestUnitBankCategoryIdentityIsBoundedAndDeterministic(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"Кафе и рестораны", "kafe_i_restorany"}, {"Ї 😀", "category"}, {"A--B", "a_b"}, {"Щётка", "shchetka"}, {"1Taxi", "1taxi"}} {
		if got := categorySlug(tc.in); got != tc.want {
			t.Fatalf("category identity: %q %q", got, tc.want)
		}
	}
	if got := categorySlug(strings.Repeat("щ", 100)); len(got) != 230 || got != categorySlug(strings.Repeat("щ", 100)) {
		t.Fatal("unbounded/unstable identity")
	}
	if got := truncateText(strings.Repeat("𐐷", 65), 64); utf8.RuneCountInString(got) != 64 || !utf8.ValidString(got) {
		t.Fatal("category rune truncation")
	}
	if truncateText("short", 64) != "short" {
		t.Fatal("short category changed")
	}
}
