package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/recalculating"
	"github.com/fin-assist/bank-statement-data/internal/testconfig"
)

type recalcTx struct {
	perfSQLTx
	queryErr, rowErr, execErr error
	queries                   []string
	version                   int32
	missing                   bool
	badJSON                   bool
	noRowsAffected            bool
}

func (t *recalcTx) Exec(_ context.Context, q string, _ ...any) (pgconn.CommandTag, error) {
	t.queries = append(t.queries, q)
	if t.noRowsAffected {
		return pgconn.NewCommandTag("UPDATE 0"), t.execErr
	}
	return pgconn.NewCommandTag("UPDATE 1"), t.execErr
}
func (t *recalcTx) QueryRow(_ context.Context, q string, _ ...any) pgx.Row {
	return recalcRow(func(args ...any) error {
		if t.rowErr != nil {
			return t.rowErr
		}
		if t.missing {
			return pgx.ErrNoRows
		}
		switch {
		case strings.Contains(q, "SELECT calc_version"):
			*args[0].(*int32) = t.version
			*args[1].(**time.Time) = nil
			*args[2].(*string) = "UTC"
		case strings.Contains(q, "SELECT state,period_keys"):
			*args[0].(*[]byte) = []byte(`{"TimeZone":"UTC"}`)
			*args[1].(*[]byte) = []byte(`[{"Kind":"month","Key":"2026-09"}]`)
			*args[2].(*bool) = true
			if t.badJSON {
				*args[0].(*[]byte) = []byte("invalid")
			}
		case strings.Contains(q, "SELECT timezone"):
			*args[0].(*string) = "UTC"
		case strings.Contains(q, "SELECT d.user_id"):
			*args[0].(*string) = "u"
			*args[1].(*int32) = 2
			*args[2].(*[]byte) = []byte("opaque request")
			*args[3].(*bool) = false
		default:
			*args[0].(*bool) = true
		}
		return nil
	})
}
func (t *recalcTx) Query(_ context.Context, q string, _ ...any) (pgx.Rows, error) {
	if t.queryErr != nil {
		return nil, t.queryErr
	}
	if strings.Contains(q, "to_char(month") {
		return &recalcRows{values: [][]any{{"2026-09-01", int16(4), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}}}, nil
	}
	return &recalcRows{values: [][]any{{"u"}}}, nil
}

type recalcRow func(...any) error

func (r recalcRow) Scan(args ...any) error { return r(args...) }

type recalcRows struct {
	pgx.Rows
	values   [][]any
	position int
	closed   bool
	failure  error
}

func (r *recalcRows) Next() bool { r.position++; return r.position <= len(r.values) }
func (r *recalcRows) Close()     { r.closed = true }
func (r *recalcRows) Err() error { return r.failure }
func (r *recalcRows) Scan(args ...any) error {
	for i, v := range r.values[r.position-1] {
		switch value := v.(type) {
		case string:
			*args[i].(*string) = value
		case int16:
			*args[i].(*int16) = value
		case time.Time:
			*args[i].(*time.Time) = value
		}
	}
	return nil
}

