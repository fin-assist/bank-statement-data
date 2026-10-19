package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres/migrate"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/bank-statement-data/migrations"
)

// The invariants of the schema (FIN-111) are checked by real inserts: a constraint that is
// dropped or weakened by a later migration fails one of these tests.

const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	checkViolation      = "23514"
	lockNotAvailable    = "55P03"
)

const (
	userA = "01a10800-0000-7000-8000-00000000000a"
	userB = "01a10800-0000-7000-8000-00000000000b"
)

// schema returns a connection to a fresh database with the service migrations applied.
func schema(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	dsn := migratetest.DSN(t)
	c := migrate.DefaultConfig(dsn)
	c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Up(context.Background(), c, []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	return connect(t, dsn), dsn
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func exec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// fails runs sql and expects it to fail with the given SQLSTATE.
func fails(t *testing.T, conn *pgx.Conn, code, sql string, args ...any) {
	t.Helper()
	_, err := conn.Exec(context.Background(), sql, args...)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("%s: want SQLSTATE %s, got %v", sql, code, err)
	}
	if pg.Code != code {
		t.Fatalf("%s: want SQLSTATE %s, got %s (%s)", sql, code, pg.Code, pg.Message)
	}
}

func count(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// fixture is one user with an account, a finished upload and one operation with its source
// record. IDs are returned so tests can build on them.
type fixture struct {
	user      string
	account   int64
	upload    string
	operation string
	at        time.Time
}

const bankCategory = "cat_supermarkets"

func seedCategories(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	exec(t, conn, `INSERT INTO category (id, name, bank_name, icon, is_known, for_expense, for_income, assignable, sort_order)
		VALUES ('cat_supermarkets', 'Супермаркеты', 'Супермаркеты', 'cart', true, true, false, true, 100),
		       ('cat_transfers', 'Переводы', 'Переводы', 'users', true, true, true, true, 110)`)
}

func seed(t *testing.T, conn *pgx.Conn, user string, hmacByte byte) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{user: user, at: time.Date(2026, 9, 25, 16, 11, 0, 0, time.UTC)}
	exec(t, conn, `INSERT INTO user_state (user_id) VALUES ($1)`, user)
	hmac := make([]byte, 32)
	hmac[0] = hmacByte
	if err := conn.QueryRow(ctx, `INSERT INTO account (user_id, source_id, ext_id_hmac, ext_id_key_version, type, mask, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 1, 'checking', '4821', false) RETURNING id`, user, hmac).Scan(&f.account); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256, object_key,
			status, finished_at, operation_count, result)
		VALUES ($1, 'tbank_ofx', 'operations.ofx', 100, $2, $3, 'done', now(), 1, '{}') RETURNING id`,
		user, make([]byte, 32), user+"/u1").Scan(&f.upload); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO upload_account (user_id, upload_id, account_id, statement_start, statement_end)
		VALUES ($1, $2, $3, '2026-09-01 00:00+03', '2026-09-25 00:00+03')`, user, f.upload, f.account)
	if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, description, bank_category_id, category_id)
		VALUES ($1, $2, $3, 'expense', 234000, 'RUB', 'posted', 'Пятёрочка', $4, $4) RETURNING id`,
		user, f.at, f.account, bankCategory).Scan(&f.operation); err != nil {
		t.Fatal(err)
	}
	var record int64
	if err := conn.QueryRow(ctx, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, '123456789012', $3, $4) RETURNING id`, user, f.account, f.at, f.operation).Scan(&record); err != nil {
		t.Fatal(err)
	}
	exec(t, conn, `INSERT INTO upload_record (user_id, upload_id, source_record_id) VALUES ($1, $2, $3)`, user, f.upload, record)
	return f
}

func TestSourceRecordDeduplication(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	seed(t, conn, userB, 1)

	// The same FITID on the same account of the same user is one record.
	fails(t, conn, uniqueViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, '123456789012', $3, $4)`, a.user, a.account, a.at, a.operation)
	// Another user with the same account number (same HMAC input) and FITID is not a duplicate:
	// seed(userB) already inserted it. Uniqueness never spans users.
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE external_id = '123456789012'`); n != 2 {
		t.Fatalf("records with the FITID: %d, want 2 (one per user)", n)
	}
}

