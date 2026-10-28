package app

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type backfillStatistics interface {
	BackfillStats(context.Context, time.Time) (int64, int64, int64, float64, error)
}

type backfillMetrics struct{ pending, planned, failed, age prometheus.Gauge }

func newBackfillMetrics() backfillMetrics {
	return backfillMetrics{
		pending: prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_definition_backfill_pending_users", Help: "Users with queued registry-only historical recalculation."}),
		planned: prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_definition_backfill_planned_users", Help: "Users scanned in the current registry release."}),
		failed:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_definition_backfill_failed_users", Help: "Users with failed registry-only recalculation attempts."}),
		age:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_definition_backfill_oldest_age_seconds", Help: "Age of the oldest registry-only queue entry."}),
	}
}

func (m backfillMetrics) observe(ctx context.Context, repo backfillStatistics, now time.Time) {
	pending, planned, failed, age, err := repo.BackfillStats(ctx, now)
	if err == nil {
		m.pending.Set(float64(pending))
		m.planned.Set(float64(planned))
		m.failed.Set(float64(failed))
		m.age.Set(age)
	}
}
