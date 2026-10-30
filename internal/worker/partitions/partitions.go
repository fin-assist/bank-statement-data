// Package partitions maintains the UTC yearly operation partitions.
package partitions

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/fin-assist/go-worker"
	"github.com/prometheus/client_golang/prometheus"
)

type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

type Store interface {
	Ensure(context.Context, int) error
	Exists(context.Context, int) (bool, error)
}

type maintenance struct {
	db      Transactions
	store   Store
	now     func() time.Time
	missing *prometheus.GaugeVec
}

// New checks immediately and weekly; each iteration has its own bounded transaction.
func New(db Transactions, store Store, now func() time.Time, clock worker.Clock, log *slog.Logger, reg prometheus.Registerer) (worker.Job, error) {
	if db == nil || store == nil || now == nil || clock == nil || log == nil || reg == nil {
		return worker.Job{}, errors.New("partition maintenance dependencies required")
	}
	missing := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "finassist_operation_partition_missing",
		Help: "Required operation partition is missing or could not be verified; next year is required within 30 days of UTC January 1.",
	}, []string{"period"})
	if err := reg.Register(missing); err != nil {
		return worker.Job{}, errors.New("partition maintenance metrics registration failed")
	}
	missing.WithLabelValues("current").Set(0)
	missing.WithLabelValues("next").Set(0)
	m := maintenance{db: db, store: store, now: now, missing: missing}
	return worker.Every("operation-partitions", 7*24*time.Hour, log, clock, m.check)
}

func (m maintenance) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	now := m.now().UTC()
	year := now.Year()
	if year < 2010 || year > 9997 {
		m.missing.WithLabelValues("current").Set(1)
		m.missing.WithLabelValues("next").Set(1)
		return errors.New("partition maintenance clock outside supported range")
	}
	err := m.db.Run(ctx, func(ctx context.Context) error {
		if err := m.store.Ensure(ctx, year); err != nil {
			return err
		}
		if now.Month() >= time.October {
			return m.store.Ensure(ctx, year+1)
		}
		return nil
	})
	// Read after COMMIT/ROLLBACK: failed creation must never report a ready partition.
	current, currentErr := m.store.Exists(ctx, year)
	next, nextErr := m.store.Exists(ctx, year+1)
	m.missing.WithLabelValues("current").Set(missingValue(!current || currentErr != nil))
	deadline := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)
	m.missing.WithLabelValues("next").Set(missingValue(deadline.Sub(now) < 30*24*time.Hour && (!next || nextErr != nil)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || currentErr != nil || nextErr != nil {
		return errors.New("operation partition maintenance failed")
	}
	return nil
}

func missingValue(missing bool) float64 {
	if missing {
		return 1
	}
	return 0
}
