package calculating

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func (c *calculator) reference(kind, key string) Reference {
	if c.in.References == nil {
		c.err = ErrCalculation
		return Reference{}
	}
	ref, err := c.in.References.Resolve(c.ctx, kind, key)
	if err != nil {
		c.err = err
	}
	if ref.ID == "" && err == nil {
		c.err = ErrCalculation
	}
	return ref
}
func median(values []int64) int64 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[len(values)/2]
}
func withinTolerance(value, center int64) bool {
	difference := new(big.Int).Sub(big.NewInt(value), big.NewInt(center))
	difference.Abs(difference)
	difference.Mul(difference, big.NewInt(100))
	limit := new(big.Int).Mul(big.NewInt(center), big.NewInt(accounting.RegularTolerancePercent))
	return difference.Cmp(limit) <= 0
}
func (c *calculator) regular(r Range) Metric {
	full := c.cal.before(r.To)
	threshold := accounting.Threshold("regular_payments").FullMonths
	if len(full) < threshold {
		return locked("need_full_months", threshold, len(full))
	}
	window := lastMonths(full, accounting.RegularWindow)
	keys := make(map[string]bool)
	for _, m := range window {
		keys[m.Format("2006-01")] = true
	}
	groups := make(map[string][]importing.Operation)
	for _, op := range c.cal.ops {
		_, cat, _, ok := FinancialRow(op, c.mode)
		if ok && op.Direction == "expense" && op.Description != "" && Observation(cat) && keys[op.At.In(c.cal.zone).Format("2006-01")] {
			groups[op.Description] = append(groups[op.Description], op)
		}
	}
	out := Metric{Status: "ready", Basis: basis(window)}
	if len(full) < accounting.RegularWindow {
		out.Status = "tentative"
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		group := groups[name]
		if len(group) != len(window) {
			continue
		}
		perMonth := make(map[string]int)
		var amounts, days []int64
		for _, op := range group {
			perMonth[op.At.In(c.cal.zone).Format("2006-01")]++
			amounts = append(amounts, op.AmountMinor)
			days = append(days, int64(op.At.In(c.cal.zone).Day()))
		}
		valid := true
		for k := range keys {
			if perMonth[k] != 1 {
				valid = false
			}
		}
		amount := median(amounts)
		for _, v := range amounts {
			if !withinTolerance(v, amount) {
				valid = false
			}
		}
		if !valid {
			continue
		}
		ref := c.reference("regular", name)
		if ref.Dismissed {
			continue
		}
		f := c.filter(r, "expense", "")
		f.Selection, f.Name = "s_regular_"+ref.ID, name
		// Category follows the most recent charge, not map iteration order.
		latest := group[0]
		for _, op := range group[1:] {
			if op.At.After(latest.At) {
				latest = op
			}
		}
		out.Payments = append(out.Payments, Payment{ID: ref.ID, Title: name, Category: latest.Category, Amount: amount, Day: int(median(days)), Filter: f})
		total = c.add(total, amount)
	}
	if len(out.Payments) == 0 {
		out.Status = "none"
		return out
	}
	sort.Slice(out.Payments, func(i, j int) bool {
		a, b := out.Payments[i], out.Payments[j]
		if a.Amount == b.Amount {
			return a.Title < b.Title
		}
		return a.Amount > b.Amount
	})
	f := c.filter(r, "expense", "")
	f.Selection, f.Name = "s_regular_all", "Подписки и регулярные платежи"
	out.Filter, out.Value = &f, &total
	return out
}
func (c *calculator) notable(r Range) Metric {
	full := c.cal.before(r.From)
	threshold := accounting.Threshold("notable_spending").FullMonths
	if len(full) < threshold {
		return locked("need_full_months", threshold, len(full))
	}
	window := lastMonths(full, threshold)
	out := Metric{Status: "ready", Basis: basis(window)}
	current := c.categories(r, "expense")
	for _, cat := range current.Categories {
		if !Observation(cat.ID) || cat.ID == importing.UncategorizedCategory {
			continue
		}
		var total int64
		for _, m := range window {
			total = c.add(total, c.sum(Range{m, m.AddDate(0, 1, 0)}, "expense", cat.ID))
		}
		typical := c.scale(total, 1, int64(threshold), true)
		// Compare without float rounding or overflowing a cross multiplication.
		left := new(big.Int).Mul(big.NewInt(cat.Amount), big.NewInt(accounting.NotableDenominator))
		right := new(big.Int).Mul(big.NewInt(typical), big.NewInt(accounting.NotableNumerator))
		if typical <= 0 || cat.Amount < accounting.NotableMinimumMinor || left.Cmp(right) < 0 {
			continue
		}
		out.Notable = append(out.Notable, Notable{ID: cat.ID, Name: cat.Name, Icon: cat.Icon, Amount: cat.Amount, Typical: typical, Filter: cat.Filter})
	}
	if len(out.Notable) == 0 {
		out.Status = "none"
		return out
	}
	sort.Slice(out.Notable, func(i, j int) bool {
		a, b := out.Notable[i], out.Notable[j]
		da, db := a.Amount-a.Typical, b.Amount-b.Typical
		if da == db {
			return a.Name < b.Name
		}
		return da > db
	})
	return out
}
func (c *calculator) fees(r Range) Metric {
	var total int64
	var names []string
	seen := make(map[string]bool)
	count := 0
	ops := append([]importing.Operation(nil), c.cal.ops...)
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].At.Equal(ops[j].At) {
			return ops[i].ID < ops[j].ID
		}
		return ops[i].At.Before(ops[j].At)
	})
	for _, op := range ops {
		kind, cat, amount, ok := FinancialRow(op, c.mode)
		if !ok || kind != "expense" || op.At.Before(r.From) || !op.At.Before(r.To) || !Fee(op, cat) {
			continue
		}
		total = c.add(total, amount)
		count++
		if op.Description != "" && !seen[op.Description] && len(names) < 3 {
			seen[op.Description] = true
			names = append(names, op.Description)
		}
	}
	if count == 0 || total <= 0 {
		return none()
	}
	f := c.filter(r, "expense", "")
	f.Selection, f.Name = "s_bank_fees", "Комиссии и проценты банку"
	out := ready(total, &f)
	if len(names) > 0 {
		s := strings.Join(names, ", ")
		out.FeeKinds = &s
	}
	return out
}
func (c *calculator) small(r Range) Metric {
	full := c.cal.before(r.To)
	threshold := accounting.Threshold("small_frequent").FullMonths
	if len(full) < threshold {
		return locked("need_full_months", threshold, len(full))
	}
	selected := full[len(full)-1]
	mr := Range{selected, selected.AddDate(0, 1, 0)}
	out := Metric{Status: "ready"}
	if !selected.Equal(r.From) {
		out.Basis = basis([]time.Time{selected})
	}
	merchants := make(map[string][]importing.Operation)
	var eligible []importing.Operation
	for _, op := range c.cal.ops {
		_, cat, _, ok := FinancialRow(op, c.mode)
		if ok && op.Direction == "expense" && Observation(cat) && !op.At.Before(mr.From) && op.At.Before(mr.To) {
			eligible = append(eligible, op)
			if op.Description != "" {
				merchants[op.Description] = append(merchants[op.Description], op)
			}
		}
	}
	used := make(map[string]bool)
	names := make([]string, 0, len(merchants))
	for name := range merchants {
		names = append(names, name)
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		group := merchants[name]
		amount, ok := c.smallAmount(group)
		if !ok {
			continue
		}
		used[name] = true
		ref := c.reference("merchant", name)
		f := c.filter(mr, "expense", "")
		f.Selection, f.Name = "s_merchant_"+ref.ID, name
		out.Small = append(out.Small, c.smallGroup(name, group, amount, f))
		total = c.add(total, amount)
	}
	categories := make(map[string][]importing.Operation)
	for _, op := range eligible {
		if used[op.Description] || op.Category == importing.UncategorizedCategory {
			continue
		}
		categories[op.Category] = append(categories[op.Category], op)
	}
	ids := make([]string, 0, len(categories))
	for id := range categories {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		group := categories[id]
		amount, ok := c.smallAmount(group)
		if !ok {
			continue
		}
		def, found := c.in.Snapshot.Categories[id]
		if !found {
			c.err = ErrCalculation
			return none()
		}
		f := c.filter(mr, "expense", id)
		f.Selection, f.Name = "s_small_category_"+id, def.Name
		out.Small = append(out.Small, c.smallGroup(def.Name, group, amount, f))
		total = c.add(total, amount)
	}
	if len(out.Small) == 0 {
		out.Status = "none"
		return out
	}
	sort.Slice(out.Small, func(i, j int) bool {
		a, b := out.Small[i], out.Small[j]
		if a.Amount == b.Amount {
			return a.Title < b.Title
		}
		return a.Amount > b.Amount
	})
	out.Value = &total
	return out
}
func (c *calculator) smallAmount(group []importing.Operation) (int64, bool) {
	if len(group) < accounting.SmallMinimumCount {
		return 0, false
	}
	var amount int64
	for _, op := range group {
		amount = c.add(amount, op.AmountMinor)
	}
	return amount, amount/int64(len(group)) <= accounting.SmallMaximumAverageMinor
}
func (c *calculator) smallGroup(name string, group []importing.Operation, amount int64, f Filter) Small {
	return Small{Title: name, Count: len(group), Amount: amount, Average: amount / int64(len(group)), Yearly: c.scale(amount, 12, 1, false), Filter: f}
}

// ScheduleName renders only the calendar day; financial formulas stay numeric.
func ScheduleName(day int) string { return fmt.Sprintf("%d-го числа", day) }
