// Package statements adapts verified RPC calls to the statement usecases.
package statements

import (
	"context"
	"errors"
	"io"
	"math"
	"time"

	commonv1 "github.com/fin-assist/contracts/gen/go/finassist/common/v1"
	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	keyadapter "github.com/fin-assist/bank-statement-data/internal/adapter/idempotency"
	"github.com/fin-assist/bank-statement-data/internal/adapter/statementwire"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

var errProjection = errors.New("invalid statement RPC projection")

// UseCases is the consumer boundary; identity and access admission run before it.
type UseCases interface {
	Config(context.Context, string) (importing.Config, error)
	History(context.Context, string) (importing.History, error)
	Unread(context.Context, string, importing.Scope) ([]importing.Unread, string, error)
	Upload(context.Context, string, importing.Header, io.Reader, []string) (importing.Accepted, error)
	Watch(context.Context, string, string, func(importing.Event) error) error
	Delete(context.Context, string, string) error
	SetTimezone(context.Context, string, string, int32) error
	Purge(context.Context, string) error
}

type handler struct {
	statementdatav1.UnimplementedStatementsServiceServer
	usersettingsv1.UnimplementedUserSettingsSyncServiceServer
	userdeletionv1.UnimplementedUserDeletionServiceServer
	svc           UseCases
	uploadTimeout time.Duration
	watchTimeout  time.Duration
}

// Register attaches the six import RPCs and the first writer's system lifecycle RPCs.
func Register(registrar grpc.ServiceRegistrar, svc UseCases) error {
	if registrar == nil || svc == nil {
		return errors.New("statement RPC dependencies required")
	}
	h := &handler{svc: svc, uploadTimeout: time.Minute, watchTimeout: 4 * time.Minute}
	statementdatav1.RegisterStatementsServiceServer(registrar, h)
	usersettingsv1.RegisterUserSettingsSyncServiceServer(registrar, h)
	userdeletionv1.RegisterUserDeletionServiceServer(registrar, h)
	return nil
}

func (h *handler) GetImportConfig(ctx context.Context, _ *statementdatav1.GetImportConfigRequest) (*statementdatav1.GetImportConfigResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.svc.Config(ctx, user)
	if err != nil {
		return nil, keyadapter.ContractError(err)
	}
	period, err := timeRange(importing.TimeRange{From: c.From, To: c.To})
	if err != nil {
		return nil, err
	}
	loaded, err := loadedSummary(c.Loaded)
	if err != nil {
		return nil, err
	}
	zone, err := profileZone(c.TimeZone)
	if err != nil {
		return nil, err
	}
	if c.DownloadURL == "" || c.LoginURL == "" {
		return nil, errProjection
	}
	return &statementdatav1.GetImportConfigResponse{DownloadUrl: c.DownloadURL, LoginUrl: c.LoginURL, MaxFileSizeBytes: importing.MaxFileBytes, AcceptedFormats: []string{"OFX"}, SuggestedPeriod: period, Loaded: loaded, TimeZone: zone}, nil
}

func (h *handler) ListUploads(ctx context.Context, _ *statementdatav1.ListUploadsRequest) (*statementdatav1.ListUploadsResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	history, err := h.svc.History(ctx, user)
	if err != nil {
		return nil, keyadapter.ContractError(err)
	}
	zone, err := profileZone(history.TimeZone)
	if err != nil {
		return nil, err
	}
	out := &statementdatav1.ListUploadsResponse{TimeZone: zone}
	for _, upload := range history.Uploads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := statementwire.ToUpload(upload)
		if err != nil {
			return nil, err
		}
		out.Uploads = append(out.Uploads, p)
	}
	if history.Loaded != nil {
		loaded, err := loadedSummary(history.Loaded)
		if err != nil {
			return nil, err
		}
		if len(history.Uploads) > math.MaxInt32 {
			return nil, errProjection
		}
		out.Summary = &statementdatav1.UploadsSummary{UploadCount: int32(len(history.Uploads)), OperationCount: loaded.OperationCount, FirstOperationAt: loaded.FirstOperationAt, LastOperationAt: loaded.LastOperationAt}
		for _, gap := range history.Gaps {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			p, err := timeRange(gap)
			if err != nil {
				return nil, err
			}
			out.Summary.Gaps = append(out.Summary.Gaps, p)
		}
	}
	return out, nil
}

