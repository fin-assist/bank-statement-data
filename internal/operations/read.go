package operations

import (
	"context"
	"math/big"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/accounting"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/domain"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

// Feed selects three calendar months anchored at the newest operation or cursor.
func (s *Service) Feed(ctx context.Context, user string, before *time.Time) (out Feed, err error) {
	err = s.tx.ReadOperations(ctx, func(ctx context.Context) error {
		meta, e := s.repo.Metadata(ctx, user)
		if e != nil {
			return e
		}
		zone, e := time.LoadLocation(meta.TimeZone)
		if e != nil {
			return ErrState
		}
		first, last := meta.FirstVisible, meta.LastVisible
		if first == nil {
			first = meta.First
		}
		if last == nil {
			last = meta.Last
		}
		anchor := s.now().In(zone)
		if last != nil {
			anchor = last.In(zone)
		}
		to := accounting.MonthStart(anchor, 1)
		if before != nil {
			to = before.In(zone)
			if !to.Equal(accounting.MonthStart(to, 0)) {
				return &importing.InputError{Field: "before", Reason: "INVALID_FORMAT"}
			}
		}
		from := accounting.MonthStart(to, -3)
		if from.Year() < 1 || to.Year() > 9999 {
			return &importing.InputError{Field: "before", Reason: "OUT_OF_RANGE"}
		}
		state, e := s.repo.Snapshot(ctx, user, &from, &to)
		if e != nil {
			return e
		}
		items, e := project(ctx, state, false)
		if e != nil {
			return e
		}
		out = Feed{Range: Range{from, to}, TimeZone: meta.TimeZone}
		for _, item := range items {
			if !item.At.Before(from) && item.At.Before(to) {
				out.Items = append(out.Items, item)
			}
		}
		if first != nil && first.Before(from) {
			out.NextBefore = &from
		}
		for m := accounting.MonthStart(to, -1); !m.Before(from); m = accounting.MonthStart(m, -1) {
			global, bounds, expense, income, e := calculating.InspectMonth(ctx, state.Snapshot, s.now(), m)
			if e != nil {
				return e
			}
			if bounds.From != nil || expense != 0 || income != 0 {
				summary := Summary{Month: m.Format("2006-01"), Range: Range{m, accounting.MonthStart(m, 1)}, Expense: expense, Income: income}
				if bounds.Coverage != "complete" {
					summary.DataTo = bounds.To
				}
				out.Summary = append(out.Summary, summary)
			}
			out.State.DataTo = global.To
			out.State.Stale = global.StaleAfter == nil || !s.now().Before(*global.StaleAfter)
		}
		out.State.LastOperation = meta.Last
		for _, u := range state.Uploads {
			if u.Status == "done" || u.Status == "processing" {
				if out.State.LastUpload == nil || u.UploadedAt.After(*out.State.LastUpload) {
					v := u.UploadedAt
					out.State.LastUpload = &v
				}
			}
		}
		return ctx.Err()
	})
	return
}

// Search returns the complete selection or refuses rather than truncating at 10000.
func (s *Service) Search(ctx context.Context, user string, f Filter, allTime bool) (out Search, err error) {
	if err = validateFilter(f, allTime); err != nil {
		return
	}
	err = s.tx.ReadOperations(ctx, func(ctx context.Context) error {
		meta, e := s.repo.Metadata(ctx, user)
		if e != nil {
			return e
		}
		zone, e := time.LoadLocation(meta.TimeZone)
		if e != nil {
			return ErrState
		}
		out.TimeZone = meta.TimeZone
		defaultRange := f.From == nil && f.To == nil && !allTime
		if defaultRange {
			today := accounting.StartOfDay(s.now(), zone)
			lastDay := time.Date(today.Year()-1, today.Month()+1, 0, 12, 0, 0, 0, zone).Day()
			from := accounting.StartOfDay(time.Date(today.Year()-1, today.Month(), min(today.Day(), lastDay), 12, 0, 0, 0, zone), zone)
			to := accounting.AddDays(today, 1)
			f.From, f.To = &from, &to
			older := meta.First != nil && meta.First.Before(from)
			out.HasOlder = &older
		}
		if f.From != nil && f.To != nil {
			out.Range = &Range{*f.From, *f.To}
		}
		// Only registry/account/coverage metadata is materialized. Raw operations
		// arrive as one public candidate at a time, even for explicit all-time reads.
		empty := time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
		state, e := s.repo.Snapshot(ctx, user, &empty, &empty)
		if e != nil {
			return e
		}
		if e = s.resolveSelection(ctx, user, &state, f.Selection); e != nil {
			return e
		}
		selected, e := prepareSelection(ctx, state, f, s.now())
		if e != nil {
			return e
		}
		opts := StreamOptions{From: f.From, To: f.To, TimeZone: meta.TimeZone}
		if f.Selection == "s_regular_all" {
			end := accounting.MonthStart(s.now().In(zone), 1)
			if f.To != nil {
				end = *f.To
			}
			opts.RegularMonths, e = calculating.RegularWindow(ctx, state.Snapshot, s.now(), end)
			if e != nil {
				return e
			}
		}
		if strings.HasPrefix(f.Selection, "s_small_category_") {
			opts.Small = true
			opts.SmallFrom, opts.SmallTo = f.From, f.To
			if opts.SmallFrom != nil {
				v := accounting.MonthStart(opts.SmallFrom.In(zone), 0)
				opts.SmallFrom = &v
			}
			if opts.SmallTo != nil {
				v := opts.SmallTo.In(zone)
				if !v.Equal(accounting.MonthStart(v, 0)) {
					v = accounting.MonthStart(v, 1)
				}
				opts.SmallTo = &v
			}
		}
		return s.repo.Stream(ctx, user, opts, func(candidate Row) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			one := state
			one.Operations = candidate.Facts
			one.Details = candidate.Details
			rows, e := project(ctx, one, false)
			if e != nil {
				return e
			}
			if len(rows) != 1 || len(candidate.Facts) == 0 {
				return ErrState
			}
			row, op := rows[0], candidate.Facts[0]
			queryOK := f.Query == ""
			for _, fact := range candidate.Facts {
				if queryMatches(row, state.Categories[fact.BankCategory].Name, f.Query) || strings.Contains(normalized(fact.Description), normalized(strings.TrimSpace(f.Query))) {
					queryOK = true
					break
				}
			}
			if !queryOK {
				return nil
			}
			local := selected
			if f.Selection == "s_regular_all" {
				local.regular = map[string]bool{op.Description: !candidate.Dismissed && calculating.RegularGroup(candidate.RegularAmounts, candidate.RegularMonths, opts.RegularMonths)}
			}
			if opts.Small {
				local.small = map[string]bool{op.At.In(zone).Format("2006-01") + "\x00" + op.Description: calculating.SmallGroup(candidate.SmallCount, candidate.SmallTotal)}
			}
			if matches(row, op, f, local, zone, state.Categories[op.BankCategory].Name) {
				out.Items = append(out.Items, row)
				if len(out.Items) > 10000 {
					return ErrTooMany
				}
			}
			return nil
		})
	})
	return
}