func TestAccountIsUniquePerUserAndHasNoNumber(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 7)
	seed(t, conn, userB, 7) // the same HMAC for another user is allowed

	hmac := make([]byte, 32)
	hmac[0] = 7
	fails(t, conn, uniqueViolation, `INSERT INTO account (user_id, source_id, ext_id_hmac, ext_id_key_version, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 1, 'savings', false)`, a.user, hmac)
	// Only a 32-byte HMAC fits: a raw 20-digit number does not.
	fails(t, conn, checkViolation, `INSERT INTO account (user_id, source_id, ext_id_hmac, ext_id_key_version, type, is_other_bank)
		VALUES ($1, 'tbank_ofx', convert_to('40817810000000004821', 'UTF8'), 1, 'checking', false)`, a.user)
	// An account of another bank has no mask.
	hmac[0] = 8
	fails(t, conn, checkViolation, `INSERT INTO account (user_id, source_id, ext_id_hmac, ext_id_key_version, type, mask, is_other_bank)
		VALUES ($1, 'tbank_ofx', $2, 1, 'checking', '1234', true)`, a.user, hmac)
}

func TestRowsCannotPointAtAnotherUser(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	b := seed(t, conn, userB, 2)

	fails(t, conn, foreignKeyViolation, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency,
			status, bank_category_id, category_id)
		VALUES ($1, now(), $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, b.user, a.account, bankCategory)
	fails(t, conn, foreignKeyViolation, `INSERT INTO source_record (user_id, account_id, external_id, operation_occurred_at, operation_id)
		VALUES ($1, $2, 'x', $3, $4)`, b.user, b.account, a.at, a.operation)
	fails(t, conn, foreignKeyViolation, `INSERT INTO unread_line (user_id, upload_id, line_number, reason, resolved_by_upload_id, resolution)
		VALUES ($1, $2, 1, 'truncated', $3, 'read')`, b.user, b.upload, a.upload)
	fails(t, conn, foreignKeyViolation, `INSERT INTO upload_record (user_id, upload_id, source_record_id)
		SELECT $1, $2, id FROM source_record WHERE user_id = $3`, b.user, b.upload, a.user)
}

