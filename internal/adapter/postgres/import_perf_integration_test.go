package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

func TestIntegrationBulkRulesPreserveUnchangedTupleVersionsAndMissingRowsRollback(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	const size = 20000
	var category, other string
	rows, err := f.db.Primary().Query(t.Context(), `SELECT id FROM category WHERE for_expense ORDER BY id LIMIT 2`)
	if err != nil {
		t.Fatal(err)
	}
	cats := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		cats = append(cats, id)
	}
	rows.Close()
	if len(cats) != 2 {
		t.Fatal(cats)
	}
	category, other = cats[0], cats[1]
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := f.repo.LockUser(ctx, user, true); err != nil {
			return err
		}
		var account int64
		if err := f.db.Writer(ctx).QueryRow(ctx, `INSERT INTO account(user_id,source,ext_id,type,is_other_bank,currency)VALUES($1,'tbank_ofx','perf','checking',false,'RUB')RETURNING id`, user).Scan(&account); err != nil {
			return err
		}
		_, err := f.db.Writer(ctx).Exec(ctx, `INSERT INTO operation(user_id,occurred_at,id,account_id,direction,amount_minor,currency,status,bank_category_id,category_id)
 SELECT $1,$2::timestamptz+n*interval '1 microsecond',md5(n::text)::uuid,$3,'expense',n,'RUB','posted',$4,$4 FROM generate_series(1,$5) n`, user, f.now, account, category, size)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.repo.Snapshot(t.Context(), user)
	if err != nil || len(snapshot.Operations) != size {
		t.Fatal(err, len(snapshot.Operations))
	}
	operations := snapshot.Operations
	operations[0].Category, operations[0].ManualCategory, operations[0].RefundTarget = other, other, category
	operations[0].Split, operations[0].PairID = true, uuid.NewString()
	for attempt, wantChanged := range []int{1, 0} {
		if err := f.db.Run(t.Context(), func(ctx context.Context) error {
			if err := f.repo.SaveRules(ctx, user, operations, f.now); err != nil {
				return err
			}
			var changed int
			if err := f.db.Writer(ctx).QueryRow(ctx, `SELECT count(*) FROM operation WHERE user_id=$1 AND xmin::text=pg_current_xact_id()::text`, user).Scan(&changed); err != nil {
				return err
			}
			if changed != wantChanged {
				t.Fatalf("attempt %d rewrote %d tuples, want %d", attempt, changed, wantChanged)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.repo.Snapshot(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	first := current.Operations[0]
	if first.Category != other || first.ManualCategory != other || first.RefundTarget != category || !first.Split || first.PairID != operations[0].PairID || first.AmountMinor != snapshot.Operations[0].AmountMinor {
		t.Fatalf("caller rules/source facts changed: %+v", first)
	}
	broken := append([]importing.Operation(nil), operations...)
	broken[0].Category = category
	broken[size-1].ID = uuid.NewString()
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { return f.repo.SaveRules(ctx, user, broken, f.now.Add(time.Second)) }); err == nil {
		t.Fatal("missing operation accepted")
	}
	var kept string
	if err := f.db.Primary().QueryRow(t.Context(), `SELECT category_id FROM operation WHERE user_id=$1 AND occurred_at=$2 AND id=$3`, user, first.At, first.ID).Scan(&kept); err != nil || kept != other {
		t.Fatal("partial bulk mutation committed", kept, err)
	}
}
func TestIntegrationReferenceBatchUsesPrefixOwnerAndOnlyActivePrimaryRows(t *testing.T) {
	f := importRepository(t)
	a, b := uuid.NewString(), uuid.NewString()
	var keys []string
	if err := f.db.Run(t.Context(), func(ctx context.Context) error {
		for _, user := range []string{a, b} {
			if _, err := f.repo.LockUser(ctx, user, true); err != nil {
				return err
			}
		}
		for i, status := range []string{"staging", "processing", "done", "failed", "deleted", "processing"} {
			owner := a
			if i == 1 {
				owner = b
			}
			key := owner + "/" + uuid.NewString()
			if i == 5 {
				key = b + "/" + uuid.NewString()
			}
			keys = append(keys, key)
			_, err := f.db.Writer(ctx).Exec(ctx, `INSERT INTO upload(user_id,id,source,file_name,size_bytes,content_sha256,object_key,status)VALUES($1,$2,'tbank_ofx','perf',1,$3,$4,$5)`, owner, uuid.NewString(), make([]byte, 32), key, status)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The replica database deliberately has no service schema. The prefilter
	// must still prove current primary references, regardless of read hints.
	routed, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: f.dsn, ReplicaDSN: testdb.DSN(t), MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer routed.Close()
	got, err := postgres.NewImports(routed, testconfig.DefaultDailyRecalculationOffset(t)).ReferencedBatch(t.Context(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[keys[0]] || !got[keys[1]] || !got[keys[2]] {
		t.Fatal("batch lost active refs or crossed prefix tenant", got)
	}
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, b, f.now); err != nil {
		t.Fatal(err)
	}
	retired, err := postgres.NewImports(routed, testconfig.DefaultDailyRecalculationOffset(t)).ReferencedBatch(t.Context(), keys)
	if err != nil || len(retired) != 2 || retired[keys[1]] {
		t.Fatal("retired retained reference cannot suppress erasure", retired, err)
	}
	if _, err := f.repo.ReferencedBatch(t.Context(), []string{"not-uuid/key"}); err == nil {
		t.Fatal("invalid batch source did not fail closed")
	}
}
