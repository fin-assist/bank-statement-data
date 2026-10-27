package accounting

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUnitCalendarSkipsMissingCivilDateAndHonorsCancellation(t *testing.T) {
	zone, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2011, 12, 1, 0, 0, 0, 0, zone)
	to := time.Date(2012, 1, 1, 0, 0, 0, 0, zone)
	var days []int
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = WalkDays(ctx, from, to, func(at time.Time) error { days = append(days, at.Day()); return nil })
	if err != nil || len(days) != 30 || days[28] != 29 || days[29] != 31 {
		t.Fatal(days, err)
	}
	dec29 := time.Date(2011, 12, 29, 0, 0, 0, 0, zone)
	if next := AddDays(dec29, 1); next.Day() != 31 || !next.After(dec29) {
		t.Fatal("date cursor did not advance", next)
	}
	if previous := AddDays(time.Date(2011, 12, 31, 0, 0, 0, 0, zone), -1); previous.Day() != 29 {
		t.Fatal("previous existing date", previous)
	}
	ctx, stop := context.WithCancel(t.Context())
	visits := 0
	err = WalkDays(ctx, from, to, func(time.Time) error { visits++; stop(); return nil })
	if !errors.Is(err, context.Canceled) || visits != 1 {
		t.Fatal("day iteration ignored cancellation", visits, err)
	}
	boom := errors.New("stop")
	if err = WalkDays(t.Context(), from, to, func(time.Time) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
func TestUnitCalendarFindsFirstInstantAtMidnightGapAndDSTDays(t *testing.T) {
	zone, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	noon := time.Date(2018, 11, 4, 12, 0, 0, 0, zone)
	first := StartOfDay(noon, zone)
	if first.Day() != 4 || first.Hour() != 1 || first.In(zone).Month() != time.November {
		t.Fatal("missing midnight", first)
	}
	zone, err = time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 3, 28, 0, 0, 0, 0, zone)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, zone)
	count := 0
	if err = WalkDays(t.Context(), from, to, func(time.Time) error { count++; return nil }); err != nil || count != 3 || to.Sub(from) != 71*time.Hour {
		t.Fatal("DST calendar", count, err)
	}
}

func TestUnitMonthStartPreservesCivilMonthAtMidnightGap(t *testing.T) {
	zone, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Fatal(err)
	}
	april := MonthStart(time.Date(2012, 4, 15, 12, 0, 0, 0, zone), 0)
	if april.Month() != time.April || april.Day() != 1 || april.Hour() != 1 {
		t.Fatal("month selected previous civil date", april)
	}
	may := MonthStart(april, 1)
	if may.Month() != time.May || may.Day() != 1 || may.Hour() != 0 {
		t.Fatal("shift carried the gap hour to next month", may)
	}
}
