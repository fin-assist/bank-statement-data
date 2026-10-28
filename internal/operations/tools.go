package operations

import (
	"context"
	"errors"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Tools implements the assistant's read-only scenarios using the public selectors.
type Tools struct {
	operations *Service
	facts      ToolRepository
}

// NewTools requires the same operations service and routed metadata repository.
func NewTools(service *Service, facts ToolRepository) (*Tools, error) {
	if service == nil || facts == nil {
		return nil, errors.New("tool dependencies required")
	}
	return &Tools{service, facts}, nil
}

func (s *Tools) admission(ctx context.Context, user string) error {
	deleted, err := s.operations.deleted.Deleted(ctx, user)
	if err != nil {
		return err
	}
	if deleted {
		return importing.ErrDeleted
	}
	return nil
}
func (s *Tools) read(ctx context.Context, user string, fn func(context.Context) error) error {
	if err := s.admission(ctx, user); err != nil {
		return err
	}
	if err := s.operations.tx.ReadOperations(ctx, fn); err != nil {
		return err
	}
	return s.admission(ctx, user)
}

// Version returns committed source revisions and legacy timestamps from primary.
func (s *Tools) Version(ctx context.Context, user string) (out DataVersion, err error) {
	if err = s.admission(ctx, user); err != nil {
		return out, err
	}
	// One query outside the routed read transaction deliberately uses primary.
	// A lagging replica must not hide a commit from history invalidation.
	out, err = s.facts.Version(ctx, user)
	if err == nil {
		err = s.admission(ctx, user)
	}
	return
}

// Overview reports statement-day coverage and source capabilities independently
// from whether the covered interval happens to contain any operation.
func (s *Tools) Overview(ctx context.Context, user string) (out Overview, err error) {
	err = s.read(ctx, user, func(ctx context.Context) error {
		empty := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
		state, e := s.operations.repo.Snapshot(ctx, user, &empty, &empty)
		if e != nil {
			return e
		}
		facts, e := s.facts.Facts(ctx, user, state.TimeZone)
		if e != nil {
			return e
		}
		state.Operations = facts.LastByDay
		out.State, _, out.Months, e = calculating.InspectCoverage(ctx, state.Snapshot, s.operations.now(), nil, nil)
		if e != nil {
			return e
		}
		out.Version = facts.Version
		for _, u := range state.Uploads {
			if u.Status == "done" {
				out.HasSource = true
				break
			}
		}
		return nil
	})
	return
}

func (s *Tools) setup(ctx context.Context, user string, state Snapshot, meta Metadata, f Filter) (ToolResult, ToolFacts, error) {
	facts, err := s.facts.Facts(ctx, user, meta.TimeZone)
	if err != nil {
		return ToolResult{}, facts, err
	}
	state.Operations = facts.LastByDay
	now := s.operations.now()
	global, bounds, _, err := calculating.InspectCoverage(ctx, state.Snapshot, now, f.From, f.To)
	if err != nil {
		return ToolResult{}, facts, err
	}
	out := ToolResult{Bounds: bounds, Version: facts.Version, TimeZone: meta.TimeZone, CalculatedAt: now}
	from, to := f.From, f.To
	if from == nil {
		from = global.From
		if meta.First != nil && (from == nil || meta.First.Before(*from)) {
			from = meta.First
		}
	}
	if to == nil {
		to = global.To
		if meta.Last != nil {
			after := meta.Last.Add(time.Microsecond)
			if to == nil || after.After(*to) {
				to = &after
			}
		}
	}
	if from != nil && to != nil && from.Before(*to) {
		out.Range = &Range{*from, *to}
		// An export may contain an operation outside its declared statement
		// days. All-time links still include that row, without calling those
		// extra days covered.
		expandedFrom := f.From == nil && (global.From == nil || !from.Equal(*global.From))
		expandedTo := f.To == nil && (global.To == nil || !to.Equal(*global.To))
		if expandedFrom || expandedTo {
			_, out.Bounds, _, err = calculating.InspectCoverage(ctx, state.Snapshot, now, from, to)
			if err != nil {
				return ToolResult{}, facts, err
			}
		}
	}
	return out, facts, nil
}

// Find returns a bounded deterministic list while scanning every match for count.
func (s *Tools) Find(ctx context.Context, user string, f Filter, all bool, order string, limit int) (out Found, err error) {
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return out, toolInput("limit")
	}
	if order != "" && order != "newest" && order != "largest" {
		return out, toolInput("order")
	}
	less := func(a, b Operation) bool {
		if order == "largest" && a.Amount != b.Amount {
			return a.Amount > b.Amount
		}
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return a.ID > b.ID
	}
	if err = s.admission(ctx, user); err != nil {
		return out, err
	}
	_, err = s.operations.scan(ctx, user, f, all, func(c context.Context, state Snapshot, meta Metadata, effective Filter) error {
		var e error
		out.ToolResult, _, e = s.setup(c, user, state, meta, effective)
		return e
	}, func(_ context.Context, row Operation, _ importing.Operation) error {
		if out.Count == math.MaxInt32 {
			return ErrState
		}
		out.Count++
		if len(out.Items) < limit {
			out.Items = append(out.Items, row)
			sort.Slice(out.Items, func(i, j int) bool { return less(out.Items[i], out.Items[j]) })
		} else if less(row, out.Items[len(out.Items)-1]) {
			out.Items[len(out.Items)-1] = row
			sort.Slice(out.Items, func(i, j int) bool { return less(out.Items[i], out.Items[j]) })
		}
		return nil
	})
	if err == nil {
		err = s.admission(ctx, user)
	}
	return
}

