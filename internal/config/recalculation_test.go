package config

import (
	"strings"
	"testing"
	"time"
)

func TestUnitDailyRecalculationOffsetEnvironmentValues(t *testing.T) {
	for _, test := range []struct {
		name, value string
		present     bool
		want        time.Duration
	}{
		{"unset default", "", false, 5 * time.Minute},
		{"empty default", "", true, 5 * time.Minute},
		{"configured ten minutes", "10m", true, 10 * time.Minute},
		{"explicit zero without unit", "0", true, 0},
		{"explicit zero duration", "0s", true, 0},
		{"last allowed nanosecond", "23h59m59.999999999s", true, 24*time.Hour - time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := base()
			if test.present {
				values["DAILY_RECALCULATION_OFFSET"] = test.value
			}
			get, list := env(values)
			cfg, err := load(get, noFiles, list)
			if err != nil || cfg.DailyRecalculationOffset != test.want {
				t.Fatalf("configured offset %q present%v got%s want%s err%v", test.value, test.present, cfg.DailyRecalculationOffset, test.want, err)
			}
		})
	}
}

func TestUnitDailyRecalculationOffsetRejectsMalformedAndOutOfRange(t *testing.T) {
	for _, value := range []string{"not-a-duration", "5", "1e3s", "-1ns", "24h", "24h1ns", "48h"} {
		t.Run(value, func(t *testing.T) {
			values := base()
			values["DAILY_RECALCULATION_OFFSET"] = value
			get, list := env(values)
			_, err := load(get, noFiles, list)
			if err == nil || !strings.Contains(err.Error(), "DAILY_RECALCULATION_OFFSET") {
				t.Fatalf("invalid offset %q accepted or error omitted variable name: %v", value, err)
			}
		})
	}
}

func TestUnitDailyRecalculationOffsetSharedRangeGuard(t *testing.T) {
	for _, value := range []time.Duration{0, 10 * time.Minute, 24*time.Hour - time.Nanosecond} {
		if err := ValidateDailyRecalculationOffset(value); err != nil {
			t.Fatalf("valid offset %s: %v", value, err)
		}
	}
	for _, value := range []time.Duration{-time.Nanosecond, 24 * time.Hour, 48 * time.Hour} {
		if err := ValidateDailyRecalculationOffset(value); err == nil || !strings.Contains(err.Error(), "DAILY_RECALCULATION_OFFSET") {
			t.Fatalf("out-of-range offset %s: %v", value, err)
		}
	}
}
