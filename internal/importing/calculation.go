package importing

import (
	"context"
	"time"
)

// CompletedPeriods exposes the same completed-upload coverage used by import
// history, including source membership fallback for rows without tagged bounds.
func CompletedPeriods(ctx context.Context, state Snapshot, zone *time.Location) ([]Period, error) {
	if zone == nil {
		return nil, errAccounting
	}
	if _, err := readUploadIndex(state.Uploads); err != nil {
		return nil, err
	}
	var out []Period
	for _, u := range state.Uploads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if u.Status != "done" {
			continue
		}
		p, err := readUploadPeriod(ctx, u, state, zone)
		if err != nil {
			return nil, err
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}