func unitRecalc(t *testing.T, tx *recalcTx, fn func(context.Context, *Recalculations) error) error {
	t.Helper()
	db := &DB{transactions: pgstore.New(unitBeginner{tx})}
	repo := NewRecalculations(db, testconfig.DefaultDailyRecalculationOffset(t))
	return db.Run(t.Context(), func(ctx context.Context) error { return fn(ctx, repo) })
}
func TestUnitRecalculationRepositoryLeaseAndVersionedCompletion(t *testing.T) {
	tx := &recalcTx{version: 7}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := unitRecalc(t, tx, func(ctx context.Context, r *Recalculations) error {
		users, e := r.Pending(ctx, now, 16)
		if e != nil || len(users) != 1 {
			t.Fatal(users, e)
		}
		lease, active, e := r.Lock(ctx, "u", now)
		if e != nil || !active || lease.Version != 7 || len(lease.Queue) != 1 || !lease.Recalculating || lease.State.TimeZone != "UTC" || len(lease.Known) != 1 {
			t.Fatal(lease, active, e)
		}
		if e = r.Start(ctx, lease, lease.State); e != nil {
			return e
		}
		if e = r.Complete(ctx, lease, lease.State, lease.Known, now); e != nil {
			return e
		}
		if e = r.Fail(ctx, lease, now); e != nil {
			return e
		}
		id, e := r.StoreDelivery(ctx, "u", 8, []byte("request"), false)
		if e != nil || id == "" {
			t.Fatal(id, e)
		}
		record, exists, e := r.LoadDelivery(ctx, id)
		if e != nil || !exists || record.Version != 2 {
			t.Fatal(record, e)
		}
		if e = r.AcknowledgeDelivery(ctx, id); e != nil {
			return e
		}
		pending, e := r.DeliveryPending(ctx, "u", 8)
		if e != nil || !pending {
			t.Fatal(pending, e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(tx.queries, "\n")
	for _, q := range []string{"calc_version=calc_version+$3", "DELETE FROM recalc_queue WHERE user_id=$1", "generation=$3", "metrics_delivery", "daily_recalc_due_at"} {
		if !strings.Contains(joined, q) {
			t.Fatal("missing invariant", q)
		}
	}
}
func TestUnitRecalculationRepositoryStorageFailuresAndAdmission(t *testing.T) {
	boom := errors.New("private driver detail")
	cases := map[string]func(context.Context, *Recalculations) error{
		"pending": func(c context.Context, r *Recalculations) error { _, e := r.Pending(c, time.Now(), 16); return e },
		"lock":    func(c context.Context, r *Recalculations) error { _, _, e := r.Lock(c, "u", time.Now()); return e },
		"start": func(c context.Context, r *Recalculations) error {
			return r.Start(c, recalculating.Lease{User: "u"}, calculating.State{})
		},
		"complete": func(c context.Context, r *Recalculations) error {
			return r.Complete(c, recalculating.Lease{User: "u"}, calculating.State{TimeZone: "UTC"}, nil, time.Now())
		},
		"fail": func(c context.Context, r *Recalculations) error {
			return r.Fail(c, recalculating.Lease{User: "u", Queue: []recalculating.QueueRow{{Month: "2026-09-01", Generation: 2}}}, time.Now())
		},
		"store": func(c context.Context, r *Recalculations) error {
			_, e := r.StoreDelivery(c, "u", 2, []byte("x"), false)
			return e
		},
		"load": func(c context.Context, r *Recalculations) error {
			_, _, e := r.LoadDelivery(c, "00000000-0000-4000-8000-000000000001")
			return e
		},
		"ack":              func(c context.Context, r *Recalculations) error { return r.AcknowledgeDelivery(c, "id") },
		"pending-delivery": func(c context.Context, r *Recalculations) error { _, e := r.DeliveryPending(c, "u", 2); return e },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			tx := &recalcTx{execErr: boom, rowErr: boom, queryErr: boom}
			err := unitRecalc(t, tx, call)
			if err == nil || strings.Contains(err.Error(), "private driver") {
				t.Fatal("unsafe/missing error", err)
			}
		})
	}
	tx := &recalcTx{missing: true}
	if err := unitRecalc(t, tx, func(c context.Context, r *Recalculations) error {
		_, ok, e := r.Lock(c, "u", time.Now())
		if e != nil || ok {
			t.Fatal("missing user recreated")
		}
		_, ok, e = r.LoadDelivery(c, "00000000-0000-4000-8000-000000000001")
		if e != nil || ok {
			t.Fatal("missing payload recreated")
		}
		return r.Fail(c, recalculating.Lease{User: "u"}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	tx = &recalcTx{badJSON: true}
	if err := unitRecalc(t, tx, func(c context.Context, r *Recalculations) error { _, _, e := r.Lock(c, "u", time.Now()); return e }); err == nil {
		t.Fatal("corrupt state accepted")
	}
	tx = &recalcTx{noRowsAffected: true}
	if err := unitRecalc(t, tx, func(c context.Context, r *Recalculations) error {
		return r.Start(c, recalculating.Lease{User: "u"}, calculating.State{})
	}); err == nil {
		t.Fatal("stale version advanced")
	}
}
func TestUnitRecalculationRepositoryRejectsInvalidInputsAndForeignTransactions(t *testing.T) {
	db := &DB{}
	r := NewRecalculations(db, 0)
	if _, e := r.Pending(t.Context(), time.Now(), 0); e == nil {
		t.Fatal("unbounded candidate query")
	}
	for _, call := range []func() error{
		func() error { _, _, e := r.Lock(t.Context(), "u", time.Now()); return e }, func() error { return r.Start(t.Context(), recalculating.Lease{}, calculating.State{}) }, func() error {
			return r.Complete(t.Context(), recalculating.Lease{}, calculating.State{}, nil, time.Now())
		}, func() error { return r.Fail(t.Context(), recalculating.Lease{}, time.Now()) }, func() error { _, e := r.StoreDelivery(t.Context(), "u", 1, []byte("x"), false); return e }, func() error { _, _, e := r.LoadDelivery(t.Context(), "invalid"); return e },
	} {
		if call() == nil {
			t.Fatal("missing owned transaction accepted")
		}
	}
	for _, version := range []int32{2147483646, 2147483647} {
		if err := unitRecalc(t, &recalcTx{}, func(c context.Context, r *Recalculations) error {
			return r.Complete(c, recalculating.Lease{Version: version}, calculating.State{}, nil, time.Now())
		}); err == nil {
			t.Fatal("version overflow")
		}
	}
	if err := unitRecalc(t, &recalcTx{}, func(c context.Context, r *Recalculations) error {
		_, e := r.StoreDelivery(c, "u", 0, []byte("x"), false)
		return e
	}); err == nil {
		t.Fatal("invalid publication")
	}
}
