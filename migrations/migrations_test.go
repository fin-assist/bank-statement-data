package migrations_test

import (
	"testing"

	"github.com/fin-assist/go-migrate"
	"github.com/fin-assist/go-migrate/migratetest"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/testdb"
)

// Every migration source the service applies passes up → down → up on PostgreSQL18, in
// the order of app.MigrationSources: a later source (the service) may reference the
// tables of an earlier one (a library), so all of them share one database.
func TestIntegrationUpDownUp(t *testing.T) {
	sources := app.MigrationSources()
	if len(sources) == 0 {
		t.Fatal("no migration sources")
	}
	dsn := testdb.DSN(t)
	for _, src := range sources {
		if err := migrate.Validate(src); err != nil {
			t.Fatalf("source %s: %v", src.Name, err)
		}
		migratetest.CheckUpDownUp(t, dsn, src)
	}
}
