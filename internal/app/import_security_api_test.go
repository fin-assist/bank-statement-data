package app_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingspb "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIForeignAndMissingUploadsReachTenantSQLWithoutMutations(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "owner.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	if _, err := settingspb.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiOther, Timezone: "UTC", SettingsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	// An existing foreign user reaches the repository instead of an absent-profile guard.
	for _, id := range []string{accepted.UploadId, uuid.NewString()} {
		ctx := apitest.User(t.Context(), apiOther)
		if _, err := c.DeleteUpload(ctx, &pb.DeleteUploadRequest{UploadId: id}); apitest.Reason(err) != "NOT_FOUND" {
			t.Fatal("foreign/missing delete", err)
		}
		watch, err := c.WatchUpload(ctx, &pb.WatchUploadRequest{UploadId: id})
		if err == nil {
			_, err = watch.Recv()
		}
		if apitest.Reason(err) != "NOT_FOUND" {
			t.Fatal("foreign/missing watch", err)
		}
		if _, err := c.ListUnreadLines(ctx, &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: id}}); apitest.Reason(err) != "NOT_FOUND" {
			t.Fatal("foreign/missing unread", err)
		}
	}
	var state string
	var operations, sources, foreignOps int
	if err := db.QueryRow(t.Context(), `SELECT status FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&state); err != nil || state != "done" {
		t.Fatal("owner upload mutated", state, err)
	}
	if err := db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM operation WHERE user_id=$1),(SELECT count(*) FROM source_record WHERE user_id=$1),(SELECT count(*) FROM operation WHERE user_id=$2)`, apiUser, apiOther).Scan(&operations, &sources, &foreignOps); err != nil || operations != 1 || sources != 1 || foreignOps != 0 {
		t.Fatal("tenant negative wrote financial rows", operations, sources, foreignOps, err)
	}
}

func TestAPISystemLifecycleRejectsOtherCallersBeforeEffects(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "private.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	settings := settingspb.NewUserSettingsSyncServiceClient(env.Conn)
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	for _, ctx := range []context.Context{apitest.Caller(t.Context(), "bank-bff"), apitest.Caller(t.Context(), "assistant"), apitest.User(t.Context(), apiUser)} {
		if _, err := settings.SetUserTimezone(ctx, &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Asia/Tokyo", SettingsVersion: 999}); status.Code(err) != codes.PermissionDenied {
			t.Fatal("unauthorized settings", err)
		}
		if _, err := purge.PurgeUserData(ctx, &deletionpb.PurgeUserDataRequest{UserId: apiUser}); status.Code(err) != codes.PermissionDenied {
			t.Fatal("unauthorized purge", err)
		}
	}
	cfg, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if err != nil || cfg.TimeZone != "Europe/Moscow" || cfg.Loaded == nil || cfg.Loaded.OperationCount != 1 {
		t.Fatal("profile/rows/tombstone changed", cfg, err)
	}
	if body, ok := env.Objects.Bytes(apiUser + "/" + accepted.UploadId); !ok || string(body) != apiOFX {
		t.Fatal("unauthorized caller erased file")
	}
}

func TestAPIExistingWatchStopsWhenPurgeRetiresExpiredUpload(t *testing.T) {
	start := time.Now().UTC()
	var offset atomic.Int64
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return start.Add(time.Duration(offset.Load())) }})
	entered := make(chan struct{}, 1)
	resume := make(chan struct{})
	env.Objects.PauseReads(entered, resume)
	defer close(resume)
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "pending.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not read source")
	}
	ctx, cancel := context.WithTimeout(apitest.User(t.Context(), apiUser), 5*time.Second)
	defer cancel()
	watch, err := c.WatchUpload(ctx, &pb.WatchUploadRequest{UploadId: accepted.UploadId})
	if err != nil {
		t.Fatal(err)
	}
	if event, err := watch.Recv(); err != nil || event.GetProgress() == nil {
		t.Fatal("no initial progress", event, err)
	}
	env.Objects.SetErasureFailure(true)
	if _, err := deletionpb.NewUserDeletionServiceClient(env.Conn).PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); status.Code(err) != codes.Internal {
		t.Fatal("purge must retain intent on cloud outage", err)
	}
	offset.Store(int64(4 * time.Minute))
	for {
		_, err = watch.Recv()
		if err != nil {
			break
		}
	}
	if apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("existing watch failed to stop on retirement", err)
	}
}

func TestAPIImportPersistsCurrencySpecificMinorUnits(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	start := strings.Index(apiOFX, "<STMTTRN>")
	end := strings.Index(apiOFX, "</STMTTRN>") + len("</STMTTRN>")
	row := apiOFX[start:end]
	var rows string
	for _, currency := range []string{"JPY", "KWD", "CLF", "ZZZ"} {
		r := strings.Replace(row, "-123.45", "-1", 1)
		r = strings.Replace(r, "opaque-fixture-1", "opaque-"+currency, 1)
		rows += strings.Replace(r, "</STMTTRN>", "<CURRENCY><CURSYM>"+currency+"</CURSYM></CURRENCY></STMTTRN>", 1)
	}
	body := apiOFX[:start] + rows + apiOFX[end:]
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "native.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	result := apiComplete(t, c, accepted.UploadId)
	if result.NewCount != 3 || result.OperationCount != 3 || result.UnreadCount != 2 || result.Totals == nil || result.Totals.ExpenseMinor != 0 || result.Totals.IncomeMinor != 0 {
		t.Fatal("native counts or RUB totals", result)
	}
	db, err := pgx.Connect(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	for currency, want := range map[string]int64{"JPY": 1, "KWD": 1000, "CLF": 10000} {
		var got int64
		if err := db.QueryRow(t.Context(), `SELECT amount_minor FROM operation WHERE user_id=$1 AND currency=$2`, apiUser, currency).Scan(&got); err != nil || got != want {
			t.Fatal("native minor units corrupted", currency, got, want, err)
		}
	}
	unread, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: accepted.UploadId}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range unread.Lines {
		if line.Reason == pb.UnreadReason_UNREAD_REASON_BAD_AMOUNT {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown currency did not become bad_amount", unread)
	}
}
