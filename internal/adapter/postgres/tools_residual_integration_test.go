package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

func TestIntegrationSmallCategoryMerchantDrilldownKeepsMonthlyResidualAndExactIdentity(t *testing.T) {
	f := importRepository(t)
	user := uuid.NewString()
	from, to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	doc := importing.Document{Accounts: []importing.Account{{Key: "card", Type: "checking"}}}
	add := func(at time.Time, amount int64, direction, merchant, category string) {
		n := len(doc.Lines) + 1
		doc.Lines = append(doc.Lines, importing.Line{AccountKey: "card", Number: n, FITID: fmt.Sprint(n), OccurredAt: &at, AmountMinor: amount, Currency: "RUB", Direction: direction, Description: merchant, BankCategory: category})
	}
	for i := range 9 {
		add(from.Add(time.Duration(i+1)*24*time.Hour), 100, "expense", "Coffee", "Супермаркеты")
	}
	sept := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	add(sept, 10000, "expense", "Coffee", "Супермаркеты")
	add(sept.Add(time.Hour), 3000, "income", "Coffee", "Возврат")
	add(sept.Add(2*time.Hour), 500, "expense", "Coffee", "Такси")
	add(sept, 777, "expense", "Coffee extra", "Супермаркеты")
	componentImport(t, f, user, doc, from, to)
	tools, public := componentTools(t, f, f.db)
	for _, mode := range []string{"with", "without"} {
		grouped, err := tools.Aggregate(t.Context(), user, operations.Filter{Mode: mode, Category: "cat_supermarkets", Selection: "s_small_category_cat_supermarkets"}, true, []string{"merchant"}, 50)
		if err != nil || len(grouped.Groups) != 2 {
			t.Fatal(grouped, err)
		}
		checked := false
		for _, group := range grouped.Groups {
			if group.Keys[0].Merchant != "Coffee" {
				continue
			}
			checked = true
			if group.Count != 1 || group.Amount != 10000 {
				t.Fatal(group)
			}
			found, err := tools.Find(t.Context(), user, group.Filters, false, "", 100)
			if err != nil || found.Count != 1 || found.Items[0].Amount != 10000 || found.Items[0].Kind != "expense" {
				t.Fatal("refund/small month/near-name/cross-category leaked", found, err)
			}
			search, err := public.Search(t.Context(), user, group.Filters, false)
			if err != nil || len(search.Items) != 1 || search.Items[0].ID != found.Items[0].ID {
				t.Fatal("public drilldown differs", search, err)
			}
			again, err := tools.Aggregate(t.Context(), user, group.Filters, false, []string{"merchant"}, 50)
			if err != nil || again.Amount != group.Amount || again.Count != group.Count {
				t.Fatal(again, err)
			}
		}
		if !checked {
			t.Fatal("Coffee residual group missing")
		}
	}
}

func TestIntegrationUncategorizedAndFeeMerchantDrilldownRetainsSignedCategory(t *testing.T) {
	for _, tc := range []struct{ selection, category string }{{"s_uncategorized", ""}, {"s_bank_fees", "Услуги банка"}} {
		f := importRepository(t)
		user := uuid.NewString()
		from := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		doc := importing.Document{Accounts: []importing.Account{{Key: "card", Type: "checking"}}}
		for i, row := range []struct {
			direction, category string
			amount              int64
		}{{"expense", tc.category, 10000}, {"income", "Возврат", 3000}, {"expense", "Супермаркеты", 20000}} {
			at := from.Add(time.Duration(i) * 24 * time.Hour)
			doc.Lines = append(doc.Lines, importing.Line{AccountKey: "card", Number: i + 1, FITID: fmt.Sprint(i), OccurredAt: &at, AmountMinor: row.amount, Currency: "RUB", Direction: row.direction, Description: "Store", BankCategory: row.category})
		}
		componentImport(t, f, user, doc, from, from.AddDate(0, 1, -1))
		tools, public := componentTools(t, f, f.db)
		grouped, err := tools.Aggregate(t.Context(), user, operations.Filter{Mode: "with", Selection: tc.selection}, true, []string{"merchant"}, 50)
		if err != nil || len(grouped.Groups) != 1 || grouped.Amount != 7000 || grouped.Count != 2 {
			t.Fatal(tc, grouped, err)
		}
		filter := grouped.Groups[0].Filters
		found, err := public.Search(t.Context(), user, filter, false)
		if err != nil || len(found.Items) != 2 {
			t.Fatal(tc, found, err)
		}
		again, err := tools.Aggregate(t.Context(), user, filter, false, nil, 50)
		if err != nil || again.Amount != 7000 || again.Count != 2 {
			t.Fatal(tc, again, err)
		}
	}
}
