package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/go-grpc/reqctx"
	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/adapter/postgres"
	"github.com/fin-assist/bank-statement-data/internal/importing"
	"github.com/fin-assist/bank-statement-data/internal/operations"
)

func TestIntegrationOperationLedgerSelectionsEditsAndReferences(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	s := a.Operations
	ctx := t.Context()
	feed, err := s.Feed(ctx, apiUser, nil)
	if err != nil || len(feed.Summary) != 3 || feed.Summary[0].Expense != 743900 || feed.Summary[0].Income != 4000000 || feed.NextBefore == nil {
		t.Fatal("real ledger feed", feed, err)
	}
	if _, err = s.Feed(ctx, apiUser, &feed.Items[0].At); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	if _, err = s.Feed(ctx, apiUser, feed.NextBefore); err != nil {
		t.Fatal(err)
	}
	from, to := calculationDate(t, "2026-09"), calculationDate(t, "2026-10")
	for _, tc := range []struct {
		f     operations.Filter
		count int
	}{{operations.Filter{From: &from, To: &to, Selection: "s_bank_fees", Mode: "with"}, 2}, {operations.Filter{From: &from, To: &to, Selection: "s_regular_all", Mode: "without"}, 2}, {operations.Filter{Query: "cloud"}, 12}, {operations.Filter{From: &from, To: &to, Mode: "without", Kind: "expense"}, 5}} {
		out, e := s.Search(ctx, apiUser, tc.f, false)
		if e != nil || len(out.Items) != tc.count {
			t.Fatal("real financial selection", out, e)
		}
	}
	catalog, err := s.Categories(ctx, apiUser)
	if err != nil || len(catalog) < 3 || catalog[0].ID != "cat_own_transfer" || catalog[2].Assignable {
		t.Fatal("catalog policy", catalog, err)
	}
	item := feed.Items[0]
	got, zone, err := s.Get(ctx, apiUser, item.ID, from)
	if err != nil || got.ID != item.ID || zone != "Europe/Moscow" || got.Description == nil {
		t.Fatal("fallback", got, zone, err)
	}
	changed, err := s.Change(ctx, apiUser, item.ID, "cat_taxi", from)
	if err != nil || len(changed.Items) != 1 || !changed.Items[0].Manual {
		t.Fatal("edit", changed, err)
	}
	var before time.Time
	if err = a.DB.Primary().QueryRow(ctx, `SELECT categories_changed_at FROM user_state WHERE user_id=$1`, apiUser).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Change(ctx, apiUser, item.ID, "cat_taxi", item.At); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err = a.DB.Primary().QueryRow(ctx, `SELECT categories_changed_at FROM user_state WHERE user_id=$1`, apiUser).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatal("idempotent edit changed version", before, after, err)
	}
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	got, _, err = s.Get(ctx, apiUser, item.ID, item.At)
	if err != nil || got.Category.ID != "cat_taxi" || !got.Manual {
		t.Fatal("manual edit lost on import", got, err)
	}
	if _, err = s.Change(ctx, apiUser, item.ID, "cat_uncategorized", item.At); !errors.Is(err, operations.ErrCategory) {
		t.Fatal(err)
	}
	if _, _, err = s.Get(ctx, apiOther, item.ID, item.At); !errors.Is(err, operations.ErrNotFound) {
		t.Fatal("foreign lookup", err)
	}
	if _, err = s.Change(ctx, apiOther, item.ID, "cat_taxi", item.At); !errors.Is(err, operations.ErrNotFound) {
		t.Fatal("foreign write", err)
	}
	result, err := a.Calculations.Calculate(ctx, apiUser, "month", from)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Periods {
		for _, group := range p.Metrics["insights.small_frequent"].Small {
			out, e := s.Search(ctx, apiUser, operations.Filter{From: &group.Filter.Range.From, To: &group.Filter.Range.To, Selection: group.Filter.Selection, Mode: p.Mode}, false)
			if e != nil || len(out.Items) != group.Count {
				t.Fatal("small group selection", out, e)
			}
		}
	}
	pay := result.Periods[0].Metrics["insights.regular_payments"].Payments[0].ID
	for range 2 {
		if err = s.Dismiss(ctx, apiUser, pay, true); err != nil {
			t.Fatal(err)
		}
	}
	result, err = a.Calculations.Calculate(ctx, apiUser, "month", from)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Periods[0].Metrics["insights.regular_payments"].Payments {
		if p.ID == pay {
			t.Fatal("dismissed reference reappeared")
		}
	}
	for range 2 {
		if err = s.Dismiss(ctx, apiUser, pay, false); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Dismiss(ctx, apiOther, pay, true); !errors.Is(err, operations.ErrNotFound) {
		t.Fatal("foreign dismissal", err)
	}
	if err = s.Dismiss(ctx, apiUser, uuid.NewString(), false); !errors.Is(err, operations.ErrNotFound) {
		t.Fatal("missing restore", err)
	}
	if _, err = s.Search(ctx, apiUser, operations.Filter{Selection: "s_regular_" + uuid.NewString()}, true); !errors.Is(err, operations.ErrSelection) {
		t.Fatal("foreign selection", err)
	}
	if err = a.Imports.Purge(ctx, apiUser); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Change(ctx, apiUser, item.ID, "cat_taxi", item.At); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("late edit resurrected user", err)
	}
	if err = s.Dismiss(ctx, apiUser, pay, true); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal("late dismissal resurrected user", err)
	}
}