func TestOperationPartitions(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)

	var part string
	if err := conn.QueryRow(context.Background(),
		`SELECT tableoid::regclass::text FROM operation WHERE user_id = $1 AND id = $2`, a.user, a.operation).Scan(&part); err != nil {
		t.Fatal(err)
	}
	if part != "operation_y2026" {
		t.Fatalf("operation of 2026 is in %s", part)
	}
	// Partition bounds are UTC: 2025-12-31 23:30 UTC is still 2025 (01:30 of 2026 in Moscow;
	// months in the user's zone come from the query, not from the partition).
	exec(t, conn, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency, status, bank_category_id, category_id)
		VALUES ($1, '2025-12-31 23:30:00+00', $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, a.user, a.account, bankCategory)
	if n := count(t, conn, `SELECT count(*) FROM operation_y2025 WHERE user_id = $1`, a.user); n != 1 {
		t.Fatalf("operations in 2025: %d, want 1", n)
	}
	// No partition for the date: the insert fails instead of landing in a default partition.
	fails(t, conn, checkViolation, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency, status, bank_category_id, category_id)
		VALUES ($1, '2009-06-01 00:00:00+00', $2, 'expense', 1, 'RUB', 'posted', $3, $3)`, a.user, a.account, bankCategory)

	// Moving an operation to another year moves its source record and pair references with it.
	exec(t, conn, `UPDATE operation SET occurred_at = '2027-01-02 00:00:00+00' WHERE user_id = $1 AND id = $2`, a.user, a.operation)
	if n := count(t, conn, `SELECT count(*) FROM source_record WHERE user_id = $1 AND operation_occurred_at = '2027-01-02 00:00:00+00'`, a.user); n != 1 {
		t.Fatalf("source record did not follow the operation: %d", n)
	}
}

func TestOperationCategoryInvariants(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	op := `UPDATE operation SET %s WHERE user_id = '` + a.user + `' AND id = '` + a.operation + `'`
	set := func(s string) string { return fmt.Sprintf(op, s) }

	// A manual category is the final category.
	fails(t, conn, checkViolation, set(`manual_category_id = 'cat_transfers'`))
	exec(t, conn, set(`manual_category_id = 'cat_transfers', category_id = 'cat_transfers'`))
	// cat_uncategorized cannot be chosen by hand (assignable = false).
	fails(t, conn, checkViolation, set(`manual_category_id = 'cat_uncategorized', category_id = 'cat_uncategorized'`))
	// A refund is an income with a target.
	fails(t, conn, checkViolation, set(`manual_category_id = NULL, category_id = 'cat_refund'`))
	fails(t, conn, checkViolation, set(`manual_category_id = NULL, category_id = 'cat_refund', refund_target_category_id = 'cat_supermarkets'`))
	exec(t, conn, set(`manual_category_id = NULL, direction = 'income', category_id = 'cat_refund', refund_target_category_id = 'cat_supermarkets'`))
	fails(t, conn, checkViolation, set(`refund_target_category_id = 'cat_own_transfer'`))
	// A target only on a refund.
	fails(t, conn, checkViolation, set(`category_id = 'cat_supermarkets'`))
	// Amounts are unsigned.
	fails(t, conn, checkViolation, set(`amount_minor = -1`))
	// An unknown category ID is not accepted.
	fails(t, conn, foreignKeyViolation, set(`bank_category_id = 'cat_unknown'`))
	// FIN-67: an operation without a description is accepted.
	exec(t, conn, set(`description = ''`))
}

func TestCategoryInvariants(t *testing.T) {
	conn, _ := schema(t)
	// A bank category not yet in the registry applies to both directions (docs/calc.md 3.1).
	fails(t, conn, checkViolation, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_kripto', 'Крипто', 'Крипто', 'tag', false, true, true)`)
	exec(t, conn, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_kripto', 'Крипто', 'Крипто', 'tag', true, true, true)`)
	// One dictionary row per bank category name.
	fails(t, conn, uniqueViolation, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_kripto_2', 'Крипто', 'Крипто', 'tag', true, true, true)`)
	// Names are cut to 64 characters by the service; a longer one is a bug.
	fails(t, conn, checkViolation, `INSERT INTO category (id, name, bank_name, icon, for_expense, for_income, assignable)
		VALUES ('cat_long', repeat('я', 65), repeat('я', 65), 'tag', true, true, true)`)
	if n := count(t, conn, `SELECT count(*) FROM category WHERE is_system`); n != 3 {
		t.Fatalf("system categories: %d, want 3", n)
	}
}

func TestUploadStatusInvariants(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	insert := `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256, object_key, status, finished_at, error_code, deleted_at, operation_count, result)
		VALUES ($1, 'tbank_ofx', 'f.ofx', 1, $2, 'k', $3, $4, $5, $6, $7, $8)`
	sha := make([]byte, 32)
	now := time.Now()
	type row struct {
		status   string
		finished any
		errCode  any
		deleted  any
		opCount  any
		result   any
	}
	bad := map[string]row{
		"processing with finished_at": {"processing", now, nil, nil, nil, nil},
		"done without finished_at":    {"done", nil, nil, nil, 1, "{}"},
		"done without result":         {"done", now, nil, nil, 1, nil},
		"failed without error code":   {"failed", now, nil, nil, nil, nil},
		"error code on done":          {"done", now, "cancelled", nil, 1, "{}"},
		"deleted without deleted_at":  {"deleted", now, nil, nil, 1, nil},
		"unknown status":              {"queued", nil, nil, nil, nil, nil},
	}
	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			fails(t, conn, checkViolation, insert, a.user, sha, r.status, r.finished, r.errCode, r.deleted, r.opCount, r.result)
		})
	}
	exec(t, conn, insert, a.user, sha, "failed", now, "processing_failed", nil, nil, nil)
	exec(t, conn, insert, a.user, sha, "processing", nil, nil, nil, nil, nil)
	// The same content may be uploaded again (FIN-79: a fixed or the same file).
	exec(t, conn, insert, a.user, sha, "done", now, nil, nil, 0, "{}")
}

