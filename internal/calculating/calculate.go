package calculating

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

type calculator struct {
	ctx  context.Context
	in   Input
	cal  calendar
	mode string
	err  error
}

// Prepared retains one validated coherent snapshot, calendar and chronological
// operation index. The caller keeps the source snapshot immutable while building
// its periods, and owns the transaction used by the reference resolver.
type Prepared struct {
	ctx   context.Context
	in    Input
	cal   calendar
	state State
}

// Prepare validates source facts and builds their shared calendar once per batch.
func Prepare(ctx context.Context, in Input) (*Prepared, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cal, err := makeCalendar(ctx, in)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, op := range cal.ops {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if op.ID == "" || seen[op.ID] || op.At.IsZero() || op.AmountMinor < 0 || op.Category == "" || op.Direction != "expense" && op.Direction != "income" || op.Category == importing.RefundCategory && op.RefundTarget == "" {
			return nil, ErrCalculation
		}
		seen[op.ID] = true
	}
	return &Prepared{ctx: ctx, in: in, cal: cal, state: cal.state(in.Now)}, nil
}

// Build computes one exact period in both modes from this immutable batch input.
func (p *Prepared) Build(kind string, at time.Time) (Result, error) {
	if p == nil || at.IsZero() {
		return Result{}, ErrCalculation
	}
	if err := p.ctx.Err(); err != nil {
		return Result{}, err
	}
	in := p.in
	in.Kind = kind
	in.At = at
	builder, ok := periodBuilders[kind]
	if !ok {
		return Result{}, ErrCalculation
	}
	r, err := builder.bounds(p.cal, at.In(p.cal.zone))
	if err != nil {
		return Result{}, err
	}
	out := Result{State: p.state}
	for _, mode := range []string{"with", "without"} {
		c := calculator{ctx: p.ctx, in: in, cal: p.cal, mode: mode}
		period := builder.build(&c, r)
		if c.err != nil {
			return Result{}, c.err
		}
		out.Periods = append(out.Periods, period)
	}
	if err := p.ctx.Err(); err != nil {
		return Result{}, err
	}
	return out, nil
}

// Build computes one exact period in both modes; no saved period is read. A caller
// building a calendar uses Prepare once and Prepared.Build for each period.
func Build(ctx context.Context, in Input) (Result, error) {
	prepared, err := Prepare(ctx, in)
	if err != nil {
		return Result{}, err
	}
	return prepared.Build(in.Kind, in.At)
}
func (c *calculator) add(a, b int64) int64 {
	n := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
	if !n.IsInt64() {
		c.err = ErrCalculation
		return 0
	}
	return n.Int64()
}
func (c *calculator) scale(value, mult, div int64, round bool) int64 {
	if div <= 0 {
		c.err = ErrCalculation
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(value), big.NewInt(mult))
	d := big.NewInt(div)
	if round {
		n.Mul(n, big.NewInt(2))
		n.Add(n, d)
		d.Mul(d, big.NewInt(2))
		n.Div(n, d)
	} else {
		n.Quo(n, d)
	}
	if !n.IsInt64() {
		c.err = ErrCalculation
		return 0
	}
	return n.Int64()
}
func (c *calculator) sum(r Range, kind, category string) int64 {
	var sum int64
	for _, op := range c.cal.operations(r) {
		if err := c.ctx.Err(); err != nil {
			c.err = err
			return 0
		}
		k, cat, amount, ok := FinancialRow(op, c.mode)
		if ok && k == kind && (category == "" || category == cat) && !op.At.Before(r.From) && op.At.Before(r.To) {
			sum = c.add(sum, amount)
		}
	}
	return sum
}
func (c *calculator) filter(r Range, kind, category string) Filter {
	return Filter{Range: r, Mode: c.mode, Kind: kind, Category: category}
}
func ready(value int64, filter *Filter) Metric {
	return Metric{Status: "ready", Value: &value, Filter: filter}
}
func locked(reason string, required, available int) Metric {
	return Metric{Status: "locked", Lock: &Lock{Reason: reason, Required: required, Available: available}}
}
func none() Metric { return Metric{Status: "none"} }
func (c *calculator) value(r Range, kind string) Metric {
	f := c.filter(r, kind, "")
	return ready(c.sum(r, kind, ""), &f)
}
func periodMonths(kind string) int {
	switch kind {
	case "quarter":
		return 3
	case "year":
		return 12
	default:
		return 1
	}
}
func (c *calculator) facts(r Range) Period {
	p := Period{Kind: c.in.Kind, Key: key(r, c.in.Kind), Mode: c.mode, Range: r, Bounds: c.cal.bounds(r), CalculatedAt: c.in.Now, Metrics: make(map[string]Metric)}
	index := make(map[string]bool)
	for _, u := range c.in.Snapshot.Uploads {
		index[u.ID] = u.Status == "done"
	}
	for _, line := range c.in.Snapshot.Unread {
		if index[line.UploadID] && line.ResolvedByUpload == "" && line.OccurredAt != nil && !line.OccurredAt.Before(r.From) && line.OccurredAt.Before(r.To) {
			p.Unread++
		}
	}
	m := p.Metrics
	if p.Bounds.From == nil { // NONE carries no zero for an uncovered period.
		for _, id := range []string{"tiles.expense", "tiles.income", "tiles.balance", "tiles.daily_expense", "expense_categories", "income_categories"} {
			m[id] = none()
		}
	} else {
		m["tiles.expense"], m["tiles.income"] = c.value(r, "expense"), c.value(r, "income")
		expense, income := *m["tiles.expense"].Value, *m["tiles.income"].Value
		balance := ready(c.add(income, c.scale(expense, -1, 1, false)), nil)
		if income > 0 {
			share := float64(*balance.Value) / float64(income)
			balance.Share = &share
		}
		m["tiles.balance"] = balance
		days := 0
		if err := accounting.WalkDays(c.ctx, r.From, minTime(r.To, nextDay(c.cal.now)), func(d time.Time) error {
			if c.cal.covered(d) {
				days++
			}
			return nil
		}); err != nil {
			c.err = err
		}
		// calc.md 1: dividing by days truncates toward zero even for net refunds.
		m["tiles.daily_expense"] = ready(expense/int64(max(1, days)), nil)
		m["expense_categories"], m["income_categories"] = c.categories(r, "expense"), c.categories(r, "income")
	}
	return p
}

