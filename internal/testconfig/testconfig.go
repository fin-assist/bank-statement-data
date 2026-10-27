// Package testconfig supplies fixture defaults from the runtime declarations.
package testconfig

import (
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/config"
)

// DefaultDailyRecalculationOffset parses the canonical default without reading
// the process environment or introducing a separate fixture default.
func DefaultDailyRecalculationOffset(t testing.TB) time.Duration {
	t.Helper()
	for _, variable := range config.Vars {
		if variable.Name == "DAILY_RECALCULATION_OFFSET" {
			offset, err := time.ParseDuration(variable.Default)
			if err != nil {
				t.Fatalf("invalid declared daily recalculation default: %v", err)
			}
			return offset
		}
	}
	t.Fatal("daily recalculation offset missing from config declarations")
	return 0
}
