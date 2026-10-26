package app_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingspb "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

const apiUser = "73ef1f95-14d5-453c-80ec-d0e2ac8c854d"
const apiOther = "ace128e4-2757-42d9-81cd-7921fdb010f4"
const apiOFX = `<?xml version="1.0" encoding="utf-8"?>
<?OFX OFXHEADER="200" VERSION="202" SECURITY="NONE" OLDFILEUID="NONE" NEWFILEUID="NONE"?>
<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>40817810000000001111</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST><DTSTART>20260901000000[+3:MSK]</DTSTART><DTEND>20260930235959[+3:MSK]</DTEND>
<STMTTRN><TRNTYPE>DEBIT</TRNTYPE><DTPOSTED>20260912120000[+3:MSK]</DTPOSTED><TRNAMT>-123.45</TRNAMT><FITID>opaque-fixture-1</FITID><NAME>Fixture purchase</NAME><MEMO>Продукты</MEMO></STMTTRN>
<STMTTRN><TRNTYPE>DEBIT</TRNTYPE><TRNAMT>-1</TRNAMT><FITID>opaque-unread-1</FITID></STMTTRN>
</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`

func apiUpload(t *testing.T, c pb.StatementsServiceClient, user, key, name, body string) (*pb.UploadStatementResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(apitest.User(t.Context(), user), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "idempotency-key", key)
	stream, err := c.UploadStatement(ctx)
	if err != nil {
		return nil, err
	}
	if err = stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: name, SizeBytes: int64(len(body))}}}); err != nil {
		return nil, err
	}
	if err = stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Chunk{Chunk: []byte(body)}}); err != nil {
		return nil, err
	}
	return stream.CloseAndRecv()
}
func apiComplete(t *testing.T, c pb.StatementsServiceClient, id string) *pb.ImportResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 15*time.Second)
	defer cancel()
	stream, err := c.WatchUpload(ctx, &pb.WatchUploadRequest{UploadId: id})
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		if event.TimeZone == "" {
			t.Fatal("missing timezone")
		}
		if event.GetError() != nil {
			t.Fatalf("terminal error: %v", event.GetError())
		}
		if result := event.GetComplete(); result != nil {
			if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal must close OK: %v", err)
			}
			return result
		}
	}
}

func TestAPIImportRealDurabilityDedupDeletionAndPurge(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	config, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if err != nil || config.TimeZone != "Europe/Moscow" || config.LoginUrl != "https://www.tbank.ru/login/" || config.Loaded != nil {
		t.Fatalf("config: %v %v", config, err)
	}
	settings := settingspb.NewUserSettingsSyncServiceClient(env.Conn)
	_, err = settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Europe/Berlin", SettingsVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Europe/Moscow", SettingsVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	name := strings.Repeat("я", 260) + "/raw.ofx"
	first, err := apiUpload(t, c, apiUser, key, name, apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	if first.FileName != name {
		t.Fatal("raw filename changed")
	}
	result := apiComplete(t, c, first.UploadId)
	if result.NewCount != 1 || result.OperationCount != 1 || result.UnreadCount != 1 || !result.IsFirstImport || result.Totals == nil {
		t.Fatalf("real result: %v", result)
	}
	replay, err := apiUpload(t, c, apiUser, key, name, apiOFX)
	if err != nil || replay.UploadId != first.UploadId {
		t.Fatalf("replay: %v %v", replay, err)
	}
	_, err = apiUpload(t, c, apiUser, key, name+"!", apiOFX)
	if apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("body identity: %v", err)
	}
	second, err := apiUpload(t, c, apiUser, uuid.NewString(), "", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := apiComplete(t, c, second.UploadId)
	if duplicate.NewCount != 0 || duplicate.DuplicateCount != 1 || duplicate.Totals != nil || duplicate.IsFirstImport {
		t.Fatalf("duplicate: %v", duplicate)
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 2 || history.TimeZone != "Europe/Berlin" {
		t.Fatalf("history: %v %v", history, err)
	}
	unread, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: second.UploadId}})
	if err != nil || len(unread.Lines) == 0 {
		t.Fatalf("unread: %v %v", unread, err)
	}
	_, err = c.DeleteUpload(apitest.User(t.Context(), apiOther), &pb.DeleteUploadRequest{UploadId: first.UploadId})
	if apitest.Reason(err) != "NOT_FOUND" {
		t.Fatalf("foreign delete: %v", err)
	}
	for _, id := range []string{first.UploadId, first.UploadId, second.UploadId} {
		if _, err = c.DeleteUpload(apitest.User(t.Context(), apiUser), &pb.DeleteUploadRequest{UploadId: id}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var count int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM operation WHERE user_id=$1", apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatalf("last source removal: %d %v", count, err)
	}
	replay, err = apiUpload(t, c, apiUser, key, name, apiOFX)
	if err != nil || replay.UploadId != first.UploadId {
		t.Fatalf("deleted replay: %v %v", replay, err)
	}
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	for range 2 {
		if _, err = purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted read: %v", err)
	}
	var rows, keys int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM user_state WHERE user_id=$1", apiUser).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys WHERE scope=$1", apiUser).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || keys != 0 {
		t.Fatalf("purge personal rows: %d %d", rows, keys)
	}
}

func TestAPIImport422IsCachedBeforeAnyObjectAdmission(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	key := uuid.NewString()
	for range 2 {
		_, err := apiUpload(t, c, apiUser, key, "bad.csv", "date,amount\n2026-01-01,1\n")
		if apitest.Reason(err) != "CSV_NOT_ACCEPTED" {
			t.Fatalf("CSV: %v", err)
		}
		if r, ok := finerr.ToREST(err); !ok || r.RequestRetryable != nil {
			t.Fatalf("invented permission: %#v", r)
		}
	}
	_, err := apiUpload(t, c, apiUser, key, "bad.csv", apiOFX)
	if apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed body reused rejected key: %v", err)
	}
}

func TestAPIImportOversizeHintPrecedesStreamKeyValidation(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	for _, keys := range [][]string{nil, {"not-a-key"}, {uuid.NewString(), uuid.NewString()}} {
		ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 5*time.Second)
		for _, key := range keys {
			ctx = metadata.AppendToOutgoingContext(ctx, "idempotency-key", key)
		}
		stream, err := c.UploadStatement(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_ = stream.Send(&pb.UploadStatementRequest{Part: &pb.UploadStatementRequest_Header{Header: &pb.UploadHeader{FileName: "oversize", SizeBytes: 10<<20 + 1}}})
		_, err = stream.CloseAndRecv()
		cancel()
		if apitest.Reason(err) != "FILE_TOO_LARGE" {
			t.Fatalf("prekey size for %d keys: %v", len(keys), err)
		}
	}
	_, err := apiUpload(t, c, apiUser, "not-a-key", "small", apiOFX)
	if apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatalf("small file key refusal after EOF: %v", err)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	var keys, uploads int
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_keys").Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(t.Context(), "SELECT count(*) FROM upload").Scan(&uploads); err != nil {
		t.Fatal(err)
	}
	if keys != 0 || uploads != 0 {
		t.Fatalf("prekey refusal stored state: %d/%d", keys, uploads)
	}
}
