package importing

import (
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
)

// ConfigDates preserves absence separately from an empty supplied calendar date.
type ConfigDates struct{ From, To *string }

type configDays struct{ from, to time.Time }

func parseConfigDates(input *ConfigDates) (*configDays, error) {
	if input == nil || input.From == nil && input.To == nil {
		return nil, nil
	}
	out := &configDays{}
	for _, field := range []struct {
		name string
		raw  *string
		at   *time.Time
	}{{"period_from", input.From, &out.from}, {"period_to", input.To, &out.to}} {
		if field.raw == nil {
			continue
		}
		at, err := time.Parse("2006-01-02", *field.raw)
		if err != nil || len(*field.raw) != 10 || at.Year() < 1 || at.Year() > 9999 || at.Format("2006-01-02") != *field.raw {
			return nil, &InputError{Field: field.name, Reason: "INVALID_FORMAT"}
		}
		*field.at = at
	}
	if input.From == nil {
		return nil, &InputError{Field: "period_from", Reason: "REQUIRED"}
	}
	if input.To == nil {
		return nil, &InputError{Field: "period_to", Reason: "REQUIRED"}
	}
	if out.from.After(out.to) {
		return nil, &InputError{Field: "period_from", Reason: "OUT_OF_RANGE"}
	}
	return out, nil
}

func (d configDays) bounds(zone *time.Location, today time.Time) (time.Time, time.Time, error) {
	zero := time.Time{}
	if d.to.Format("2006-01-02") > today.Format("2006-01-02") {
		return zero, zero, &InputError{Field: "period_to", Reason: "OUT_OF_RANGE"}
	}
	from := accounting.CivilDayStart(d.from, zone)
	to := accounting.CivilDayStart(d.to.AddDate(0, 0, 1), zone)
	if from.UTC().Year() < 1 || from.UTC().Year() > 9999 {
		return zero, zero, &InputError{Field: "period_from", Reason: "OUT_OF_RANGE"}
	}
	if to.UTC().Year() < 1 || to.UTC().Year() > 9999 || !from.Before(to) {
		return zero, zero, &InputError{Field: "period_to", Reason: "OUT_OF_RANGE"}
	}
	return from, to, nil
}