func toolInput(field string) error {
	return &importing.InputError{Field: field, Reason: "OUT_OF_RANGE"}
}

type groupAccumulator struct {
	group  AggregateGroup
	amount big.Int
}

// Aggregate applies shared signed accounting rules before grouping. Totals include
// all groups even when the response keeps only the largest absolute amounts.
func (s *Tools) Aggregate(ctx context.Context, user string, f Filter, all bool, dimensions []string, limit int) (out Aggregate, err error) {
	if f.Mode != "with" && f.Mode != "without" {
		return out, toolInput("filter.transfer_mode")
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return out, toolInput("limit")
	}
	if len(dimensions) > 2 {
		return out, toolInput("group_by")
	}
	seen := map[string]bool{}
	for _, d := range dimensions {
		if seen[d] || d != "month" && d != "category" && d != "merchant" {
			return out, toolInput("group_by")
		}
		seen[d] = true
	}
	groups := map[string]*groupAccumulator{}
	var total big.Int
	var references ToolReferences
	var state Snapshot
	var effective Filter
	var zone *time.Location
	if err = s.admission(ctx, user); err != nil {
		return out, err
	}
	_, err = s.operations.scan(ctx, user, f, all, func(c context.Context, snapshot Snapshot, meta Metadata, selection Filter) error {
		state, effective = snapshot, selection
		var e error
		out.ToolResult, _, e = s.setup(c, user, state, meta, effective)
		if e != nil {
			return e
		}
		if seen["merchant"] {
			references, e = s.facts.References(c, user)
			if e != nil {
				return e
			}
		}
		zone, e = time.LoadLocation(meta.TimeZone)
		return e
	}, func(_ context.Context, _ Operation, op importing.Operation) error {
		_, category, amount, ok := calculating.FinancialRow(op, f.Mode)
		if !ok {
			return nil
		}
		total.Add(&total, big.NewInt(amount))
		if out.Count == math.MaxInt64 {
			return ErrState
		}
		out.Count++
		keys := make([]GroupKey, 0, len(dimensions))
		filters := effective
		if out.Range != nil {
			if filters.From == nil {
				filters.From = &out.Range.From
			}
			if filters.To == nil {
				filters.To = &out.Range.To
			}
		}
		idParts := make([]string, 0, len(dimensions))
		for _, d := range dimensions {
			switch d {
			case "month":
				month := op.At.In(zone).Format("2006-01")
				keys = append(keys, GroupKey{Month: month})
				idParts = append(idParts, month)
				start := accounting.MonthStart(op.At.In(zone), 0)
				end := accounting.MonthStart(start, 1)
				if filters.From == nil || filters.From.Before(start) {
					filters.From = &start
				}
				if filters.To == nil || filters.To.After(end) {
					filters.To = &end
				}
			case "category":
				cat, exists := state.Categories[category]
				if !exists {
					return ErrState
				}
				keys = append(keys, GroupKey{Category: &cat})
				idParts = append(idParts, category)
				filters.Category = category
			case "merchant":
				id, exists := references.Merchants[op.Description]
				if !exists || id == "" {
					return ErrState
				}
				title := op.Description
				if title == "" {
					title = "Без описания"
				}
				token := "s_merchant_" + id
				keys = append(keys, GroupKey{Merchant: title, Selection: token})
				idParts = append(idParts, id)
				switch {
				case effective.Selection == "s_regular_all":
					id := references.Regular[op.Description]
					if id == "" {
						return ErrState
					}
					filters.Selection = "s_regular_" + id
				case effective.Selection == "s_uncategorized":
					filters.Selection = token
					filters.Category = importing.UncategorizedCategory
				case effective.Selection == "s_bank_fees":
					filters.Selection = token
					filters.Kind = "expense"
					if !calculating.Fee(op, "") {
						filters.Category = "cat_bank_fees"
					}
				case strings.HasPrefix(effective.Selection, "s_small_category_"):
					cat := strings.TrimPrefix(effective.Selection, "s_small_category_")
					if strings.HasPrefix(effective.Selection, smallCategoryMerchantPrefix) {
						_, cat = smallCategoryMerchant(effective.Selection)
					}
					filters.Selection = smallCategoryMerchantPrefix + id + "_" + cat
					filters.Category = cat
					filters.Kind = "expense"
				case effective.Selection == "":
					filters.Selection = token
					// A merchant-specific selection already has this exact description and
					// retaining it also preserves regular/small observation semantics.
				}
			}
		}
		key := strings.Join(idParts, "\x00")
		acc := groups[key]
		if acc == nil {
			acc = &groupAccumulator{group: AggregateGroup{Keys: keys, Filters: filters}}
			groups[key] = acc
		}
		acc.amount.Add(&acc.amount, big.NewInt(amount))
		acc.group.Count++
		return nil
	})
	if err != nil {
		return out, err
	}
	if err = s.admission(ctx, user); err != nil {
		return out, err
	}
	if !total.IsInt64() {
		return out, ErrState
	}
	out.Amount = total.Int64()
	for _, acc := range groups {
		if !acc.amount.IsInt64() {
			return out, ErrState
		}
		g := acc.group
		g.Amount = acc.amount.Int64()
		g.Average = roundedAverage(g.Amount, g.Count)
		out.Groups = append(out.Groups, g)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		a, b := out.Groups[i], out.Groups[j]
		cmp := new(big.Int).Abs(big.NewInt(a.Amount)).Cmp(new(big.Int).Abs(big.NewInt(b.Amount)))
		if cmp != 0 {
			return cmp > 0
		}
		return groupIdentity(a) < groupIdentity(b)
	})
	if len(out.Groups) > limit {
		out.Truncated = true
		out.Groups = out.Groups[:limit]
	}
	return
}
func groupIdentity(g AggregateGroup) string {
	var keys []string
	for _, k := range g.Keys {
		switch {
		case k.Category != nil:
			keys = append(keys, k.Category.ID)
		case k.Month != "":
			keys = append(keys, k.Month)
		default:
			keys = append(keys, k.Selection)
		}
	}
	return strings.Join(keys, "\x00")
}
func roundedAverage(amount, count int64) int64 {
	if count == 0 {
		return 0
	}
	n := big.NewInt(amount)
	sign := n.Sign()
	n.Abs(n)
	n.Add(n, big.NewInt(count/2))
	n.Quo(n, big.NewInt(count))
	if sign < 0 {
		n.Neg(n)
	}
	return n.Int64()
}
