package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIntegrationPendingSelectsOldestHeadPerUserBeforeGlobalLimit(t *testing.T) {
	f := importRepository(t)
	a, b := uuid.NewString(), uuid.NewString()
	var firstA, firstB string
	for i := range 6 {
		user := a
		if i == 5 {
			user = b
		}
		id := uuid.NewString()
		if i == 0 {
			firstA = id
		}
		if i == 5 {
			firstB = id
		}
		if err := f.db.Run(t.Context(), func(ctx context.Context) error {
			if err := f.admitted(ctx, user, id); err != nil {
				return err
			}
			_, err := f.db.Writer(ctx).Exec(ctx, `UPDATE upload SET uploaded_at=$3 WHERE user_id=$1 AND id=$2`, user, id, f.now.Add(time.Duration(i)*time.Second))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := f.repo.Pending(t.Context(), 2)
	if err != nil || len(pending) != 2 || pending[0].ID != firstA || pending[1].ID != firstB {
		t.Fatal("one user's backlog hid another user's head", pending, err)
	}
	// Once the first head is retired, its next oldest job becomes eligible.
	if err := f.db.Run(t.Context(), func(ctx context.Context) error { _, err := f.repo.Fail(ctx, a, firstA, "cancelled", f.now); return err }); err != nil {
		t.Fatal(err)
	}
	next, err := f.repo.Pending(t.Context(), 2)
	if err != nil || len(next) != 2 || next[0].UserID != a || next[0].ID == firstA || next[1].ID != firstB {
		t.Fatal("next durable head lost", next, err)
	}
	// Retired users never re-enter the head selection despite retained processing rows.
	if _, err := f.db.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, a, f.now); err != nil {
		t.Fatal(err)
	}
	pending, err = f.repo.Pending(t.Context(), 2)
	if err != nil || len(pending) != 1 || pending[0].ID != firstB {
		t.Fatal("retired backlog returned", pending, err)
	}
}
