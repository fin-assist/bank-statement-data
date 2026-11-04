package postgres

import (
	"context"
	"errors"
	"testing"
)

func TestUnitUnreadEvidenceSnapshotRequiresTransaction(t *testing.T) {
	for _, failure := range []error{nil, errors.New("source read failed")} {
		tx := &perfSQLTx{failure: failure, rows: &perfRows{}}
		db := perfDB(tx)
		repo := NewRecalculations(db, 0)
		if _, err := repo.UnreadSnapshot(t.Context(), "user"); err == nil {
			t.Fatal("unowned closure snapshot accepted")
		}
		if err := db.Run(t.Context(), func(ctx context.Context) error {
			state, err := repo.UnreadSnapshot(ctx, "user")
			if failure != nil {
				if !errors.Is(err, errImportStorage) {
					t.Fatal("snapshot error escaped storage boundary", err)
				}
			} else if err != nil || state.TimeZone != "Europe/Moscow" || len(state.Operations) != 0 {
				t.Fatal("closure snapshot projection changed", state, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
