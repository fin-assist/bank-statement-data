package operations

import (
	"context"
	"sort"
	"time"
)

func text(value string) *string { return &value }

func project(ctx context.Context, state Snapshot, details bool) ([]Operation, error) {
	zone, err := time.LoadLocation(state.TimeZone)
	if err != nil {
		return nil, ErrState
	}
	return projectInZone(ctx, state, details, zone)
}

// projectInZone reuses the request's validated calendar for every streamed row.
func projectInZone(ctx context.Context, state Snapshot, details bool, zone *time.Location) ([]Operation, error) {
	pairs := make(map[string][]int)
	for i, op := range state.Operations {
		if op.PairID != "" {
			pairs[op.PairID] = append(pairs[op.PairID], i)
		}
	}
	var out []Operation
	for _, op := range state.Operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if op.PairID != "" && op.Direction == "income" {
			continue
		}
		cat, ok := state.Categories[op.Category]
		if !ok || op.AmountMinor < 0 {
			return nil, ErrState
		}
		account, ok := state.Accounts[op.AccountID]
		if !ok {
			return nil, ErrState
		}
		detail, ok := state.Details[op.ID]
		if !ok {
			return nil, ErrState
		}
		item := Operation{ID: op.ID, Kind: op.Direction, At: op.At, Amount: op.AmountMinor, Currency: op.Currency, Title: op.Description, Category: cat, Manual: op.ManualCategory != "", Status: detail.Status, Account: &account}
		if !details {
			item.Account.Spoken = ""
		}
		if details {
			item.Description = text(op.Description)
			item.PostedAt = detail.PostedAt
			item.MCC = detail.MCC
			bank := state.Categories[op.BankCategory]
			if bank.BankName != "" {
				item.BankCategory = text(bank.BankName)
			}
		}
		if op.Category == "cat_own_transfer" {
			item.Note = text("не в тратах")
			if details {
				item.NoteDetails = text("Не считаем тратой и доходом")
			}
		}
		if op.Category == "cat_refund" {
			target, ok := state.Categories[op.RefundTarget]
			if !ok {
				return nil, ErrState
			}
			item.Note = text("возврат · " + target.Name)
			if op.RefundTarget == "cat_uncategorized" {
				item.Note = text("возврат")
			}
			if details {
				months := []string{"январе", "феврале", "марте", "апреле", "мае", "июне", "июле", "августе", "сентябре", "октябре", "ноябре", "декабре"}
				item.NoteDetails = text("Уменьшает расходы «" + target.Name + "» в " + months[int(op.At.In(zone).Month())-1])
				if op.RefundTarget == "cat_uncategorized" {
					item.NoteDetails = text("Уменьшает общие расходы в " + months[int(op.At.In(zone).Month())-1])
				}
			}
		}
		if op.PairID != "" {
			indices := pairs[op.PairID]
			if len(indices) != 2 {
				return nil, ErrState
			}
			incomeIndex := -1
			for _, i := range indices {
				other := state.Operations[i]
				if other.Direction == "income" && other.Currency == op.Currency && other.AmountMinor == op.AmountMinor {
					incomeIndex = i
				}
			}
			if incomeIndex < 0 {
				return nil, ErrState
			}
			other := state.Operations[incomeIndex]
			to, ok := state.Accounts[other.AccountID]
			if !ok {
				return nil, ErrState
			}
			if !details {
				to.Spoken = ""
			}
			item.ID, item.Kind, item.Title = op.PairID, "own_transfer", "Перевод между счетами"
			item.Manual = false
			item.Account = nil
			item.FromAccount = &account
			item.ToAccount = &to
			item.Description, item.BankCategory, item.MCC, item.PostedAt = nil, nil, nil, nil
			if state.Details[other.ID].Status == "pending" {
				item.Status = "pending"
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID > out[j].ID
		}
		return out[i].At.After(out[j].At)
	})
	return out, nil
}
