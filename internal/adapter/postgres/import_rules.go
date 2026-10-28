package postgres

import (
	"context"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// The matched count includes unchanged rows: IS DISTINCT must not turn a valid
// complete history into a false missing-operation refusal. All work stays in the
// caller's transaction, so a genuinely missing row rolls the recomputation back.
const saveRulesSQL = `WITH input AS (
 SELECT at,id,category,NULLIF(manual,'' ) AS manual,NULLIF(refund,'' ) AS refund,split,NULLIF(pair,'' )::uuid AS pair
 FROM unnest($2::timestamptz[],$3::uuid[],$4::text[],$5::text[],$6::text[],$7::boolean[],$8::text[])
 AS i(at,id,category,manual,refund,split,pair)
), matched AS MATERIALIZED (
 SELECT o.user_id,o.occurred_at,o.id,i.category,i.manual,i.refund,i.split,i.pair
 FROM operation o JOIN input i ON o.occurred_at=i.at AND o.id=i.id
 WHERE o.user_id=$1
), updated AS (
 UPDATE operation o SET category_id=m.category,manual_category_id=m.manual,
 refund_target_category_id=m.refund,is_split=m.split,transfer_pair_id=m.pair
 FROM matched m WHERE o.user_id=m.user_id AND o.occurred_at=m.occurred_at AND o.id=m.id
 AND (o.category_id,o.manual_category_id,o.refund_target_category_id,o.is_split,o.transfer_pair_id)
 IS DISTINCT FROM (m.category,m.manual,m.refund,m.split,m.pair)
)
SELECT count(*) FROM matched`

// SaveRules persists the caller's complete recomputation without rewriting unchanged facts.
func (r *Imports) SaveRules(ctx context.Context, user string, operations []importing.Operation, now time.Time) error {
	if _, err := r.transaction(ctx); err != nil {
		return err
	}
	if len(operations) > 0 {
		times := make([]time.Time, len(operations))
		ids := make([]string, len(operations))
		categories := make([]string, len(operations))
		manual := make([]string, len(operations))
		refunds := make([]string, len(operations))
		split := make([]bool, len(operations))
		pairs := make([]string, len(operations))
		for i, op := range operations {
			times[i], ids[i], categories[i] = op.At, op.ID, op.Category
			manual[i], refunds[i], split[i], pairs[i] = op.ManualCategory, op.RefundTarget, op.Split, op.PairID
		}
		var matched int64
		if err := r.db.Writer(ctx).QueryRow(ctx, saveRulesSQL, user, times, ids, categories, manual, refunds, split, pairs).Scan(&matched); err != nil {
			return importError(ctx, err)
		}
		if matched != int64(len(operations)) {
			return errImportStorage
		}
	}
	// Tools read replicas and must never create identities while serving a read.
	// Keep UUIDs stable across imports, edits and calculator runs.
	if _, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO merchant(user_id,key)
 SELECT DISTINCT user_id,description FROM operation WHERE user_id=$1
 ON CONFLICT(user_id,key) DO NOTHING`, user); err != nil {
		return importError(ctx, err)
	}
	if _, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO regular_payment(user_id,periodicity,description)
 SELECT DISTINCT user_id,'monthly',description FROM operation WHERE user_id=$1 AND direction='expense' AND description<>''
 ON CONFLICT(user_id,periodicity,description) DO NOTHING`, user); err != nil {
		return importError(ctx, err)
	}
	return r.schedule(ctx, user, operations, now)
}
