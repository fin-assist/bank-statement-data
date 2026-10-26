package app_test

import (
	"context"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIWatchRechecksFreshPrimaryBeforeEvents(t *testing.T) {
	for _, mode := range []string{"partial-purge", "complete-purge", "primary-error"} {
		t.Run(mode, func(t *testing.T) {
			env := apitest.Start(t)
			entered := make(chan struct{}, 1)
			resume := make(chan struct{})
			env.Objects.PauseReads(entered, resume)
			defer close(resume)
			client := pb.NewStatementsServiceClient(env.Conn)
			accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "unexpired.ofx", apiOFX)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not enter source read")
			}
			ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 5*time.Second)
			defer cancel()
			watch, err := client.WatchUpload(ctx, &pb.WatchUploadRequest{UploadId: accepted.UploadId})
			if err != nil {
				t.Fatal(err)
			}
			if event, err := watch.Recv(); err != nil || event.GetProgress() == nil {
				t.Fatal("missing initial progress", event, err)
			}
			if mode == "primary-error" {
				if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE tombstone_users RENAME TO private_unavailable_tombstones`); err != nil {
					t.Fatal(err)
				}
			} else {
				cloudFailure := mode == "partial-purge"
				env.Objects.SetErasureFailure(cloudFailure)
				_, err = deletionpb.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser})
				if cloudFailure && status.Code(err) != codes.Internal || !cloudFailure && err != nil {
					t.Fatal("purge outcome", err)
				}
			}
			// No clock shift: the upload remains far below its three-minute timeout.
			for {
				event, recvErr := watch.Recv()
				if recvErr != nil {
					err = recvErr
					break
				}
				if event.GetComplete() != nil || event.GetError() != nil {
					t.Fatal("business terminal leaked after access revocation", event)
				}
			}
			if mode == "primary-error" {
				readAPIInternal(t, err)
				return
			}
			if apitest.Reason(err) != "TOKEN_REVOKED" {
				t.Fatal("open stream did not observe fresh primary deletion", err)
			}
		})
	}
}
