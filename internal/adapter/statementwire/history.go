package statementwire

import (
	"strings"
	"time"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// ToUpload projects only visible PROCESSING/DONE history; the caller supplies
// inclusive period dates already normalized in the profile calendar.
func ToUpload(u importing.Upload) (*statementdatav1.Upload, error) {
	if u.OperationCount < 0 {
		return nil, errProjection
	}
	unread, err := count32(u.UnreadCount)
	if err != nil {
		return nil, err
	}
	resolved, err := count32(u.ResolvedUnreadCount)
	if err != nil {
		return nil, err
	}
	uploaded, err := timestamp(&u.UploadedAt, true)
	if err != nil {
		return nil, err
	}
	resolvedAt, err := timestamp(u.ResolvedByUploadedAt, resolved > 0)
	if err != nil || resolved == 0 && resolvedAt != nil {
		return nil, errProjection
	}
	p := &statementdatav1.Upload{UploadId: u.ID, FileName: u.FileName, UploadedAt: uploaded, OperationCount: u.OperationCount, UnreadCount: unread, ResolvedUnreadCount: resolved, ResolvedByUploadedAt: resolvedAt, SourceName: "Выписка Т-Банка"}
	switch u.Status {
	case "processing":
		p.Status = statementdatav1.UploadStatus_UPLOAD_STATUS_PROCESSING
	case "done":
		p.Status = statementdatav1.UploadStatus_UPLOAD_STATUS_DONE
		if u.Period != nil {
			if _, err := timestamp(&u.Period.From, true); err != nil {
				return nil, err
			}
			if _, err := timestamp(&u.Period.To, true); err != nil || u.Period.From.After(u.Period.To) {
				return nil, errProjection
			}
			from, to := u.Period.From.Format(time.DateOnly), u.Period.To.Format(time.DateOnly)
			p.PeriodFrom, p.PeriodTo = &from, &to
		}
	default:
		return nil, errProjection
	}
	return p, nil
}

// ToUnread returns only permitted public metadata, never account IDs, FITIDs or raw rows.
func ToUnread(u importing.Unread) (*statementdatav1.UnreadLine, error) {
	line, err := count32(u.Number)
	if err != nil || line == 0 {
		return nil, errProjection
	}
	date, err := timestamp(u.OccurredAt, false)
	if err != nil {
		return nil, err
	}
	var code statementdatav1.UnreadReason
	var name string
	switch u.Reason {
	case "bad_amount":
		code, name = statementdatav1.UnreadReason_UNREAD_REASON_BAD_AMOUNT, "не распознали сумму"
	case "bad_date":
		code, name = statementdatav1.UnreadReason_UNREAD_REASON_BAD_DATE, "дата не разбирается"
	case "no_date":
		code, name = statementdatav1.UnreadReason_UNREAD_REASON_NO_DATE, "даты нет"
	case "truncated":
		code, name = statementdatav1.UnreadReason_UNREAD_REASON_TRUNCATED, "строка обрезана"
	default:
		return nil, errProjection
	}
	return &statementdatav1.UnreadLine{UploadId: u.UploadID, FileName: u.FileName, LineNumber: line, Date: date, Reason: code, ReasonName: name}, nil
}

// Failure creates a terminal operation result, not a transport refusal or retry permission.
func Failure(code string) (*statementdatav1.ImportStreamError, error) {
	var legacy statementdatav1.ImportErrorCode
	var statusCode codes.Code
	switch code {
	case "cancelled":
		legacy, statusCode = statementdatav1.ImportErrorCode_IMPORT_ERROR_CODE_CANCELLED, codes.Canceled
	case "processing_failed":
		legacy, statusCode = statementdatav1.ImportErrorCode_IMPORT_ERROR_CODE_PROCESSING_FAILED, codes.FailedPrecondition
	default:
		return nil, errProjection
	}
	reason := strings.ToUpper(code)
	status, err := finerr.NewResultStatus(statusCode, reason, nil)
	if err != nil {
		return nil, errProjection
	}
	return &statementdatav1.ImportStreamError{Code: legacy, Message: reason, Status: status}, nil
}