func TestUnreadLineInvariants(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	var later string
	if err := conn.QueryRow(context.Background(), `INSERT INTO upload (user_id, source_id, file_name, size_bytes, content_sha256, object_key, status, finished_at, operation_count, result)
		VALUES ($1, 'tbank_ofx', 'fixed.ofx', 1, $2, 'k2', 'done', now(), 1, '{}') RETURNING id`, a.user, make([]byte, 32)).Scan(&later); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO unread_line (user_id, upload_id, line_number, reason, occurred_at, account_id, external_id, resolved_by_upload_id, resolution)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	exec(t, conn, ins, a.user, a.upload, 412, "bad_amount", day, a.account, "FIT1", nil, nil)
	// The same line number of one upload is one line.
	fails(t, conn, uniqueViolation, ins, a.user, a.upload, 412, "bad_amount", day, a.account, "FIT1", nil, nil)
	// A closed line has both the closing upload and the outcome; never itself.
	fails(t, conn, checkViolation, ins, a.user, a.upload, 413, "truncated", nil, a.account, nil, later, nil)
	fails(t, conn, checkViolation, ins, a.user, a.upload, 414, "truncated", nil, a.account, nil, a.upload, "read")
	// A line without a date cannot carry one.
	fails(t, conn, checkViolation, ins, a.user, a.upload, 415, "no_date", day, a.account, nil, nil, nil)
	// FITID without an account cannot close anything.
	fails(t, conn, checkViolation, ins, a.user, a.upload, 416, "bad_amount", day, nil, "FIT2", nil, nil)
	exec(t, conn, ins, a.user, a.upload, 417, "truncated", nil, a.account, nil, later, "replaced")

	// Deleting the closing upload (logically) reopens its lines: the service clears the reference.
	exec(t, conn, `UPDATE upload SET status = 'deleted', deleted_at = now(), result = NULL WHERE user_id = $1 AND id = $2`, a.user, later)
	exec(t, conn, `UPDATE unread_line SET resolved_by_upload_id = NULL, resolution = NULL WHERE user_id = $1 AND resolved_by_upload_id = $2`, a.user, later)
	if n := count(t, conn, `SELECT count(*) FROM unread_line WHERE user_id = $1 AND resolved_by_upload_id IS NULL`, a.user); n != 2 {
		t.Fatalf("open lines after the closing upload was deleted: %d, want 2", n)
	}
}

