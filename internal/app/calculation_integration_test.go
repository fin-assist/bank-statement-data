package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/adapter/metricswire"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type calculationReference struct {
	Source, Kind, Key, Mode, Coverage string
	Tiles                             map[string]int64
	Categories                        map[string][]struct {
		Name   string
		Amount int64
	}
}

func calculationDate(t *testing.T, key string) time.Time {
	t.Helper()
	layout := "2006-01"
	switch {
	case strings.Contains(key, "Q"):
		var year, quarter int
		if _, err := fmt.Sscanf(key, "%d-Q%d", &year, &quarter); err != nil {
			t.Fatal(err)
		}
		zone, _ := time.LoadLocation("Europe/Moscow")
		return time.Date(year, time.Month((quarter-1)*3+1), 1, 0, 0, 0, 0, zone)
	case len(key) == 4:
		layout = "2006"
	}
	zone, _ := time.LoadLocation("Europe/Moscow")
	at, err := time.ParseInLocation(layout, key, zone)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
func calculationNow() time.Time {
	return time.Date(2026, 9, 26, 10, 30, 0, 0, time.FixedZone("MSK", 3*3600))
}
func calculationChecks(t *testing.T, s *calculating.Service) {
	t.Helper()
	raw, err := os.ReadFile("testdata/calculation-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []calculationReference
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Source, func(t *testing.T) {
			result, err := s.Calculate(t.Context(), apiUser, tc.Kind, calculationDate(t, tc.Key))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Periods) != 2 {
				t.Fatal("mode pair not atomic")
			}
			p := result.Periods[0]
			if tc.Mode == "without" {
				p = result.Periods[1]
			}
			if p.Key != tc.Key || p.Bounds.Coverage != tc.Coverage {
				t.Fatalf("period %+v", p.Bounds)
			}
			for id, want := range tc.Tiles {
				m := p.Metrics["tiles."+id]
				if m.Value == nil || *m.Value != want {
					t.Errorf("%s: got %+v want %d", id, m.Value, want)
				}
			}
			for id, wants := range tc.Categories {
				got := p.Metrics[id].Categories
				if len(got) != len(wants) {
					t.Errorf("%s: got %d categories want %d", id, len(got), len(wants))
					continue
				}
				for i, w := range wants {
					if got[i].Name != w.Name || got[i].Amount != w.Amount {
						t.Errorf("%s[%d]: got %s %d want %s %d", id, i, got[i].Name, got[i].Amount, w.Name, w.Amount)
					}
				}
			}
			state, periods, err := metricswire.Encode(result)
			if err != nil {
				t.Fatal(err)
			}
			if state.TimeZone != "Europe/Moscow" || len(periods) != 2 || state.StaleAfter == nil {
				t.Fatal("typed state/modes missing")
			}
			for _, wire := range periods {
				bytes, err := proto.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				clone := proto.Clone(wire)
				if err = proto.Unmarshal(bytes, clone); err != nil || !proto.Equal(clone, wire) {
					t.Fatal("ingest typed snapshot changed", err)
				}
			}
		})
	}
}
func TestIntegrationCalculationFromCommittedFixtureHistory(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	for _, file := range []string{"00-2025-10-15_2025-10-31.ofx", "01-2025-10-15_2026-06-30.ofx", "02-2026-06-20_2026-08-07.ofx", "03-2026-08-20_2026-09-25.ofx", "04-edge-cases-2026-09.ofx"} {
		importFile(t, a, file)
	}
	calculationChecks(t, a.Calculations)
	first, err := a.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	regular := first.Periods[0].Metrics["insights.regular_payments"].Payments
	if len(regular) == 0 {
		t.Fatal("fixture regular missing")
	}
	id := regular[0].ID
	if _, err = a.DB.Primary().Exec(t.Context(), `UPDATE regular_payment SET dismissed_at=$3 WHERE user_id=$1 AND id=$2`, apiUser, id, calculationNow()); err != nil {
		t.Fatal(err)
	}
	second, err := a.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range second.Periods {
		for _, payment := range p.Metrics["insights.regular_payments"].Payments {
			if payment.ID == id {
				t.Fatal("dismissal ignored")
			}
		}
	}
	if _, err = a.Calculations.Calculate(t.Context(), apiOther, "month", calculationDate(t, "2026-09")); !errors.Is(err, importing.ErrNotFound) {
		t.Fatal("missing user read", err)
	}
	if err = a.Imports.Purge(t.Context(), apiUser); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-09")); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("deleted user re-created", err)
	}
	var rows int
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM regular_payment WHERE user_id=$1`, apiUser).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("references survived purge", rows, err)
	}
}
func TestIntegrationCalculationReferencesAreStableAndTenantScoped(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importFile(t, a, "01-2025-10-15_2026-06-30.ofx")
	var first, second calculating.Result
	for i := 0; i < 2; i++ {
		result, err := a.Calculations.Calculate(t.Context(), apiUser, "month", calculationDate(t, "2026-06"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = result
		} else {
			second = result
		}
	}
	_, one, err := metricswire.Encode(first)
	if err != nil {
		t.Fatal(err)
	}
	_, two, err := metricswire.Encode(second)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(one[0], two[0]) || !proto.Equal(one[1], two[1]) {
		t.Fatal("recalculation changed identities or content")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Calculations.Calculate(ctx, apiUser, "month", calculationDate(t, "2026-06")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled computation", err)
	}
}
