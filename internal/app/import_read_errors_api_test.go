package app_test

import (
	"context"
	"crypto/sha256"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	commonpb "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	deletionpb "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	settingspb "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func readAPIInternal(t *testing.T, err error) {
	t.Helper()
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private") || len(status.Convert(err).Details()) != 0 {
		t.Fatal("unsafe malformed-storage response", err)
	}
}

func TestAPIImportConfigUsesActualLoadedRangeAndProfileCalendar(t *testing.T) {
	now := time.Date(2026, 10, 25, 23, 30, 0, 0, time.UTC)
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return now }})
	c := pb.NewStatementsServiceClient(env.Conn)
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 0 || history.Summary != nil {
		t.Fatal("empty history invented summary", history, err)
	}
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "exact.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	_, err = settingspb.NewUserSettingsSyncServiceClient(env.Conn).SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Asia/Tokyo", SettingsVersion: 7})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	zone, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 9, 12, 0, 0, 0, 0, zone)
	to := time.Date(2026, 10, 27, 0, 0, 0, 0, zone)
	first := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
	if cfg.TimeZone != "Asia/Tokyo" || cfg.Loaded == nil || cfg.Loaded.OperationCount != 1 || !cfg.Loaded.FirstOperationAt.AsTime().Equal(first) || !cfg.Loaded.LastOperationAt.AsTime().Equal(first) || !cfg.SuggestedPeriod.Start.AsTime().Equal(from) || !cfg.SuggestedPeriod.End.AsTime().Equal(to) {
		t.Fatal("loaded/calendar mismatch", cfg)
	}
	link, err := url.Parse(cfg.DownloadUrl)
	if err != nil {
		t.Fatal(err)
	}
	if link.Host != "www.tbank-online.com" || link.Path != "/mybank/api/operations/timeline/public/legacy/v1/export_operations" || link.Query().Get("start") != strconv.FormatInt(from.UnixMilli(), 10) || link.Query().Get("end") != strconv.FormatInt(to.UnixMilli(), 10) || link.Query().Get("format") != "ofx" {
		t.Fatal("browser session export parameters changed")
	}
	history, err = c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || history.TimeZone != "Asia/Tokyo" || len(history.Uploads) != 1 || history.Uploads[0].Status != pb.UploadStatus_UPLOAD_STATUS_DONE || history.Summary == nil || history.Summary.OperationCount != 1 {
		t.Fatal(history, err)
	}
	foreign, err := c.ListUploads(apitest.User(t.Context(), apiOther), &pb.ListUploadsRequest{})
	if err != nil || len(foreign.Uploads) != 0 || foreign.Summary != nil {
		t.Fatal("foreign history leaked", foreign, err)
	}
	for _, actor := range []string{apiUser, apiOther} {
		_, err = c.ListUnreadLines(apitest.User(t.Context(), actor), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: uuid.NewString()}})
		if apitest.Reason(err) != "NOT_FOUND" {
			t.Fatal("missing unread lookup", err)
		}
	}
	_, err = c.ListUnreadLines(apitest.User(t.Context(), apiOther), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: accepted.UploadId}})
	if apitest.Reason(err) != "NOT_FOUND" {
		t.Fatal("foreign unread accessible", err)
	}
}

func TestAPIUnreadScopeAndSystemValidationCannotResurrectPurgedUser(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, req := range []*pb.ListUnreadLinesRequest{{}, {Scope: &pb.ListUnreadLinesRequest_Period{Period: &commonpb.TimeRange{Start: timestamppb.New(at), End: timestamppb.New(at.Add(-time.Hour))}}}, {Scope: &pb.ListUnreadLinesRequest_Period{Period: &commonpb.TimeRange{Start: timestamppb.New(at)}}}} {
		_, err := c.ListUnreadLines(apitest.User(t.Context(), apiUser), req)
		if apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatal("invalid scope accepted", err)
		}
	}
	settings := settingspb.NewUserSettingsSyncServiceClient(env.Conn)
	purge := deletionpb.NewUserDeletionServiceClient(env.Conn)
	for _, zone := range []string{"Local", "../UTC", "/UTC", "no/such-zone"} {
		_, err := settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: zone, SettingsVersion: 10})
		if apitest.Reason(err) != "VALIDATION_ERROR" || strings.Contains(err.Error(), zone) {
			t.Fatal("invalid timezone accepted or reflected", err)
		}
	}
	for _, id := range []string{"invalid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: id, Timezone: "UTC", SettingsVersion: 1}); apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatal("invalid system actor accepted", err)
		}
		if _, err := purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: id}); apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatal("invalid purge actor accepted", err)
		}
	}
	if _, err := settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "UTC", SettingsVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := purge.PurgeUserData(apitest.Caller(t.Context(), "user"), &deletionpb.PurgeUserDataRequest{UserId: apiUser}); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.SetUserTimezone(apitest.Caller(t.Context(), "user"), &settingspb.SetUserTimezoneRequest{UserId: apiUser, Timezone: "Asia/Tokyo", SettingsVersion: 99}); err != nil {
		t.Fatal("late system event should acknowledge retired actor", err)
	}
	var count int
	if err := env.App.DB.Primary().QueryRow(t.Context(), "SELECT count(*) FROM user_state WHERE user_id=$1", apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatal("late settings resurrected personal state", count, err)
	}
	if _, err := c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("purged actor admitted", err)
	}
}