func (c *calculator) period(r Range) Period {
	p := c.facts(r)
	m := p.Metrics
	c.historical(m, r, p.Bounds)
	m["monthly_chart"] = c.chart(r)
	if c.in.Kind == "month" {
		m["insights.regular_payments"] = c.regular(r)
		m["insights.notable_spending"] = c.notable(r)
		m["insights.bank_fees"] = c.fees(r)
		m["insights.small_frequent"] = c.small(r)
	}
	return c.definitions(p)
}

func (c *calculator) definitions(p Period) Period {
	for id, metric := range p.Metrics {
		definition, ok := accounting.DefinitionFor(id)
		if !ok || definition.Version < 1 {
			c.err = ErrCalculation
			return p
		}
		metric.DefinitionVersion = definition.Version
		p.Metrics[id] = metric
	}
	return p
}
func (c *calculator) categories(r Range, kind string) Metric {
	items := make(map[string]*CategoryAmount)
	var total int64
	for _, op := range c.cal.operations(r) {
		k, cat, amount, ok := FinancialRow(op, c.mode)
		if !ok || k != kind || op.At.Before(r.From) || !op.At.Before(r.To) {
			continue
		}
		v := items[cat]
		if v == nil {
			def, found := c.in.Snapshot.Categories[cat]
			if !found {
				c.err = ErrCalculation
				return none()
			}
			v = &CategoryAmount{ID: cat, Name: def.Name, Icon: def.Icon, Filter: c.filter(r, kind, cat)}
			items[cat] = v
		}
		v.Amount = c.add(v.Amount, amount)
		v.Count++
		total = c.add(total, amount)
	}
	if len(items) == 0 {
		return none()
	}
	out := Metric{Status: "ready"}
	for _, v := range items {
		if total > 0 && v.Amount >= 0 {
			share := float64(v.Amount) / float64(total)
			v.Share = &share
		}
		out.Categories = append(out.Categories, *v)
	}
	sort.Slice(out.Categories, func(i, j int) bool {
		a, b := out.Categories[i], out.Categories[j]
		if a.Amount == b.Amount {
			return a.Name < b.Name
		}
		return a.Amount > b.Amount
	})
	return out
}
func (c *calculator) historical(m map[string]Metric, r Range, b Bounds) {
	months := periodMonths(c.in.Kind)
	prev := Range{shiftMonths(r.From, -months), r.From}
	fullPrevious := c.cal.complete(prev)
	m["tiles.expense.comparison"] = locked("need_full_months", months, 0)
	m["tiles.daily_expense.comparison"] = locked("need_full_months", months, 0)
	if fullPrevious {
		cutoff := prev.To
		if b.To != nil {
			days := c.dates(Range{r.From, nextDay(day(*b.To, c.cal.zone))})
			cutoff = minTime(accounting.AddDays(prev.From, days), prev.To)
		}
		comparisonRange := Range{prev.From, cutoff}
		comparison := c.value(comparisonRange, "expense")
		comparison.Range = &comparisonRange
		m["tiles.expense.comparison"] = comparison
		m["tiles.daily_expense.comparison"] = ready(c.sum(prev, "expense", "")/int64(max(1, c.dates(prev))), nil)
	}
	var previous []Range
	if len(c.cal.months) > 0 {
		for at := shiftMonths(r.From, -months); !at.Before(month(c.cal.months[0])); at = shiftMonths(at, -months) {
			if err := c.ctx.Err(); err != nil {
				c.err = err
				return
			}
			if at.Year() < 1 {
				break
			}
			candidate := Range{at, shiftMonths(at, months)}
			if c.cal.complete(candidate) {
				previous = append(previous, candidate)
			}
			if len(previous) == accounting.TypicalWindow {
				break
			}
		}
	}
	required := accounting.Threshold("typical").FullMonths
	typical := locked("need_full_months", required*months, len(previous)*months)
	if len(previous) >= required {
		var sum int64
		for _, v := range previous {
			sum = c.add(sum, c.sum(v, "expense", ""))
		}
		typical = ready(c.scale(sum, 1, int64(len(previous)), true), nil)
		typical.Basis = &Basis{Range: Range{previous[len(previous)-1].From, previous[0].To}, FullMonths: len(previous) * months}
	}
	m["tiles.expense.typical"] = typical
	if c.cal.now.Before(r.From) || !c.cal.now.Before(r.To) {
		return
	}
	state := c.cal.state(c.in.Now)
	if state.StaleAfter == nil || !c.in.Now.Before(*state.StaleAfter) {
		return
	}
	if months == 1 && c.cal.now.Day() < accounting.ForecastDay {
		at := accounting.AddDays(r.From, accounting.ForecastDay-1)
		m["tiles.forecast"] = Metric{Status: "locked", Lock: &Lock{Reason: "too_early_in_month", AvailableFrom: &at}}
		return
	}
	if b.From == nil {
		m["tiles.forecast"] = none()
		return
	}
	expense := c.sum(r, "expense", "")
	if months == 1 {
		end := c.cal.now
		if b.To != nil {
			end = minTime(end, day(*b.To, c.cal.zone))
		}
		elapsed := max(1, c.dates(Range{r.From, nextDay(end)}))
		m["tiles.forecast"] = ready(c.scale(expense, int64(c.dates(r)), int64(elapsed), false), nil)
		return
	}
	base := lastMonths(c.cal.full, months)
	if len(base) < months {
		m["tiles.forecast"] = locked("need_full_months", months, len(base))
		return
	}
	var baseSum int64
	var baseDays int
	for _, v := range base {
		br := Range{v, shiftMonths(v, 1)}
		baseSum = c.add(baseSum, c.sum(br, "expense", ""))
		baseDays += c.dates(br)
	}
	start := r.From
	if b.To != nil {
		start = nextDay(day(*b.To, c.cal.zone))
	}
	remaining := c.dates(Range{start, r.To})
	forecast := ready(c.add(expense, c.scale(baseSum, int64(remaining), int64(baseDays), true)), nil)
	forecast.Basis = basis(base)
	m["tiles.forecast"] = forecast
}
func (c *calculator) chart(r Range) Metric {
	required := accounting.Threshold("monthly_chart").MonthsWithData
	if len(c.cal.months) < required {
		return locked("need_months_with_data", required, len(c.cal.months))
	}
	months := periodMonths(c.in.Kind)
	start := r.From
	if months == 1 {
		months, start = 6, shiftMonths(r.From, -5)
	}
	out := Metric{Status: "ready"}
	for i := 0; i < months; i++ {
		from := shiftMonths(start, i)
		v := Range{from, shiftMonths(from, 1)}
		p := Point{Month: from.Format("2006-01"), Range: v, Bounds: c.cal.bounds(v)}
		if p.Bounds.From != nil {
			e, in := c.sum(v, "expense", ""), c.sum(v, "income", "")
			p.Expense, p.Income = &e, &in
		}
		out.Points = append(out.Points, p)
	}
	return out
}

func (c *calculator) dates(r Range) int {
	count, err := dates(c.ctx, r)
	if err != nil {
		c.err = err
	}
	return count
}