func (h *handler) ListUnreadLines(ctx context.Context, r *statementdatav1.ListUnreadLinesRequest) (*statementdatav1.ListUnreadLinesResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	var scope importing.Scope
	switch v := r.GetScope().(type) {
	case *statementdatav1.ListUnreadLinesRequest_UploadId:
		scope.UploadID = v.UploadId
	case *statementdatav1.ListUnreadLinesRequest_Period:
		if v.Period == nil || v.Period.Start.CheckValid() != nil || v.Period.End.CheckValid() != nil {
			return nil, invalid("period", "INVALID_FORMAT")
		}
		scope.Period = &importing.TimeRange{From: v.Period.Start.AsTime(), To: v.Period.End.AsTime()}
	}
	rows, zone, err := h.svc.Unread(ctx, user, scope)
	if err != nil {
		return nil, keyadapter.ContractError(err)
	}
	zone, err = profileZone(zone)
	if err != nil {
		return nil, err
	}
	out := &statementdatav1.ListUnreadLinesResponse{TimeZone: zone}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := statementwire.ToUnread(row)
		if err != nil {
			return nil, err
		}
		out.Lines = append(out.Lines, p)
	}
	return out, nil
}

func (h *handler) DeleteUpload(ctx context.Context, r *statementdatav1.DeleteUploadRequest) (*statementdatav1.DeleteUploadResponse, error) {
	user, err := actor(ctx)
	if err != nil {
		return nil, err
	}
	if err = h.svc.Delete(ctx, user, r.GetUploadId()); err != nil {
		return nil, keyadapter.ContractError(err)
	}
	return &statementdatav1.DeleteUploadResponse{}, nil
}

func (h *handler) SetUserTimezone(ctx context.Context, r *usersettingsv1.SetUserTimezoneRequest) (*usersettingsv1.SetUserTimezoneResponse, error) {
	id, err := systemUser(r.GetUserId())
	if err != nil {
		return nil, err
	}
	if err = h.svc.SetTimezone(ctx, id, r.GetTimezone(), r.GetSettingsVersion()); err != nil {
		return nil, keyadapter.ContractError(err)
	}
	return &usersettingsv1.SetUserTimezoneResponse{}, nil
}

func (h *handler) PurgeUserData(ctx context.Context, r *userdeletionv1.PurgeUserDataRequest) (*userdeletionv1.PurgeUserDataResponse, error) {
	id, err := systemUser(r.GetUserId())
	if err != nil {
		return nil, err
	}
	if err = h.svc.Purge(ctx, id); err != nil {
		return nil, keyadapter.ContractError(err)
	}
	return &userdeletionv1.PurgeUserDataResponse{}, nil
}

func actor(ctx context.Context) (string, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return "", finerr.InvalidToken()
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil {
		return "", finerr.InvalidToken()
	}
	return parsed.String(), nil
}

func systemUser(raw string) (string, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return "", invalid("user_id", "INVALID_FORMAT")
	}
	return id.String(), nil
}

func invalid(field, reason string) error {
	return keyadapter.ContractError(&importing.InputError{Field: field, Reason: reason})
}

func profileZone(zone string) (string, error) {
	if zone == "" {
		zone = "Europe/Moscow"
	}
	if zone == "Local" {
		return "", errProjection
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "", errProjection
	}
	return zone, nil
}

func instant(at time.Time) (*timestamppb.Timestamp, error) {
	p := timestamppb.New(at)
	if p.CheckValid() != nil {
		return nil, errProjection
	}
	return p, nil
}
func timeRange(r importing.TimeRange) (*commonv1.TimeRange, error) {
	if !r.From.Before(r.To) {
		return nil, errProjection
	}
	from, err := instant(r.From)
	if err != nil {
		return nil, err
	}
	to, err := instant(r.To)
	if err != nil {
		return nil, err
	}
	return &commonv1.TimeRange{Start: from, End: to}, nil
}
func loadedSummary(l *importing.Loaded) (*statementdatav1.LoadedSummary, error) {
	if l == nil {
		return nil, nil
	}
	if l.Count <= 0 || l.First.After(l.Last) {
		return nil, errProjection
	}
	first, err := instant(l.First)
	if err != nil {
		return nil, err
	}
	last, err := instant(l.Last)
	if err != nil {
		return nil, err
	}
	return &statementdatav1.LoadedSummary{FirstOperationAt: first, LastOperationAt: last, OperationCount: l.Count}, nil
}
