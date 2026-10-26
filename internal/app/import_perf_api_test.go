package app_test

import (
	"context"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/app/apitest"
)

func TestAPIReferencedHistorySweepDoesNotBlockAnAdmittedWriterBarrier(t *testing.T) {
	future := time.Now().Add(2 * time.Hour)
	env := apitest.StartWith(t, app.Options{Now: func() time.Time { return future }})
	client := pb.NewStatementsServiceClient(env.Conn)
	accepted, err := apiUpload(t, client, apiUser, uuid.NewString(), "healthy.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, accepted.UploadId)
	retired, err := apiUpload(t, client, apiOther, uuid.NewString(), "retired.ofx", apiOFX)
	if err != nil {
		t.Fatal(err)
	}
	apiComplete(t, client, retired.UploadId, apiOther)
	// A committed tombstone with retained source metadata is the real partial-purge boundary.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO tombstone_users(user_id,deleted_at)VALUES($1,$2)`, apiOther, future); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	release, err := postgres.NewObjectBarrier(pool).Shared(t.Context(), apiUser)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Error(err)
		}
	}()
	// The admitted writer holds a real cross-pool shared lease. Healthy history
	// must be discarded by the primary batch before any exclusive lock is tried.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := env.App.Imports.SweepOrphans(ctx); err != nil {
		t.Fatalf("healthy source tried an exclusive orphan lock: %v", err)
	}
	if body, ok := env.Objects.Bytes(apiUser + "/" + accepted.UploadId); !ok || len(body) == 0 {
		t.Fatal("healthy source erased")
	}
	if body, ok := env.Objects.Bytes(apiOther + "/" + retired.UploadId); !ok || len(body) != 0 {
		t.Fatal("retired source survived due to a retained upload reference")
	}
	apiComplete(t, client, accepted.UploadId)
}