func TestAPIInvalidStoredUploadMetadataNeverLeaksOrChangesUnrelatedUser(t *testing.T) {
	env := apitest.Start(t)
	c := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, c, apiUser, uuid.NewString(), "private.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	_ = apiComplete(t, c, accepted.UploadId)
	db := env.App.DB.Primary()
	var size int32
	var digest []byte
	var percent int16
	var storedStatus string
	var uploaded time.Time
	if err := db.QueryRow(t.Context(), `SELECT size_bytes,content_sha256,percent,status,uploaded_at FROM upload WHERE user_id=$1 AND id=$2`, apiUser, accepted.UploadId).Scan(&size, &digest, &percent, &storedStatus, &uploaded); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query   string
		bad, original any
	}{
		{"size", `UPDATE upload SET size_bytes=$3 WHERE user_id=$1 AND id=$2`, int32(0), size},
		{"digest", `UPDATE upload SET content_sha256=$3 WHERE user_id=$1 AND id=$2`, []byte{1}, digest},
		{"progress", `UPDATE upload SET percent=$3 WHERE user_id=$1 AND id=$2`, int16(101), percent},
		{"status", `UPDATE upload SET status=$3 WHERE user_id=$1 AND id=$2`, "private_future", storedStatus},
		{"timestamp", `UPDATE upload SET uploaded_at=$3 WHERE user_id=$1 AND id=$2`, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), uploaded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(t.Context(), tc.query, apiUser, accepted.UploadId, tc.bad); err != nil {
				t.Fatal(err)
			}
			reply, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
			readAPIInternal(t, err)
			if reply != nil {
				t.Fatal("malformed row partially returned")
			}
			foreign, err := c.ListUploads(apitest.User(t.Context(), apiOther), &pb.ListUploadsRequest{})
			if err != nil || len(foreign.Uploads) != 0 {
				t.Fatal("corruption escaped tenant boundary", err)
			}
			if _, err := db.Exec(t.Context(), tc.query, apiUser, accepted.UploadId, tc.original); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := db.Exec(t.Context(), `UPDATE user_state SET timezone='private-invalid-zone' WHERE user_id=$1`, apiUser); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetImportConfig(apitest.User(t.Context(), apiUser), &pb.GetImportConfigRequest{})
	readAPIInternal(t, err)
	if _, err := db.Exec(t.Context(), `UPDATE user_state SET timezone='Europe/Moscow' WHERE user_id=$1`, apiUser); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := db.QueryRow(t.Context(), `SELECT reason FROM unread_line WHERE user_id=$1 AND upload_id=$2`, apiUser, accepted.UploadId).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE unread_line SET reason='private-unknown' WHERE user_id=$1 AND upload_id=$2`, apiUser, accepted.UploadId); err != nil {
		t.Fatal(err)
	}
	_, err = c.ListUnreadLines(apitest.User(t.Context(), apiUser), &pb.ListUnreadLinesRequest{Scope: &pb.ListUnreadLinesRequest_UploadId{UploadId: accepted.UploadId}})
	readAPIInternal(t, err)
	if _, err := db.Exec(t.Context(), `UPDATE unread_line SET reason=$3 WHERE user_id=$1 AND upload_id=$2`, apiUser, accepted.UploadId, reason); err != nil {
		t.Fatal(err)
	}
	history, err := c.ListUploads(apitest.User(t.Context(), apiUser), &pb.ListUploadsRequest{})
	if err != nil || len(history.Uploads) != 1 {
		t.Fatal("restored row was mutated by refused reads", err)
	}
	expected := sha256.Sum256([]byte(apiOFX))
	if string(digest) != string(expected[:]) {
		t.Fatal("source digest fixture changed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.GetImportConfig(apitest.User(ctx, apiUser), &pb.GetImportConfigRequest{})
	if status.Code(err) != codes.Canceled {
		t.Fatal("cancelled read retried or reclassified", err)
	}
}