func validateFilter(f Filter, all bool) error {
	invalid := func(field string) error { return &importing.InputError{Field: field, Reason: "OUT_OF_RANGE"} }
	if all && (f.From != nil || f.To != nil) {
		return invalid("all_time")
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return invalid("filter.to")
	}
	if f.AmountFrom != nil && *f.AmountFrom < 0 {
		return invalid("filter.amount_from_minor")
	}
	if f.AmountTo != nil && *f.AmountTo < 0 {
		return invalid("filter.amount_to_minor")
	}
	if f.AmountFrom != nil && f.AmountTo != nil && *f.AmountFrom > *f.AmountTo {
		return invalid("filter.amount_to_minor")
	}
	if f.Kind != "" && f.Kind != "expense" && f.Kind != "income" {
		return invalid("filter.kind")
	}
	if f.Mode != "" && f.Mode != "with" && f.Mode != "without" {
		return invalid("filter.transfer_mode")
	}
	return nil
}

type selection struct {
	name, key      string
	regular, small map[string]bool
}

func (s *Service) resolveSelection(ctx context.Context, user string, state *Snapshot, token string) error {
	var kind, id string
	switch {
	case token == "s_regular_all":
		return nil
	case strings.HasPrefix(token, "s_regular_"):
		kind, id = domain.ReferenceKindRegular, strings.TrimPrefix(token, "s_regular_")
	case strings.HasPrefix(token, "s_small_merchant_"):
		kind, id = domain.ReferenceKindMerchant, strings.TrimPrefix(token, "s_small_merchant_")
	case strings.HasPrefix(token, "s_merchant_"):
		kind, id = domain.ReferenceKindMerchant, strings.TrimPrefix(token, "s_merchant_")
	default:
		return nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || id != parsed.String() {
		return ErrSelection
	}
	key, err := s.repo.SelectionKey(ctx, user, kind, id)
	if err != nil {
		return err
	}
	if kind == domain.ReferenceKindRegular {
		state.Regular = map[string]string{id: key}
	} else {
		state.Merchants = map[string]string{id: key}
	}
	return nil
}

func prepareSelection(ctx context.Context, state Snapshot, f Filter, now time.Time) (out selection, err error) {
	out.name = f.Selection
	mode := f.Mode
	if mode == "" {
		mode = "with"
	}
	switch {
	case f.Selection == "", f.Selection == "s_uncategorized", f.Selection == "s_bank_fees":
		return out, nil
	case f.Selection == "s_regular_all":
		zone, e := time.LoadLocation(state.TimeZone)
		if e != nil {
			return out, ErrState
		}
		from := accounting.MonthStart(now.In(zone), 0)
		to := accounting.MonthStart(from, 1)
		if f.From != nil {
			from = *f.From
		}
		if f.To != nil {
			to = *f.To
		}
		out.regular, err = calculating.RegularDescriptions(ctx, state.Snapshot, now, calculating.Range{From: from, To: to}, mode, state.Dismissed)
		return
	case strings.HasPrefix(f.Selection, "s_regular_"):
		out.key = state.Regular[strings.TrimPrefix(f.Selection, "s_regular_")]
	case strings.HasPrefix(f.Selection, "s_small_merchant_"):
		out.key = state.Merchants[strings.TrimPrefix(f.Selection, "s_small_merchant_")]
	case strings.HasPrefix(f.Selection, "s_merchant_"):
		out.key = state.Merchants[strings.TrimPrefix(f.Selection, "s_merchant_")]
	case strings.HasPrefix(f.Selection, "s_small_category_"):
		out.key = strings.TrimPrefix(f.Selection, "s_small_category_")
		if _, ok := state.Categories[out.key]; !ok {
			return out, ErrSelection
		}
		out.small, err = calculating.SmallMerchantGroups(ctx, state.Snapshot, mode)
		return
	default:
		return out, ErrSelection
	}
	if out.key == "" {
		return out, ErrSelection
	}
	return out, nil
}

func matches(row Operation, op importing.Operation, f Filter, selection selection, zone *time.Location, bankName string) bool {
	if f.From != nil && row.At.Before(*f.From) || f.To != nil && !row.At.Before(*f.To) || f.AmountFrom != nil && row.Amount < *f.AmountFrom || f.AmountTo != nil && row.Amount > *f.AmountTo {
		return false
	}
	kind, cat := row.Kind, row.Category.ID
	if f.Mode != "" {
		var ok bool
		kind, cat, _, ok = calculating.FinancialRow(op, f.Mode)
		if !ok {
			return false
		}
	}
	if f.Kind != "" && (kind != f.Kind || row.Kind == "own_transfer" || op.Category == importing.OwnTransferCategory) {
		return false
	}
	if f.Category != "" && cat != f.Category {
		return false
	}
	_ = bankName // Text matching also visits the paired source facts in Search.
	switch selection.name {
	case "":
		return true
	case "s_uncategorized":
		return op.Category == importing.UncategorizedCategory || op.Category == importing.RefundCategory && op.RefundTarget == importing.UncategorizedCategory
	case "s_bank_fees":
		k, c, _, ok := calculating.FinancialRow(op, "with")
		return ok && k == "expense" && calculating.Fee(op, c)
	case "s_regular_all":
		return op.Direction == "expense" && selection.regular[op.Description] && calculating.Observation(cat)
	}
	if strings.HasPrefix(selection.name, "s_small_category_") {
		return op.Direction == "expense" && cat == selection.key && !selection.small[op.At.In(zone).Format("2006-01")+"\x00"+op.Description] && calculating.Observation(cat)
	}
	if strings.HasPrefix(selection.name, "s_merchant_") {
		return op.Description == selection.key
	}
	return op.Direction == "expense" && op.Description == selection.key && calculating.Observation(cat)
}

func normalized(s string) string { return strings.ReplaceAll(strings.ToLower(s), "ё", "е") }
func queryMatches(row Operation, bankName, q string) bool {
	q = normalized(strings.TrimSpace(q))
	if strings.Contains(normalized(row.Title), q) || strings.Contains(normalized(row.Category.Name), q) || strings.Contains(normalized(bankName), q) {
		return true
	}
	if amount, ok := queryAmount(q); ok && amount == row.Amount {
		return true
	}
	if len(q) == 4 && strings.IndexFunc(q, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		for _, a := range []*Account{row.Account, row.FromAccount, row.ToAccount} {
			if a != nil && a.Mask != nil && strings.HasSuffix(*a.Mask, q) {
				return true
			}
		}
	}
	return false
}

func queryAmount(q string) (int64, bool) {
	q = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		if r == ',' {
			return '.'
		}
		return r
	}, q)
	if q == "" || strings.IndexFunc(q, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }) >= 0 {
		return 0, false
	}
	parts := strings.Split(q, ".")
	if len(parts) > 2 || len(parts) == 2 && len(parts[1]) > 2 {
		return 0, false
	}
	n, ok := new(big.Rat).SetString(q)
	if !ok {
		return 0, false
	}
	n.Mul(n, big.NewRat(100, 1))
	if !n.IsInt() || !n.Num().IsInt64() {
		return 0, false
	}
	return n.Num().Int64(), true
}
