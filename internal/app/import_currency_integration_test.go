package app_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Direct usecase integration checks production parser -> PostgreSQL -> a fresh
// assembly's reads, independently of the generated-client API currency tests.
func TestIntegrationImportPersistsNativeCurrencyRoundingAndUnknownRefusal(t *testing.T) {
	a, objects := importAssembly(t)
	rows := []struct {
		currency, amount, direction string
		minor                       int64
	}{
		{"JPY", "-1.5", "expense", 2},
		{"KWD", "-1.0005", "expense", 1001},
		{"CLF", "+1.23445", "income", 12345},
		{"RUB", "-0.015", "expense", 2},
	}
	var body strings.Builder
	body.WriteString(`<?OFX OFXHEADER="200" VERSION="202"?><OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>native0001</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST><DTSTART>20260901</DTSTART><DTEND>20260930</DTEND>`)
	for _, row := range rows {
		fmt.Fprintf(&body, `<STMTTRN><DTPOSTED>20260912120000[+3:MSK]</DTPOSTED><TRNAMT>%s</TRNAMT><CURSYM>%s</CURSYM><FITID>synthetic-%s</FITID><NAME>Native rounding fixture</NAME></STMTTRN>`, row.amount, row.currency, row.currency)
	}
	body.WriteString(`<STMTTRN><DTPOSTED>20260912120000[+3:MSK]</DTPOSTED><TRNAMT>-3.21</TRNAMT><CURSYM>ZZZ</CURSYM><FITID>synthetic-unknown</FITID><NAME>Unrecognized currency fixture</NAME></STMTTRN></BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`)
	source := []byte(body.String())
	accepted, err := a.Imports.Upload(t.Context(), apiUser, importing.Header{FileName: "native-currency.ofx", SizeBytes: int64(len(source))}, bytes.NewReader(source), []string{uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Imports.Process(t.Context(), apiUser, accepted.UploadID); err != nil {
		t.Fatal(err)
	}
	state := importSnapshot(t, a, apiUser)
	if len(state.Operations) != len(rows) || len(state.Unread) != 1 || len(state.Sources) != len(rows) {
		t.Fatal("native currencies were discarded or unknown currency became an operation", len(state.Operations), len(state.Unread), len(state.Sources))
	}
	expected := make(map[string]struct {
		minor     int64
		direction string
	}, len(rows))
	for _, row := range rows {
		expected[row.currency] = struct {
			minor     int64
			direction string
		}{row.minor, row.direction}
	}
	for _, operation := range state.Operations {
		want, ok := expected[operation.Currency]
		if !ok || operation.AmountMinor != want.minor || operation.Direction != want.direction {
			t.Fatalf("persisted native amount mismatch for currency %s", operation.Currency)
		}
		delete(expected, operation.Currency)
	}
	if len(expected) != 0 {
		t.Fatal("expected native currency missing after persistence")
	}
	if state.Unread[0].UploadID != accepted.UploadID || state.Unread[0].Reason != "bad_amount" || state.Unread[0].OccurredAt == nil {
		t.Fatal("unknown currency did not retain date-scoped bad_amount refusal")
	}
	// Build an independent service over the same real database and TLS provider;
	// the original in-memory Document cannot supply its history or loaded summary.
	fresh, err := app.New(t.Context(), a.Cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), app.Options{ObjectClient: objects.Server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fresh.Close)
	history, err := fresh.Imports.History(t.Context(), apiUser)
	if err != nil {
		t.Fatal(err)
	}
	if history.Loaded == nil || history.Loaded.Count != 4 || len(history.Uploads) != 1 {
		t.Fatal("native operations lost on fresh history read")
	}
	result := history.Uploads[0].Result
	if history.Uploads[0].ID != accepted.UploadID || history.Uploads[0].Status != "done" || result == nil || result.OperationCount != 4 || result.NewCount != 4 || result.UnreadCount != 1 {
		t.Fatal("persisted import result disagrees with native facts")
	}
	if result.Totals == nil || result.Totals.ExpenseMinor != 2 || result.Totals.IncomeMinor != 0 {
		t.Fatal("non-RUB units leaked into RUB totals")
	}
	unread, _, err := fresh.Imports.Unread(t.Context(), apiUser, importing.Scope{UploadID: accepted.UploadID})
	if err != nil || len(unread) != 1 || unread[0].Reason != "bad_amount" {
		t.Fatal("unknown currency refusal lost on fresh read", err)
	}
	cfg, err := fresh.Imports.Config(t.Context(), apiUser)
	if err != nil || cfg.Loaded == nil || cfg.Loaded.Count != 4 {
		t.Fatal("native operations missing from loaded summary", err)
	}
}