func TestTransferPairInvariants(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	ctx := context.Background()
	newOp := func(direction string, at time.Time) string {
		var id string
		if err := conn.QueryRow(ctx, `INSERT INTO operation (user_id, occurred_at, account_id, direction, amount_minor, currency, status, bank_category_id, category_id)
			VALUES ($1, $2, $3, $4, 2000000, 'RUB', 'posted', 'cat_transfers', 'cat_own_transfer') RETURNING id`, a.user, at, a.account, direction).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t0 := time.Date(2026, 9, 18, 6, 30, 0, 0, time.UTC)
	debit, credit, credit2 := newOp("expense", t0), newOp("income", t0.Add(time.Minute)), newOp("income", t0.Add(2*time.Minute))
	ins := `INSERT INTO transfer_pair (user_id, debit_occurred_at, debit_operation_id, credit_occurred_at, credit_operation_id) VALUES ($1, $2, $3, $4, $5)`
	exec(t, conn, ins, a.user, t0, debit, t0.Add(time.Minute), credit)
	// An operation is in at most one pair (docs/calc.md 3.2).
	fails(t, conn, uniqueViolation, ins, a.user, t0, debit, t0.Add(2*time.Minute), credit2)
	fails(t, conn, checkViolation, ins, a.user, t0, debit, t0, debit)
	// Losing a side deletes the pair.
	exec(t, conn, `DELETE FROM operation WHERE user_id = $1 AND id = $2`, a.user, credit)
	if n := count(t, conn, `SELECT count(*) FROM transfer_pair WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("pairs after a side was deleted: %d", n)
	}
}

func TestRegularPaymentIDIsStable(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	seed(t, conn, userA, 1)
	seed(t, conn, userB, 2)
	upsert := `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'monthly', $2)
		ON CONFLICT (user_id, periodicity, description) DO UPDATE SET description = EXCLUDED.description RETURNING id`
	get := func(user string) string {
		var id string
		if err := conn.QueryRow(context.Background(), upsert, user, "Яндекс Плюс").Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := get(userA)
	exec(t, conn, `UPDATE regular_payment SET dismissed_at = now() WHERE user_id = $1 AND id = $2`, userA, first)
	if again := get(userA); again != first {
		t.Fatalf("payment ID changed between recalculations: %s -> %s", first, again)
	}
	if n := count(t, conn, `SELECT count(*) FROM regular_payment WHERE user_id = $1 AND dismissed_at IS NOT NULL`, userA); n != 1 {
		t.Fatal("the dismissal did not survive recognition")
	}
	if other := get(userB); other == first {
		t.Fatal("two users share a payment ID")
	}
	fails(t, conn, checkViolation, `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'monthly', '')`, userA)
	fails(t, conn, checkViolation, `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'weekly', 'x')`, userA)
}

func TestNoDataIsNotZero(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	agg := `INSERT INTO monthly_agg (user_id, month, account_id, category_id, kind, currency, amount_minor, op_count)
		VALUES ($1, '2026-09-01', $2, 'cat_supermarkets', 'expense', 'RUB', 234000, 1)`
	// A sum needs a covered month: without month_state there is no data, not a zero.
	fails(t, conn, foreignKeyViolation, agg, a.user, a.account)
	exec(t, conn, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-09-01', 'Europe/Moscow', 25, 30, '2026-09-01 00:00+03', '2026-09-25 21:40+03', 1, now())`, a.user)
	exec(t, conn, agg, a.user, a.account)
	fails(t, conn, checkViolation, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-08-01', 'Europe/Moscow', 0, 31, '2026-08-01 00:00+03', '2026-08-01 00:00+03', 1, now())`, a.user)
	fails(t, conn, checkViolation, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-08-15', 'Europe/Moscow', 1, 31, now(), now(), 1, now())`, a.user)
	// Recalculating the month replaces it: deleting the month removes its sums.
	exec(t, conn, `DELETE FROM month_state WHERE user_id = $1 AND month = '2026-09-01'`, a.user)
	if n := count(t, conn, `SELECT count(*) FROM monthly_agg WHERE user_id = $1`, a.user); n != 0 {
		t.Fatalf("sums left without their month: %d", n)
	}
}

func TestRecalcQueueMarksAMonthOnce(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	mark := `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')
		ON CONFLICT (user_id, month) DO UPDATE SET generation = recalc_queue.generation + 1, next_attempt_at = now()`
	exec(t, conn, mark, a.user)
	exec(t, conn, mark, a.user)
	if n := count(t, conn, `SELECT generation FROM recalc_queue WHERE user_id = $1`, a.user); n != 2 {
		t.Fatalf("generation after two marks: %d, want 2", n)
	}
	// A worker that read generation 1 must not delete a row marked again since.
	tag, err := conn.Exec(context.Background(), `DELETE FROM recalc_queue WHERE user_id = $1 AND month = '2026-09-01' AND generation = 1`, a.user)
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("stale worker deleted a re-marked month: %v %d", err, tag.RowsAffected())
	}
	fails(t, conn, checkViolation, `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-02')`, a.user)
}

func TestOneCurrentMetricDefinition(t *testing.T) {
	conn, _ := schema(t)
	exec(t, conn, `INSERT INTO metric_definition (metric_id, version, is_current) VALUES ('insights.regular_payments', 1, true)`)
	fails(t, conn, uniqueViolation, `INSERT INTO metric_definition (metric_id, version, is_current) VALUES ('insights.regular_payments', 2, true)`)
	exec(t, conn, `INSERT INTO metric_definition (metric_id, version) VALUES ('insights.regular_payments', 2)`)
	fails(t, conn, foreignKeyViolation, `INSERT INTO user_metric_version (user_id, metric_id, version) VALUES ($1, 'insights.regular_payments', 3)`, userA)
}

