package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/config"
)

func TestUnitAssemblyRejectsInvalidDailyOffsetBeforeOpeningResources(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 24 * time.Hour, 25 * time.Hour} {
		opened := false
		a, err := assemble(t.Context(), config.Config{DailyRecalculationOffset: offset}, slog.New(slog.DiscardHandler), prometheus.NewRegistry(), func(context.Context, postgres.Options) (*postgres.DB, error) { opened = true; return nil, nil })
		if a != nil || opened || err == nil || !strings.Contains(err.Error(), "DAILY_RECALCULATION_OFFSET") {
			t.Fatal("invalid daily offset reached resource assembly", offset, opened, err)
		}
	}
}
