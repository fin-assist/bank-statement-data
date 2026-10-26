package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/ofx"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestIntegrationParserSameLineFactsKeepIndependentPostgresIdentity(t *testing.T) {
	f := importRepository(t)
	user, upload, repair := uuid.NewString(), uuid.NewString(), uuid.NewString()
	body := `<?OFX OFXHEADER="200" VERSION="202"?><OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>synthetic</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST><DTSTART>20260901</DTSTART><DTEND>20260930</DTEND>` +
		`<STMTTRN><DTPOSTED>20260912</DTPOSTED><TRNAMT>1</TRNAMT><FITID>valid</FITID></STMTTRN>` +
		`<STMTTRN><TRNAMT>1</TRNAMT><FITID>undated</FITID></STMTTRN>` +
		`<STMTTRN><DTPOSTED>20260912</DTPOSTED><TRNAMT>invalid</TRNAMT><FITID>bad-amount</FITID></STMTTRN>` +
		"\n" + `<STMTTRN><TRNAMT>1</TRNAMT></STMTTRN></BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`
	parser := ofx.Parser{}
	if err := parser.Validate(strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	doc, err := parser.Parse(t.Context(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if err := f.admitted(ctx, user, upload); err != nil {
			return err
		}
		if err := f.admitted(ctx, user, repair); err != nil {
			return err
		}
		_, err := f.repo.Stage(ctx, user, upload, doc, f.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state, err := f.repo.Snapshot(t.Context(), user)
	if err != nil || len(state.Operations) != 1 || len(state.Unread) != 3 {
		t.Fatal("valid operation or unread metadata rolled back", state, err)
	}
	for i, want := range [][2]int{{1, 1}, {1, 2}, {2, 1}} {
		if state.Unread[i].Number != want[0] || state.Unread[i].Occurrence != want[1] {
			t.Fatal("physical number/occurrence changed", state.Unread)
		}
	}
	if state.Unread[0].FITID != "undated" || state.Unread[0].Reason != "no_date" || state.Unread[1].FITID != "bad-amount" || state.Unread[1].Reason != "bad_amount" || state.Unread[1].OccurredAt == nil {
		t.Fatal("distinct source metadata was merged", state.Unread)
	}
	target := state.Unread[0]
	target.ResolvedByUpload, target.Resolution = repair, "read"
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { return f.repo.SaveUnread(ctx, user, []importing.Unread{target}) }); err != nil {
		t.Fatal(err)
	}
	state, err = f.repo.Snapshot(t.Context(), user)
	if err != nil || state.Unread[0].ResolvedByUpload != repair || state.Unread[1].ResolvedByUpload != "" || state.Unread[2].ResolvedByUpload != "" {
		t.Fatal("one projection overwrote another occurrence", state.Unread, err)
	}
}
