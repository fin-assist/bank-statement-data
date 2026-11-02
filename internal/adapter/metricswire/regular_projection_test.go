package metricswire

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/fin-assist/contracts/gen/go/finassist/analytics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/bank-statement-data/internal/calculating"
)

func regularWireFixture() calculating.RegularProjection {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	last := to.Add(-time.Second)
	r := calculating.Range{From: from, To: to}
	sum := int64(10000)
	f := calculating.Filter{Range: r, Mode: "with", Kind: "expense", Selection: "s_regular_all", Name: "Subscriptions"}
	p := calculating.RegularProjection{User: "00000000-0000-4000-8000-000000000001", Currency: "RUB", TimeZone: "UTC", Params: calculating.RegularParams{Month: "2026-09", Mode: "with"}, Range: r, DataRange: r, Bounds: calculating.Bounds{Coverage: "complete", From: &from, To: &last}, Metric: calculating.Metric{DefinitionVersion: 1, Status: "ready", Value: &sum, Filter: &f, Basis: &calculating.Basis{Range: r, FullMonths: 3}}}
	for i := range 4 {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+2)
		itemFilter := f
		itemFilter.Selection = "s_regular_" + id
		p.Metric.Payments = append(p.Metric.Payments, calculating.Payment{ID: id, Title: fmt.Sprint(i), Category: "cat_food", Amount: int64(i+1) * 1000, Day: 15, Filter: itemFilter})
	}
	return p
}

func TestUnitRegularProjectionWirePreservesFullListAndLimits(t *testing.T) {
	p := regularWireFixture()
	wire, err := EncodeRegularProjection(p)
	if err != nil {
		t.Fatal(err)
	}
	m := wire.Metrics[0].Metric
	if len(wire.Metrics) != 1 || m.Status != pb.MetricStatus_METRIC_STATUS_READY || m.GetValueMinor() != 10000 || m.GetRegularPayments().GetCount() != 4 || len(m.GetRegularPayments().Items) != 4 || m.GetRegularPayments().Items[3].MonthlyAmountMinor != 4000 || wire.UserId != p.User {
		t.Fatal(wire)
	}
	for _, status := range []string{"none", "locked", "tentative"} {
		p = regularWireFixture()
		p.Metric.Status = status
		if status != "tentative" {
			p.Metric.Payments = nil
			p.Metric.Value = nil
			p.Metric.Filter = nil
		}
		if status == "locked" {
			p.Metric.Basis = nil
			p.Metric.Lock = &calculating.Lock{Reason: "need_full_months", Required: 2}
		}
		if err = ValidateRegularProjection(p); err != nil {
			t.Fatal(status, err)
		}
	}
	// The exact boundary is measured by protobuf, not title bytes or item count.
	low, high := 0, ProjectionLimit
	for low < high {
		n := (low + high + 1) / 2
		p = regularWireFixture()
		p.Metric.Payments[0].Title = strings.Repeat("x", n)
		_, err = EncodeRegularProjection(p)
		if errors.Is(err, ErrProjectionSize) {
			high = n - 1
		} else if err != nil {
			t.Fatal(err)
		} else {
			low = n
		}
	}
	p = regularWireFixture()
	p.Metric.Payments[0].Title = strings.Repeat("x", low)
	wire, err = EncodeRegularProjection(p)
	if err != nil || proto.Size(wire) != ProjectionLimit {
		t.Fatal(proto.Size(wire), err)
	}
	p.Metric.Payments[0].Title += "x"
	if _, err = EncodeRegularProjection(p); !errors.Is(err, ErrProjectionSize) {
		t.Fatal(err)
	}
}

func TestUnitRegularProjectionWireRejectsMalformedSource(t *testing.T) {
	for name, change := range map[string]func(*calculating.RegularProjection){
		"user":            func(p *calculating.RegularProjection) { p.User = "foreign" },
		"currency":        func(p *calculating.RegularProjection) { p.Currency = "rub" },
		"zone":            func(p *calculating.RegularProjection) { p.TimeZone = "Local" },
		"unknown zone":    func(p *calculating.RegularProjection) { p.TimeZone = "Area/Future_City" },
		"month":           func(p *calculating.RegularProjection) { p.Params.Month = "2026-9" },
		"mode":            func(p *calculating.RegularProjection) { p.Params.Mode = "unknown" },
		"definition":      func(p *calculating.RegularProjection) { p.Metric.DefinitionVersion = 0 },
		"missing value":   func(p *calculating.RegularProjection) { p.Metric.Value = nil },
		"missing items":   func(p *calculating.RegularProjection) { p.Metric.Payments = nil },
		"duplicate id":    func(p *calculating.RegularProjection) { p.Metric.Payments[1] = p.Metric.Payments[0] },
		"bad filter":      func(p *calculating.RegularProjection) { p.Metric.Payments[0].Filter.Selection = "s_regular_other" },
		"unknown status":  func(p *calculating.RegularProjection) { p.Metric.Status = "unavailable" },
		"none with value": func(p *calculating.RegularProjection) { p.Metric.Status = "none" },
		"range":           func(p *calculating.RegularProjection) { p.Range.To = p.Range.From },
		"bounds":          func(p *calculating.RegularProjection) { p.Bounds.From = nil },
		"encoding":        func(p *calculating.RegularProjection) { p.Metric.Payments[0].Title = "\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			p := regularWireFixture()
			change(&p)
			if err := ValidateRegularProjection(p); !errors.Is(err, calculating.ErrCalculation) {
				t.Fatal(err)
			}
		})
	}
}
