package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
)

func TestIntegrationBackfillCompletedReleaseDoesNotBlockOtherCalculations(t *testing.T) {
	f := importRepository(t)
	repo := postgres.NewRecalculations(f.db, 0)
	if e := f.db.Run(t.Context(), func(c context.Context) error {
		if _, _, e := repo.BeginBackfill(c, f.now); e != nil {
			return e
		}
		return repo.AdvanceBackfill(c, "", 0, true, f.now)
	}); e != nil {
		t.Fatal(e)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- f.db.Run(t.Context(), func(c context.Context) error {
			ready, e := repo.CatalogReady(c)
			if e != nil {
				return e
			}
			if !ready {
				return errors.New("completed catalog unavailable")
			}
			close(held)
			select {
			case <-release:
				return nil
			case <-c.Done():
				return c.Err()
			}
		})
	}()
	select {
	case <-held:
	case e := <-holder:
		t.Fatal("calculation holder failed", e)
	case <-time.After(5 * time.Second):
		t.Fatal("calculation holder did not acquire release fence")
	}
	bounded, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	var complete bool
	e := f.db.Run(bounded, func(c context.Context) error { var e error; _, complete, e = repo.BeginBackfill(c, f.now); return e })
	cancel()
	close(release)
	if holderErr := <-holder; holderErr != nil {
		t.Fatal(holderErr)
	}
	if e != nil || !complete {
		t.Fatal("completed release waited for another user's calculation", complete, e)
	}
}
