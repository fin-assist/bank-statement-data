package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

// Dismissed reads the owned assignment after the usecase acquired the user lock.
func (r *Operations) Dismissed(ctx context.Context, user, id string) (bool, error) {
	if _, err := r.transaction(ctx); err != nil {
		return false, err
	}
	var dismissed bool
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT dismissed_at IS NOT NULL FROM regular_payment WHERE user_id=$1 AND id=$2::uuid`, user, id).Scan(&dismissed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, operations.ErrNotFound
	}
	return dismissed, importError(ctx, err)
}

// RegularSnapshot loads source coverage and at most four distinct calendar months.
// The user lock keeps metadata, operations and subsequent reference reads coherent.
func (r *Operations) RegularSnapshot(ctx context.Context, user string, now time.Time, params calculating.RegularParams) (importing.Snapshot, error) {
	if _, err := r.transaction(ctx); err != nil {
		return importing.Snapshot{}, err
	}
	empty := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	state, err := r.snapshot(ctx, user, &empty, &empty, false)
	if err != nil {
		return state, err
	}
	ranges, err := calculating.RegularRanges(ctx, state, now, params)
	if err != nil {
		return state, err
	}
	seen := make(map[string]bool)
	for _, period := range ranges {
		rows, err := r.operationsRange(ctx, user, &period.From, &period.To)
		if err != nil {
			return importing.Snapshot{}, err
		}
		for _, row := range rows {
			if !seen[row.ID] {
				seen[row.ID] = true
				state.Operations = append(state.Operations, row)
			}
		}
	}
	return state, nil
}
