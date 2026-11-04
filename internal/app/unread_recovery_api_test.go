package app_test

import (
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func assertAPIUnreadRecovery(t *testing.T, client pb.StatementsServiceClient, upload string, want int) {
	t.Helper()
	ctx := apitest.User(t.Context(), apiUser)
	lines, err := client.ListUnreadLines(ctx, &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: upload}})
	if err != nil || len(lines.Lines) != want {
		t.Fatal("unread evidence transition", lines, want, err)
	}
	history, err := client.ListUploads(ctx, &pb.ListUploadsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range history.Uploads {
		if row.UploadId == upload {
			if row.UnreadCount != int32(want) || row.ResolvedUnreadCount != int32(3-want) {
				t.Fatal("history lost recovered closure", row)
			}
			return
		}
	}
	t.Fatal("original upload missing")
}

func TestAPICoverageRecoveryRestoresPreviouslyClosedUnread(t *testing.T) {
	for _, tc := range []struct {
		name, period string
		wantOpen     int
	}{{"tags", apiProvenanceStart + apiProvenanceEnd, 0}, {"operations", "", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			client := pb.NewStatementsServiceClient(env.Conn)
			upload := func(name, period, rows string) string {
				t.Helper()
				accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), name, apiProvenanceBody(period, rows))
				if err != nil {
					t.Fatal(err)
				}
				_ = apiComplete(t, client, accepted.UploadId)
				return accepted.UploadId
			}
			old := upload("broken.ofx", apiProvenanceStart+apiProvenanceEnd, apiProvenanceBroken)
			closer := upload("old-closer.ofx", tc.period, apiProvenanceRepaired)
			// Arrange retained rows from an older writer, including its legacy
			// closure decision. The exact original object remains in real storage.
			if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE upload SET statement_range_source=NULL,statement_from_instant=NULL,statement_to_instant=NULL WHERE user_id=$1 AND id=$2`, apiUser, closer); err != nil {
				t.Fatal(err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE unread_line SET resolved_by_upload_id=$3,resolution='read' WHERE user_id=$1 AND upload_id=$2 AND external_id IS NULL`, apiUser, old, closer); err != nil {
				t.Fatal(err)
			}
			assertAPIUnreadRecovery(t, client, old, 0)
			_ = upload("new-source.ofx", "", "")
			assertAPIUnreadRecovery(t, client, old, 2)
			if err := env.App.Recalculations.Recalculate(t.Context(), apiUser); err != nil {
				t.Fatal(err)
			}
			assertAPIUnreadRecovery(t, client, old, tc.wantOpen)
			var source string
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT statement_range_source FROM upload WHERE user_id=$1 AND id=$2`, apiUser, closer).Scan(&source); err != nil || source != tc.name {
				t.Fatal("original evidence was not restored", source, err)
			}
		})
	}
}
