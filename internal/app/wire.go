package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"
	"github.com/fin-assist/go-grpc/server"
	idem "github.com/fin-assist/go-idempotency"
	"github.com/fin-assist/go-migrate"
	outbox "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/fin-assist/go-worker"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	operationsgrpc "github.com/fin-assist/bank-statement-data/internal/adapter/grpc/operations"
	statementsgrpc "github.com/fin-assist/bank-statement-data/internal/adapter/grpc/statements"
	keyadapter "github.com/fin-assist/bank-statement-data/internal/adapter/idempotency"
	"github.com/fin-assist/bank-statement-data/internal/adapter/objectstore"
	"github.com/fin-assist/bank-statement-data/internal/adapter/ofx"
	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
	"github.com/fin-assist/bank-statement-data/internal/worker/partitions"
	"github.com/fin-assist/bank-statement-data/migrations"
)

// AccessPolicy copies the import and mandatory system lifecycle callers from grpc-api.md 3.3.
func AccessPolicy() server.Policy {
	return server.Policy{
		"/finassist.statementdata.v1.StatementsService/*":      {Callers: []string{"bank-bff"}, Model: server.ModelUser},
		"/finassist.statementdata.v1.OperationsService/*":      {Callers: []string{"bank-bff"}, Model: server.ModelUser},
		"/finassist.statementdata.v1.CategoriesService/*":      {Callers: []string{"bank-bff"}, Model: server.ModelUser},
		"/finassist.statementdata.v1.RegularPaymentsService/*": {Callers: []string{"bank-bff"}, Model: server.ModelUser},
		"/finassist.statementdata.v1.AssistantToolsService/*":  {Callers: []string{"assistant"}, Model: server.ModelSystem},
		"/finassist.usersettings.v1.UserSettingsSyncService/*": {Callers: []string{"user"}, Model: server.ModelSystem},
		"/finassist.userdeletion.v1.UserDeletionService/*":     {Callers: []string{"user"}, Model: server.ModelSystem},
	}
}

// MigrationSources initializes shared deletion and key storage before the service schema.
func MigrationSources() []migrate.Source {
	return []migrate.Source{
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: idem.MigrationsName, Table: idem.MigrationsTable, FS: idem.Migrations},
		{Name: outbox.MigrationsName, Table: outbox.MigrationsTable, FS: outbox.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire owns every resource opened after the data pool, including the independent
// object barrier pool. No network call runs inside a database transaction.
func wire(ctx context.Context, a *App, reg prometheus.Registerer, openDB func(context.Context, postgres.Options) (*postgres.DB, error), opts Options) ([]worker.Job, func(), error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	client := opts.ObjectClient
	var owned *http.Transport
	if client == nil {
		owned = &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxConnsPerHost: 8, MaxIdleConns: 8, IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 10 * time.Second}
		client = &http.Client{Transport: owned, Timeout: 10 * time.Second}
	}
	var barrier *postgres.DB
	closeAll := func() {
		if barrier != nil {
			barrier.Close()
		}
		if owned != nil {
			owned.CloseIdleConnections()
		}
	}
	fail := func(err error) ([]worker.Job, func(), error) { closeAll(); return nil, nil, err }
	c := a.Cfg.ObjectStorage
	objects, err := objectstore.New(objectstore.Config{Endpoint: c.Endpoint, Bucket: c.Bucket, KMSKeyID: c.KMSKeyID, Token: objectstore.FileToken(c.IAMTokenFile), Client: client})
	if err != nil {
		return fail(err)
	}
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = objects.CheckBucket(check)
	cancel()
	if err != nil {
		return fail(errors.New("object bucket safety check failed"))
	}
	barrier, err = openDB(ctx, postgres.Options{PrimaryDSN: a.Cfg.DB.PrimaryDSN, MaxConns: 8, AppName: a.Cfg.Service + "-object-barrier"})
	if err != nil {
		return fail(err)
	}
	keys, err := idem.New(a.DB, idem.Options{Clock: now, Registerer: reg, Methods: map[string]idem.Policy{
		statementdatav1.StatementsService_UploadStatement_FullMethodName: {TTL: 24 * time.Hour},
	}})
	if err != nil {
		return fail(err)
	}
	deleted, err := tombstone.New(a.DB, a.DB.Primary(), now)
	if err != nil {
		return fail(err)
	}
	service, err := importing.New(a.DB, postgres.NewImports(a.DB, a.Cfg.DailyRecalculationOffset), objects, postgres.NewObjectBarrier(barrier.Primary()), ofx.Parser{}, keyadapter.New(keys), deleted, now,
		func() (string, error) {
			id, err := uuid.NewV7()
			if err != nil {
				return "", err
			}
			return id.String(), nil
		}, statementdatav1.StatementsService_UploadStatement_FullMethodName)
	if err != nil {
		return fail(err)
	}
	if err = statementsgrpc.Register(a.GRPC.GRPC(), service); err != nil {
		return fail(err)
	}
	cleanup, err := worker.Every("idempotency-cleanup", time.Minute, a.Log, worker.SystemClock{}, func(ctx context.Context) error {
		work, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			var n int64
			err := a.DB.Run(work, func(ctx context.Context) error { var e error; n, e = keys.Cleanup(ctx, 1000); return e })
			if err != nil {
				return err
			}
			if n < 1000 {
				return nil
			}
		}
	})
	if err != nil {
		return fail(err)
	}
	a.Calculations, err = calculating.New(a.DB, postgres.NewImports(a.DB, a.Cfg.DailyRecalculationOffset), deleted, now)
	if err != nil {
		return fail(err)
	}
	a.Imports = service
	a.Operations, err = operations.New(a.DB, postgres.NewOperations(a.DB, a.Cfg.DailyRecalculationOffset), deleted, now, func() (string, error) { id, err := uuid.NewV7(); return id.String(), err })
	if err != nil {
		return fail(err)
	}
	if err = operationsgrpc.Register(a.GRPC.GRPC(), a.Operations); err != nil {
		return fail(err)
	}
	tools, err := operations.NewTools(a.Operations, postgres.NewToolFacts(a.DB))
	if err != nil {
		return fail(err)
	}
	if err = operationsgrpc.RegisterTools(a.GRPC.GRPC(), tools); err != nil {
		return fail(err)
	}
	partitionClock := opts.PartitionClock
	if partitionClock == nil {
		partitionClock = worker.SystemClock{}
	}
	partitionJob, err := partitions.New(a.DB, postgres.NewOperationPartitions(a.DB), now, partitionClock, a.Log, reg)
	if err != nil {
		return fail(err)
	}
	recalcJobs, closeRecalc, err := wireRecalculations(ctx, a, reg, opts, now, objects)
	if err != nil {
		return fail(err)
	}
	return append([]worker.Job{{Name: "statement-import", Run: service.Run}, {Name: "statement-files", Run: service.RunCleanup}, cleanup, partitionJob}, recalcJobs...), func() { closeRecalc(); closeAll() }, nil
}