func TestIntegrationOperationPairBoundariesAndDerivedRefundScheduling(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, operationsPairBody())
	s := a.Operations
	feed, err := s.Feed(t.Context(), apiUser, nil)
	if err != nil || len(feed.Items) != 1 || feed.Range.To.In(time.FixedZone("MSK", 10800)).Format("2006-01-02") != "2026-10-01" {
		t.Fatal("public pair bound", feed, err)
	}
	pair := feed.Items[0]
	split, err := s.Change(t.Context(), apiUser, pair.ID, "cat_taxi", pair.At)
	if err != nil || len(split.Items) != 2 || split.ReplacedID == nil {
		t.Fatal("split", split, err)
	}
	var debit operations.Operation
	for _, op := range split.Items {
		if op.Kind == "expense" {
			debit = op
		}
	}
	merge, err := s.Change(t.Context(), apiUser, debit.ID, "cat_own_transfer", debit.At)
	if err != nil || len(merge.Items) != 1 || merge.Items[0].Kind != "own_transfer" || merge.Items[0].Manual {
		t.Fatal("merge", merge, err)
	}
	// A distant manually recategorised purchase changes a refund in a later month.
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, zone)
	to := time.Date(2026, 9, 30, 23, 59, 59, 0, zone)
	body := ruleOFXHeader(from, to) + ruleOFXRow(20, from.AddDate(0, 0, 1), -10000, "Distant Shop", "Супермаркеты") + ruleOFXRow(21, to.Add(-time.Hour), 5000, "Distant Shop", "Возврат") + ruleOFXEnd
	importCalculationBody(t, a, apiUser, body)
	q := "Distant Shop"
	out, err := s.Search(t.Context(), apiUser, operations.Filter{Query: q, Kind: "expense", Mode: "with"}, true)
	if err != nil || len(out.Items) != 2 {
		t.Fatal(out, err)
	}
	var purchase operations.Operation
	for _, op := range out.Items {
		if op.Kind == "expense" {
			purchase = op
		}
	}
	if _, err = s.Change(t.Context(), apiUser, purchase.ID, "cat_taxi", purchase.At); err != nil {
		t.Fatal(err)
	}
	sept := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, zone)
	out, err = s.Search(t.Context(), apiUser, operations.Filter{From: &sept, To: &oct, Category: "cat_taxi", Mode: "with", Kind: "expense"}, false)
	if err != nil || len(out.Items) != 1 || out.Items[0].Kind != "income" || out.Items[0].Note == nil || !strings.Contains(*out.Items[0].Note, "Такси") {
		t.Fatal("persisted cross-window refund target", out, err)
	}
	var queued int
	if err = a.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM recalc_queue WHERE user_id=$1 AND month IN('2026-01-01','2026-09-01','2026-10-01')`, apiUser).Scan(&queued); err != nil || queued != 3 {
		t.Fatal("dependent months not queued", queued, err)
	}
}

func TestIntegrationOperationEditDeleteConcurrencyAndCoherentRouting(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	ctx := t.Context()
	s := a.Operations
	feed, err := s.Feed(ctx, apiUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	item := feed.Items[0]
	for _, read := range []context.Context{ctx, reqctx.WithReadPrimary(ctx)} {
		if _, err = s.Search(read, apiUser, operations.Filter{Query: "cloud"}, false); err != nil {
			t.Fatal("routed read", err)
		}
	}
	// Two writers serialize on user_state; each edit remains a complete assignment.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, category := range []string{"cat_taxi", "cat_supermarkets"} {
		wg.Add(1)
		go func(cat string) {
			defer wg.Done()
			<-start
			_, e := s.Change(ctx, apiUser, item.ID, cat, item.At)
			errs <- e
		}(category)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var raw string
	if err = a.DB.Primary().QueryRow(ctx, `SELECT manual_category_id FROM operation WHERE user_id=$1 AND id=$2`, apiUser, item.ID).Scan(&raw); err != nil || raw != "cat_taxi" && raw != "cat_supermarkets" {
		t.Fatal("partial concurrent write", raw, err)
	}
	// Competing edit and purge may serialize in either order, but after both
	// finish the deleted user's financial rows and version must be gone.
	race := make(chan struct{})
	editDone := make(chan error, 1)
	purgeDone := make(chan error, 1)
	go func() { <-race; _, e := s.Change(ctx, apiUser, item.ID, "cat_own_transfer", item.At); editDone <- e }()
	go func() { <-race; purgeDone <- a.Imports.Purge(ctx, apiUser) }()
	close(race)
	if e := <-editDone; e != nil && !errors.Is(e, importing.ErrDeleted) {
		t.Fatal("concurrent edit", e)
	}
	if e := <-purgeDone; e != nil {
		t.Fatal("concurrent purge", e)
	}
	if _, err = s.Change(ctx, apiUser, item.ID, "cat_taxi", item.At); !errors.Is(err, importing.ErrDeleted) {
		t.Fatal(err)
	}
	var count int
	if err = a.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM user_state WHERE user_id=$1`, apiUser).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted row resurrected", count, err)
	}
	// Foreign transaction ownership is still refused by the shared DB adapter.
	if _, err = postgres.NewOperations(a.DB, 5*time.Minute).SelectionKey(ctx, apiUser, "unknown", uuid.NewString()); !errors.Is(err, operations.ErrSelection) {
		t.Fatal(err)
	}
}
