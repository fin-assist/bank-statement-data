package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type backfillStatsFake struct{ err error }

func (f backfillStatsFake) BackfillStats(context.Context, time.Time) (int64, int64, int64, float64, error) {
	return 3, 16, 2, 45, f.err
}

func TestUnitBackfillMetricsPreserveLastObservationOnStorageFailure(t *testing.T) {
	m := newBackfillMetrics()
	reg := prometheus.NewRegistry()
	for _, gauge := range []prometheus.Gauge{m.pending, m.planned, m.failed, m.age} {
		reg.MustRegister(gauge)
	}
	m.observe(t.Context(), backfillStatsFake{}, time.Now())
	m.observe(t.Context(), backfillStatsFake{err: errors.New("unavailable")}, time.Now())
	metrics, err := reg.Gather()
	if err != nil || len(metrics) != 4 {
		t.Fatal(metrics, err)
	}
	want := map[string]float64{
		"finassist_definition_backfill_pending_users":      3,
		"finassist_definition_backfill_planned_users":      16,
		"finassist_definition_backfill_failed_users":       2,
		"finassist_definition_backfill_oldest_age_seconds": 45,
	}
	for _, family := range metrics {
		if len(family.Metric) != 1 || len(family.Metric[0].Label) != 0 || family.Metric[0].Gauge.GetValue() != want[family.GetName()] {
			t.Fatal("incorrect or privately labelled backfill observation", family)
		}
	}
}
