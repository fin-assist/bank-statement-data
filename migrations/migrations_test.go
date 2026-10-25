package migrations_test

import (
	"testing"

	"github.com/fin-assist/go-migrate"
	"github.com/fin-assist/go-migrate/migratetest"

	"github.com/fin-assist/bank-statement-data/internal/testdb"
	"github.com/fin-assist/bank-statement-data/migrations"
)

// Every real service migration applies, rolls back and applies again on PostgreSQL18.
func TestIntegrationUpDownUp(t *testing.T) {
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Validate(src); err != nil {
		t.Fatal(err)
	}
	migratetest.CheckUpDownUp(t, testdb.DSN(t), src)
}