// PurgeUserData is one DELETE of the user's row: nothing of the user is left, the other user
// is untouched.
func TestPurgeUserRemovesEverything(t *testing.T) {
	conn, _ := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	seed(t, conn, userB, 1)
	ctx := context.Background()
	exec(t, conn, `INSERT INTO unread_line (user_id, upload_id, line_number, reason) VALUES ($1, $2, 1, 'truncated')`, a.user, a.upload)
	exec(t, conn, `INSERT INTO regular_payment (user_id, periodicity, description) VALUES ($1, 'monthly', 'x')`, a.user)
	exec(t, conn, `INSERT INTO merchant (user_id, key) VALUES ($1, 'x')`, a.user)
	exec(t, conn, `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')`, a.user)
	exec(t, conn, `INSERT INTO user_bank_category (user_id, category_id, first_uploaded_at, first_line_number) VALUES ($1, 'cat_supermarkets', now(), 1)`, a.user)
	exec(t, conn, `INSERT INTO month_state (user_id, month, time_zone, covered_days, days_in_month, data_from, data_to, calc_version, calculated_at)
		VALUES ($1, '2026-09-01', 'Europe/Moscow', 25, 30, now(), now(), 1, now())`, a.user)
	exec(t, conn, `INSERT INTO monthly_agg (user_id, month, account_id, category_id, kind, currency, amount_minor, op_count)
		VALUES ($1, '2026-09-01', $2, 'cat_supermarkets', 'expense', 'RUB', 1, 1)`, a.user, a.account)

	exec(t, conn, `DELETE FROM user_state WHERE user_id = $1`, a.user)

	rows, err := conn.Query(ctx, `SELECT c.relname FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
		  AND a.attname = 'user_id' AND NOT a.attisdropped`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) < 15 {
		t.Fatalf("user tables found: %d, the query is wrong", len(tables))
	}
	for _, table := range tables {
		if n := count(t, conn, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+` WHERE user_id = $1`, a.user); n != 0 {
			t.Errorf("%s: %d rows of the purged user", table, n)
		}
	}
	if n := count(t, conn, `SELECT count(*) FROM operation WHERE user_id = $1`, userB); n != 1 {
		t.Errorf("the other user lost operations: %d", n)
	}
}

// The per-user lock is FOR NO KEY UPDATE on user_state. It serialises per-user steps
// (closing unread lines, calc_version) and does not conflict with the FOR KEY SHARE that the
// foreign key of every child insert takes, so an import holding the lock and another
// transaction writing rows of the same user do not wait for each other. FOR UPDATE would.
func TestUserLockDoesNotBlockChildInserts(t *testing.T) {
	conn, dsn := schema(t)
	seedCategories(t, conn)
	a := seed(t, conn, userA, 1)
	ctx := context.Background()

	holder := connect(t, dsn)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM user_state WHERE user_id = $1 FOR NO KEY UPDATE`, a.user); err != nil {
		t.Fatal(err)
	}

	other := connect(t, dsn)
	exec(t, other, `SET lock_timeout = '500ms'`)
	// A child insert of the same user passes the lock.
	exec(t, other, `INSERT INTO merchant (user_id, key) VALUES ($1, 'Самокат')`, a.user)
	// A second per-user step waits.
	fails(t, other, lockNotAvailable, `SELECT 1 FROM user_state WHERE user_id = $1 FOR NO KEY UPDATE`, a.user)

	// The other way round: an open transaction that inserted a child row blocks FOR UPDATE
	// of the user, but not FOR NO KEY UPDATE.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	writer, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	// recalc_queue references user_state directly, so its insert takes FOR KEY SHARE on the user.
	if _, err := writer.Exec(ctx, `INSERT INTO recalc_queue (user_id, month) VALUES ($1, '2026-09-01')`, a.user); err != nil {
		t.Fatal(err)
	}
	fails(t, other, lockNotAvailable, `SELECT 1 FROM user_state WHERE user_id = $1 FOR UPDATE`, a.user)
	exec(t, other, `SELECT 1 FROM user_state WHERE user_id = $1 FOR NO KEY UPDATE`, a.user)
}
