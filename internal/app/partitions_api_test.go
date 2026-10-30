package app_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/go-worker"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

type partitionAPIClock struct {
	ready chan struct{}
	ticks chan time.Time
	once  sync.Once
}
type partitionAPITicker struct{ clock *partitionAPIClock }

func (c *partitionAPIClock) NewTicker(time.Duration) worker.Ticker { return partitionAPITicker{c} }
func (c *partitionAPIClock) NewTimer(time.Duration) worker.Timer   { return partitionAPITicker{c} }
func (t partitionAPITicker) C() <-chan time.Time {
	t.clock.once.Do(func() { close(t.clock.ready) })
	return t.clock.ticks
}
func (partitionAPITicker) Stop() {}

func TestAPIPartitionJobEnablesNewYearImport(t *testing.T) {
	clock := &partitionAPIClock{ready: make(chan struct{}), ticks: make(chan time.Time)}
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return time.Date(2028, 10, 1, 0, 0, 0, 0, time.UTC) }, PartitionClock: clock})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-clock.ready:
	case <-ctx.Done():
		t.Fatal("assembled partition job did not run")
	}
	var exists bool
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT to_regclass('public.operation_y2029') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatal("next year partition not created by real assembly", err)
	}
	c := pb.NewStatementsServiceClient(env.Conn)
	body := strings.ReplaceAll(apiOFX, "2026", "2028")
	upload, err := apiUpload(t, c, apiUser, uuid.NewString(), "new-year.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	result := apiComplete(t, c, upload.UploadId)
	if result.NewCount != 1 || result.OperationCount != 1 {
		t.Fatal("2028 import did not complete", result)
	}
	var table string
	if err = env.App.DB.Primary().QueryRow(ctx, `SELECT tableoid::regclass::text FROM operation WHERE user_id=$1`, apiUser).Scan(&table); err != nil || table != "operation_y2028" {
		t.Fatal("wrong imported partition", table, err)
	}
	replay, err := apiUpload(t, c, apiUser, uuid.NewString(), "new-year-again.ofx", body)
	if err != nil {
		t.Fatal(err)
	}
	if repeated := apiComplete(t, c, replay.UploadId); repeated.DuplicateCount != 1 || repeated.NewCount != 0 {
		t.Fatal("new year dedup changed", repeated)
	}
}
