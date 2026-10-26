package app_test

import (
	"strings"
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPISamePhysicalLineKeepsUnreadOperationsAndIndependentClosures(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	secondBad := `<STMTTRN><DTPOSTED>20260913120000[+3:MSK]</DTPOSTED><TRNAMT>invalid</TRNAMT><FITID>same-line-bad-2</FITID></STMTTRN>`
	body := strings.ReplaceAll(strings.Replace(apiOFX, "</BANKTRANLIST>", secondBad+"</BANKTRANLIST>", 1), "\n", "")
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "one-line.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	result := apiComplete(t, c, accepted.UploadId)
	if result.NewCount != 1 || result.OperationCount != 1 || result.UnreadCount != 2 {
		t.Fatalf("valid operation or independent unread facts lost: %v", result)
	}
	check := func(want int) *pb.ListUnreadLinesResponse {
		t.Helper()
		rows, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: accepted.UploadId}})
		if err != nil || len(rows.Lines) != want {
			t.Fatalf("unread entries: %v %v", rows, err)
		}
		for _, row := range rows.Lines {
			if row.LineNumber != 1 {
				t.Fatalf("physical line was renumbered: %v", row)
			}
		}
		return rows
	}
	initial := check(2)
	reasons := map[pb.UnreadReason]bool{}
	for _, row := range initial.Lines {
		reasons[row.Reason] = true
	}
	if !reasons[pb.UnreadReason_UNREAD_REASON_NO_DATE] || !reasons[pb.UnreadReason_UNREAD_REASON_BAD_AMOUNT] {
		t.Fatal("distinct failure reasons were merged", initial)
	}
	// Only the first unread FITID is repaired. The second fact on that same
	// physical line must remain open, with its own reason and future resolution.
	fixed := strings.Replace(apiOFX, "<TRNAMT>-1</TRNAMT>", "<DTPOSTED>20260912120000[+3:MSK]</DTPOSTED><TRNAMT>-1</TRNAMT>", 1)
	repair, err := apiUpload(t, c, apiUser, uuid.NewString(), "repair-one.ofx", fixed)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, repair.UploadId)
	remaining := check(1)
	if remaining.Lines[0].Reason != pb.UnreadReason_UNREAD_REASON_BAD_AMOUNT {
		t.Fatal("repair closed the wrong fact on the shared physical line", remaining)
	}
	if _, err := c.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: repair.UploadId}); err != nil {
		t.Fatal(err)
	}
	check(2)
}
