package postgres

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/domain"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

// SelectionKey resolves exactly one opaque identity, never a whole user's dictionary.
func (r *Operations) SelectionKey(ctx context.Context, user, kind, id string) (string, error) {
	var query string
	switch kind {
	case domain.ReferenceKindRegular:
		query = `SELECT description FROM regular_payment WHERE user_id=$1 AND id=$2::uuid`
	case domain.ReferenceKindMerchant:
		query = `SELECT key FROM merchant WHERE user_id=$1 AND id=$2::uuid`
	default:
		return "", operations.ErrSelection
	}
	var key string
	err := r.db.Writer(ctx).QueryRow(ctx, query, user, id).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", operations.ErrSelection
	}
	return key, importError(ctx, err)
}

// Recognition CTEs retain statistics, not unbounded arrays of operations. PostgreSQL
// may spill its grouping/sort to disk under work_mem. Regular arrays are built only
// after count <= the requested window length is established; financial gates stay in shared Go rules.
const operationStreamSQL = `WITH eligible AS MATERIALIZED (
 SELECT description,occurred_at,amount_minor,to_char(occurred_at AT TIME ZONE $4,'YYYY-MM') AS month
 FROM operation WHERE user_id=$1 AND (cardinality($5::text[])>0 OR $10) AND direction='expense' AND currency='RUB'
 AND transfer_pair_id IS NULL AND category_id NOT IN($8,$9)
 AND ((cardinality($5::text[])>0 AND to_char(occurred_at AT TIME ZONE $4,'YYYY-MM')=ANY($5::text[]))
 OR ($10 AND ($6::timestamptz IS NULL OR occurred_at >= $6) AND ($7::timestamptz IS NULL OR occurred_at < $7)))
), regular_counts AS MATERIALIZED (
 SELECT description,count(*) AS n FROM eligible WHERE month=ANY($5::text[]) AND description<>'' GROUP BY description
), regular_stats AS (
 SELECT e.description,array_agg(e.amount_minor ORDER BY e.occurred_at) AS amounts,
 array_agg(e.month ORDER BY e.occurred_at) AS months
 FROM eligible e JOIN regular_counts c ON c.description=e.description AND c.n<=cardinality($5::text[])
 WHERE e.month=ANY($5::text[]) GROUP BY e.description
), small_stats AS (
 SELECT description,month,count(*) AS n,sum(amount_minor)::text AS total FROM eligible
 WHERE $10 AND description<>'' AND ($6::timestamptz IS NULL OR occurred_at >= $6) AND ($7::timestamptz IS NULL OR occurred_at < $7)
 GROUP BY description,month
)
SELECT o.id::text,o.account_id,o.occurred_at,o.amount_minor,o.currency,o.direction,o.description,
 o.bank_category_id,COALESCE(o.manual_category_id,''),o.category_id,COALESCE(o.refund_target_category_id,''),o.is_split,COALESCE(o.transfer_pair_id::text,''),o.status,o.posted_at,o.mcc,
 p.id::text,p.account_id,p.occurred_at,p.description,p.bank_category_id,p.manual_category_id,p.status,p.posted_at,p.mcc,
 COALESCE(g.amounts,ARRAY[]::bigint[]),COALESCE(g.months,ARRAY[]::text[]),COALESCE(r.dismissed_at IS NOT NULL,false),COALESCE(s.n,0),COALESCE(s.total,'0')
FROM operation o
LEFT JOIN LATERAL(SELECT id,account_id,occurred_at,description,bank_category_id,manual_category_id,status,posted_at,mcc FROM operation
 WHERE user_id=$1 AND transfer_pair_id=o.transfer_pair_id AND direction='income' AND o.transfer_pair_id IS NOT NULL LIMIT 1)p ON true
LEFT JOIN regular_stats g ON g.description=o.description
LEFT JOIN regular_payment r ON r.user_id=$1 AND r.periodicity='monthly' AND r.description=o.description
LEFT JOIN small_stats s ON s.description=o.description AND s.month=to_char(o.occurred_at AT TIME ZONE $4,'YYYY-MM')
WHERE o.user_id=$1 AND (o.transfer_pair_id IS NULL OR o.direction='expense')
 AND ($2::timestamptz IS NULL OR o.occurred_at >=$2) AND ($3::timestamptz IS NULL OR o.occurred_at <$3)
ORDER BY o.occurred_at DESC,COALESCE(o.transfer_pair_id,o.id) DESC`

// Stream visits one public candidate at a time. The caller stops after 10001
// matches; even a rare text match in a large all-time ledger does not materialize it.
func (r *Operations) Stream(ctx context.Context, user string, o operations.StreamOptions, visit func(operations.Row) error) error {
	if visit == nil {
		return operations.ErrState
	}
	months := o.RegularMonths
	if months == nil {
		months = []string{}
	}
	rows, err := r.db.Writer(ctx).Query(ctx, operationStreamSQL, user, o.From, o.To, o.TimeZone, months, o.SmallFrom, o.SmallTo, accounting.OwnTransferCategory, accounting.TransfersCategory, o.Small)
	if err != nil {
		return importError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var op importing.Operation
		var detail operations.Detail
		var peerID, peerDescription, peerCategory, peerManual, peerStatus *string
		var peerAccount *int64
		var peerAt *time.Time
		var peerDetail operations.Detail
		var peer importing.Operation
		var row operations.Row
		var total string
		err = rows.Scan(&op.ID, &op.AccountID, &op.At, &op.AmountMinor, &op.Currency, &op.Direction, &op.Description, &op.BankCategory, &op.ManualCategory, &op.Category, &op.RefundTarget, &op.Split, &op.PairID, &detail.Status, &detail.PostedAt, &detail.MCC,
			&peerID, &peerAccount, &peerAt, &peerDescription, &peerCategory, &peerManual, &peerStatus, &peerDetail.PostedAt, &peerDetail.MCC,
			&row.RegularAmounts, &row.RegularMonths, &row.Dismissed, &row.SmallCount, &total)
		if err != nil {
			return importError(ctx, err)
		}
		row.SmallTotal, err = strconv.ParseInt(total, 10, 64)
		if err != nil {
			return operations.ErrState
		}
		row.Facts = []importing.Operation{op}
		row.Details = map[string]operations.Detail{op.ID: detail}
		if op.PairID != "" {
			if peerID == nil || peerAccount == nil || peerDescription == nil || peerCategory == nil || peerStatus == nil || peerAt == nil {
				return operations.ErrState
			}
			peer = importing.Operation{ID: *peerID, AccountID: *peerAccount, At: *peerAt, AmountMinor: op.AmountMinor, Currency: op.Currency, Direction: "income", Description: *peerDescription, BankCategory: *peerCategory, Category: accounting.OwnTransferCategory, PairID: op.PairID}
			if peerManual != nil {
				peer.ManualCategory = *peerManual
			}
			peerDetail.Status = *peerStatus
			row.Facts = append(row.Facts, peer)
			row.Details[peer.ID] = peerDetail
		}
		if err = visit(row); err != nil {
			return err
		}
	}
	return importError(ctx, rows.Err())
}
