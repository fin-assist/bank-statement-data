package app_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIDefaultEncryptionRefusesImportsAndPreservesPurgeIntents(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	// This is a real RPC over the whole service assembly. A changed default must
	// refuse original admission and its attempted cleanup, without storing bytes.
	env.Objects.SetDefaultEncryptionFailure(true)
	_, err = apiUpload(t, c, apiOther, uuid.NewString(), "denied.ofx", apiOFX)
	if status.Code(err) != codes.Internal || apitest.Reason(err) != "" || strings.Contains(err.Error(), "unexpected-key") {
		t.Fatal("unsafe import/diagnostics", err)
	}
	var rejectedID string
	if err = db.QueryRow(t.Context(), "SELECT id FROM upload WHERE user_id=$1", apiOther).Scan(&rejectedID); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Objects.Bytes(apiOther + "/" + rejectedID); ok {
		t.Fatal("config refusal stored file or fence")
	}
	env.Objects.SetDefaultEncryptionFailure(false)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "verified.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	env.Objects.SetDefaultEncryptionFailure(true)
	_, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser})
	if status.Code(err) != codes.Internal {
		t.Fatal("wrong default acknowledged erasure", err)
	}
	if body, ok := env.Objects.Bytes(apiUser + "/" + accepted.UploadId); !ok || len(body) == 0 {
		t.Fatal("failed preflight wrote fence")
	}
	// Provider commits the fence but gives no encryption proof. The service must
	// retain durable intents, then repeat the fence after evidence is restored.
	env.Objects.SetDefaultEncryptionFailure(false)
	env.Objects.SetResponseEncryptionFailure(true)
	_, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser})
	if status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
		t.Fatal("unverified fence acknowledged", err)
	}
	var pending int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM upload WHERE user_id=$1 AND file_deleted_at IS NULL", apiUser).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("lost uncertain erasure intent", pending, err)
	}
	if body, ok := env.Objects.Bytes(apiUser + "/" + accepted.UploadId); !ok || len(body) != 0 {
		t.Fatal("fixture did not model committed uncertain fence")
	}
	env.Objects.SetResponseEncryptionFailure(false)
	if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("purged user resurrected", err)
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM upload WHERE user_id=$1", apiUser).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("confirmed purge retained personal rows", pending, err)
	}
}

func TestAPIUnverifiedSourceEncryptionCannotPublishFinancialFacts(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	entered, resume := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	defer release()
	env.Objects.PauseReads(entered, resume)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "read-proof.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("worker never read admitted source")
	}
	env.Objects.SetResponseEncryptionFailure(true)
	release()
	env.Objects.PauseReads(nil, nil)
	watch, err := c.WatchUpload(apitest.User(ctx, apiUser), &pb.WatchUploadRequest{UploadId: accepted.UploadId})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := watch.Recv()
		if err != nil {
			if status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
				t.Fatal("uncertain cleanup exposed diagnostics", err)
			}
			break
		}
		if event.GetComplete() != nil || event.GetError() != nil {
			t.Fatal("unverified source/cleanup was confirmed", event)
		}
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var operations, pending int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM operation WHERE user_id=$1", apiUser).Scan(&operations); err != nil || operations != 0 {
		t.Fatal("unverified source facts retained", operations, err)
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM upload WHERE user_id=$1 AND file_deleted_at IS NULL", apiUser).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("unconfirmed cleanup discarded intent", pending, err)
	}
	env.Objects.SetResponseEncryptionFailure(false)
	watch, err = c.WatchUpload(apitest.User(ctx, apiUser), &pb.WatchUploadRequest{UploadId: accepted.UploadId})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := watch.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.GetComplete() != nil {
			t.Fatal("unverified source became a successful import")
		}
		if failure := event.GetError(); failure != nil {
			decoded, ok := finerr.DecodeResultStatus(failure.Status)
			if !ok || decoded.Reason != "PROCESSING_FAILED" || failure.Code != pb.ImportErrorCode_IMPORT_ERROR_CODE_PROCESSING_FAILED {
				t.Fatal("wrong processing refusal", failure)
			}
			break
		}
	}
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal("cleanup did not recover", err)
	}
}
