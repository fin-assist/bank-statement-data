package app_test

import (
	"context"
	"testing"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIIncompleteUploadsFromOneUserLeaveOtherUserAdmissible(t *testing.T) {
	env := apitest.Start(t)
	client := pb.NewStatementsServiceClient(env.Conn)
	slowContext, cancelSlow := context.WithCancel(apitest.User(t.Context(), apiUser))
	defer cancelSlow()
	for range 4 {
		ctx := metadata.AppendToOutgoingContext(slowContext, "idempotency-key", uuid.NewString())
		stream, err := client.UploadStatement(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: "slow.ofx", SizeBytes: int64(len(apiOFX))}}}); err != nil {
			t.Fatal(err)
		}
		// A further RPC round trip lets each header reach the service while its
		// upload remains open: no file bytes or EOF have been sent.
		if _, err := client.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	accepted, err := apiUpload(t, client, apiOther, uuid.NewString(), "other.ofx", apiOFX)
	if err != nil {
		t.Fatal("one user's unfinished streams blocked another user's upload", err)
	}
	_ = apiComplete(t, client, accepted.UploadId, apiOther)
	cancelSlow()
	// Cancellation must release both the active receiver and its queued callers.
	accepted, err = apiUpload(t, client, apiUser, uuid.NewString(), "after-cancel.ofx", apiOFX)
	if err != nil {
		t.Fatal("cancelled receivers prevented fresh admission", err)
	}
	_ = apiComplete(t, client, accepted.UploadId)
}
