package app_test

import (
	"strings"
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

const apiProvenanceStart = "<DTSTART>20260901000000[+3:MSK]</DTSTART>"
const apiProvenanceEnd = "<DTEND>20260930235959[+3:MSK]</DTEND>"
const apiProvenanceFixture = `<?xml version="1.0" encoding="utf-8"?>
<?OFX OFXHEADER="200" VERSION="202"?>
<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>unread-coverage</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST>{{period}}
<STMTTRN><DTPOSTED>20260901120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT><FITID>first</FITID></STMTTRN>
<STMTTRN><DTPOSTED>20260930120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT><FITID>last</FITID></STMTTRN>
{{rows}}</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`
const apiProvenanceBroken = `<STMTTRN><DTPOSTED>20260915120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT></STMTTRN>
<STMTTRN><TRNAMT>-1</TRNAMT></STMTTRN>
<STMTTRN><DTPOSTED>20260910120000[+3:MSK]</DTPOSTED><TRNAMT>invalid</TRNAMT><FITID>repair</FITID></STMTTRN>`
const apiProvenanceRepaired = `<STMTTRN><DTPOSTED>20260910120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT><FITID>repair</FITID></STMTTRN>`

func apiProvenanceBody(period, rows string) string {
	return strings.NewReplacer("{{period}}", period, "{{rows}}", rows).Replace(apiProvenanceFixture)
}

func TestAPIUnreadClosureRequiresExplicitSourceRange(t *testing.T) {
	for _, tc := range []struct {
		name, period string
		wantOpen     int
	}{
		{"missing", "", 2},
		{"start_only", apiProvenanceStart, 2},
		{"end_only", apiProvenanceEnd, 2},
		{"both", apiProvenanceStart + apiProvenanceEnd, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			client := pb.NewStatementsServiceClient(env.Conn)
			old, err := apiUpload(t, client, apiUser, uuid.NewString(), "broken.ofx", apiProvenanceBody(apiProvenanceStart+apiProvenanceEnd, apiProvenanceBroken))
			if err != nil {
				t.Fatal(err)
			}
			if result := apiComplete(t, client, old.UploadId); result.UnreadCount != 3 {
				t.Fatal("fixture did not retain all independent unread facts", result)
			}
			repair, err := apiUpload(t, client, apiUser, uuid.NewString(), "repair.ofx", apiProvenanceBody(tc.period, apiProvenanceRepaired))
			if err != nil {
				t.Fatal(err)
			}
			_ = apiComplete(t, client, repair.UploadId)
			check := func(open int) {
				t.Helper()
				ctx := apitest.User(t.Context(), apiUser)
				unread, err := client.ListUnreadLines(ctx, &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: old.UploadId}})
				if err != nil || len(unread.Lines) != open {
					t.Fatal("source interval falsely repaired missing rows", unread, err)
				}
				history, err := client.ListUploads(ctx, &pb.ListUploadsRequest{})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, upload := range history.Uploads {
					if upload.UploadId == old.UploadId {
						found = true
						if upload.UnreadCount != int32(open) || upload.ResolvedUnreadCount != int32(3-open) {
							t.Fatal("history counters disagree with source evidence", upload)
						}
					}
					if upload.UploadId == repair.UploadId && (upload.PeriodFrom == nil || upload.PeriodTo == nil) {
						t.Fatal("fallback period disappeared from history", upload)
					}
				}
				if !found {
					t.Fatal("original upload disappeared")
				}
			}
			check(tc.wantOpen)
			if _, err := client.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: repair.UploadId}); err != nil {
				t.Fatal(err)
			}
			check(3)
		})
	}
}
