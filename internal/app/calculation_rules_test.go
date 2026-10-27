package app_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fin-assist/bank-statement-data/internal/app"
	"github.com/fin-assist/bank-statement-data/internal/calculating"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func ruleOFXHeader(from, to time.Time) string {
	stamp := func(at time.Time) string {
		_, offset := at.Zone()
		return at.Format("20060102150405") + fmt.Sprintf("[%+d:TEST]", offset/3600)
	}
	return `<?xml version="1.0" encoding="utf-8"?>
<?OFX OFXHEADER="200" VERSION="202" SECURITY="NONE" OLDFILEUID="NONE" NEWFILEUID="NONE"?>
<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS><BANKACCTFROM><BANKID>T-BANK</BANKID><ACCTID>40817810000000001111</ACCTID><ACCTTYPE>CHECKING</ACCTTYPE></BANKACCTFROM><BANKTRANLIST><DTSTART>` + stamp(from) + `</DTSTART><DTEND>` + stamp(to) + `</DTEND>`
}
func ruleOFXRow(index int, at time.Time, amount int64, name, category string) string {
	kind := "CREDIT"
	if amount < 0 {
		kind = "DEBIT"
	}
	_, offset := at.Zone()
	stamp := at.Format("20060102150405") + fmt.Sprintf("[%+d:TEST]", offset/3600)
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}
	return fmt.Sprintf("<STMTTRN><TRNTYPE>%s</TRNTYPE><DTPOSTED>%s</DTPOSTED><TRNAMT>%s%d.%02d</TRNAMT><FITID>rule-%d</FITID><NAME>%s</NAME><MEMO>%s</MEMO></STMTTRN>\n", kind, stamp, sign, amount/100, amount%100, index, name, category)
}

const ruleOFXEnd = `</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>`

// This small controlled ledger has hand-computed expectations independent of the
// calculator: each full base month costs 100000+29900+9*28000 = 381900 minor.
func calculationRuleOFX() string {
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, zone)
	sept := time.Date(2026, 9, 1, 0, 0, 0, 0, zone)
	var body strings.Builder
	body.WriteString(ruleOFXHeader(from, sept.AddDate(0, 0, 24)))
	index := 0
	add := func(at time.Time, amount int64, name, category string) {
		index++
		body.WriteString(ruleOFXRow(index, at, amount, name, category))
	}
	for month := from; month.Before(sept); month = month.AddDate(0, 1, 0) {
		add(month.AddDate(0, 0, 14), -29900, "Cloud", "Супермаркеты")
		add(month.AddDate(0, 0, 2), -100000, "Taxi", "Такси")
		for i := 0; i < 9; i++ {
			add(month.AddDate(0, 0, i), -28000, "Coffee", "Супермаркеты")
		}
	}
	add(sept, -720000, "Taxi", "Такси")
	add(sept.AddDate(0, 0, 3), 4000000, "Salary", "Зарплата")
	add(sept.AddDate(0, 0, 14), -29900, "Cloud", "Супермаркеты")
	add(sept, -5000, "КОМИССИЯ за перевод", "Супермаркеты")
	add(sept, -9000, "Bank", "Услуги банка")
	add(sept, -100000, "Transfer", "Переводы")
	add(sept, -777777, "Себе в другой банк", "Переводы")
	add(sept.AddDate(0, 0, 1), 120000, "Coffee", "Возврат")
	body.WriteString(ruleOFXEnd)
	return body.String()
}
func importCalculationBody(t *testing.T, a *app.App, user, body string) {
	t.Helper()
	accepted, err := a.Imports.Upload(t.Context(), user, importing.Header{FileName: "rules.ofx", SizeBytes: int64(len(body))}, bytes.NewBufferString(body), []string{uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Imports.Process(t.Context(), user, accepted.UploadID); err != nil {
		t.Fatal(err)
	}
}
func calculationMetricValue(t *testing.T, p calculating.Period, id, status string, want int64) {
	t.Helper()
	m := p.Metrics[id]
	if m.Status != status || m.Value == nil || *m.Value != want {
		t.Fatalf("%s = %+v want %s %d", id, m, status, want)
	}
}
func calculationSelection(t *testing.T, a *app.App, user string, p calculating.Period, f calculating.Filter, wantCount int, wantSum int64) {
	t.Helper()
	if f.Mode != p.Mode || f.Range.From.IsZero() || !f.Range.From.Before(f.Range.To) {
		t.Fatal("missing ready selection scope", f)
	}
	key := ""
	groups := map[string]bool{}
	switch {
	case strings.HasPrefix(f.Selection, "s_regular_") && f.Selection != "s_regular_all":
		id := strings.TrimPrefix(f.Selection, "s_regular_")
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
			t.Fatal("regular selection is not an opaque UUID")
		}
		if err := a.DB.Primary().QueryRow(t.Context(), `SELECT description FROM regular_payment WHERE user_id=$1 AND id=$2`, user, id).Scan(&key); err != nil {
			t.Fatal(err)
		}
	case strings.HasPrefix(f.Selection, "s_small_merchant_"):
		id := strings.TrimPrefix(f.Selection, "s_small_merchant_")
		if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
			t.Fatal("merchant selection is not an opaque UUID")
		}
		if err := a.DB.Primary().QueryRow(t.Context(), `SELECT key FROM merchant WHERE user_id=$1 AND id=$2`, user, id).Scan(&key); err != nil {
			t.Fatal(err)
		}
	case f.Selection == "s_regular_all":
		groups = map[string]bool{"Cloud": true, "Taxi": true}
	case strings.HasPrefix(f.Selection, "s_small_category_"):
		key = strings.TrimPrefix(f.Selection, "s_small_category_")
		groups = map[string]bool{"Coffee": true}
	}
	state := importSnapshot(t, a, user)
	count := 0
	var sum int64
	for _, op := range state.Operations {
		if calculating.Match(op, f, key, groups) {
			_, _, amount, ok := calculating.FinancialRow(op, p.Mode)
			if !ok {
				t.Fatal("selection admitted nonfinancial row")
			}
			count++
			sum += amount
		}
	}
	if count != wantCount || sum != wantSum {
		t.Fatalf("selection %+v: %d / %d want %d / %d", f, count, sum, wantCount, wantSum)
	}
}
func calculationRuleChecks(t *testing.T, a *app.App, user string) {
	t.Helper()
	result, err := a.Calculations.Calculate(t.Context(), user, "month", calculationDate(t, "2026-09"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Periods) != 2 {
		t.Fatal("missing mode pair")
	}
	for _, p := range result.Periods {
		wantExpense := int64(743900)
		if p.Mode == "without" {
			wantExpense = 643900
		}
		calculationMetricValue(t, p, "tiles.expense", "ready", wantExpense)
		calculationMetricValue(t, p, "tiles.expense.comparison", "ready", 381900)
		calculationMetricValue(t, p, "tiles.daily_expense.comparison", "ready", 12319)
		calculationMetricValue(t, p, "tiles.expense.typical", "ready", 381900)
		comparison := p.Metrics["tiles.expense.comparison"]
		if comparison.Range == nil || comparison.Range.From.Month() != time.August || comparison.Range.To.Day() != 26 {
			t.Fatal("comparison did not use previous period to same covered day")
		}
		calculationSelection(t, a, user, p, *comparison.Filter, 11, 381900)
		typical := p.Metrics["tiles.expense.typical"]
		if typical.Basis == nil || typical.Basis.FullMonths != 6 || typical.Basis.Range.From.Month() != time.March || typical.Basis.Range.To.Month() != time.September {
			t.Fatal("typical basis wrong")
		}
		regular := p.Metrics["insights.regular_payments"]
		calculationMetricValue(t, p, "insights.regular_payments", "ready", 129900)
		if len(regular.Payments) != 2 || regular.Payments[0].Title != "Taxi" || regular.Payments[0].Amount != 100000 || regular.Payments[1].Title != "Cloud" || regular.Payments[1].Amount != 29900 {
			t.Fatal("regular medians/order", regular)
		}
		// The contract deliberately opens current charges, not median values in the base.
		calculationSelection(t, a, user, p, *regular.Filter, 2, 749900)
		for _, payment := range regular.Payments {
			want := int64(29900)
			if payment.Title == "Taxi" {
				want = 720000
			}
			calculationSelection(t, a, user, p, payment.Filter, 1, want)
		}
		fees := p.Metrics["insights.bank_fees"]
		calculationMetricValue(t, p, "insights.bank_fees", "ready", 14000)
		calculationSelection(t, a, user, p, *fees.Filter, 2, 14000)
		notable := p.Metrics["insights.notable_spending"]
		if notable.Status != "ready" || len(notable.Notable) != 1 || notable.Notable[0].Name != "Такси" || notable.Notable[0].Amount != 720000 || notable.Notable[0].Typical != 100000 || notable.Basis == nil || notable.Basis.FullMonths != 3 || notable.Basis.Range.From.Month() != time.June {
			t.Fatal("notable amount/typical/basis", notable)
		}
		calculationSelection(t, a, user, p, notable.Notable[0].Filter, 1, 720000)
		small := p.Metrics["insights.small_frequent"]
		calculationMetricValue(t, p, "insights.small_frequent", "ready", 252000)
		if len(small.Small) != 1 || small.Small[0].Title != "Coffee" || small.Small[0].Count != 9 || small.Small[0].Average != 28000 || small.Small[0].Yearly != 3024000 || small.Basis == nil || small.Basis.Range.From.Month() != time.August {
			t.Fatal("small count/average/yearly/basis", small)
		}
		calculationSelection(t, a, user, p, small.Small[0].Filter, 9, 252000)
		for _, id := range []string{"expense_categories", "income_categories"} {
			for _, category := range p.Metrics[id].Categories {
				calculationSelection(t, a, user, p, category.Filter, int(category.Count), category.Amount)
			}
		}
	}
}
func TestIntegrationCalculationRulesAndSelectionsHaveIndependentNumbers(t *testing.T) {
	a, _ := importAssembly(t, calculationNow)
	importCalculationBody(t, a, apiUser, calculationRuleOFX())
	calculationRuleChecks(t, a, apiUser)
}

func calculationApiaBody(t *testing.T) string {
	t.Helper()
	zone, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	return ruleOFXHeader(time.Date(2011, 12, 1, 0, 0, 0, 0, zone), time.Date(2011, 12, 31, 23, 59, 59, 0, zone)) + ruleOFXRow(1, time.Date(2011, 12, 29, 12, 0, 0, 0, zone), -30000, "Apia", "Супермаркеты") + ruleOFXEnd
}
func calculationApiaNow() time.Time {
	zone, _ := time.LoadLocation("Pacific/Apia")
	return time.Date(2012, 1, 1, 12, 0, 0, 0, zone)
}
func calculationApiaCheck(t *testing.T, a *app.App) {
	t.Helper()
	zone, _ := time.LoadLocation("Pacific/Apia")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result, err := a.Calculations.Calculate(ctx, apiUser, "month", time.Date(2011, 12, 15, 12, 0, 0, 0, zone))
	if err != nil {
		t.Fatal(err)
	}
	if result.State.FullMonths != 1 || result.Periods[0].Bounds.Coverage != "complete" {
		t.Fatal("skipped date broke complete month")
	}
	calculationMetricValue(t, result.Periods[0], "tiles.daily_expense", "ready", 1000)
}
func TestIntegrationCalculationApiaImportAndFullMonthTerminate(t *testing.T) {
	a, _ := importAssembly(t, calculationApiaNow)
	if err := a.Imports.SetTimezone(t.Context(), apiUser, "Pacific/Apia", 1); err != nil {
		t.Fatal(err)
	}
	importCalculationBody(t, a, apiUser, calculationApiaBody(t))
	calculationApiaCheck(t, a)
}

func calculationMidnightNow() time.Time {
	zone, _ := time.LoadLocation("America/Havana")
	return time.Date(2012, 5, 1, 12, 0, 0, 0, zone)
}
func calculationMidnightBody(t *testing.T) string {
	t.Helper()
	zone, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Fatal(err)
	}
	return ruleOFXHeader(time.Date(2012, 4, 1, 1, 0, 0, 0, zone), time.Date(2012, 4, 30, 23, 59, 59, 0, zone)) + ruleOFXRow(1, time.Date(2012, 4, 29, 12, 0, 0, 0, zone), -30000, "Midnight", "Супермаркеты") + ruleOFXEnd
}
func calculationMidnightCheck(t *testing.T, a *app.App) {
	t.Helper()
	zone, _ := time.LoadLocation("America/Havana")
	result, err := a.Calculations.Calculate(t.Context(), apiUser, "month", time.Date(2012, 4, 15, 12, 0, 0, 0, zone))
	if err != nil {
		t.Fatal(err)
	}
	if result.State.From.Day() != 1 || result.State.From.Month() != time.April || result.State.From.Hour() != 1 || result.State.FullMonths != 1 {
		t.Fatalf("SQL date moved to previous calendar month: %+v", result.State)
	}
	chart := result.Periods[0].Metrics["monthly_chart"]
	if chart.Status != "locked" || chart.Lock.Available != 1 {
		t.Fatal("midnight gap invented a second month", chart)
	}
	calculationMetricValue(t, result.Periods[0], "tiles.daily_expense", "ready", 1000)
}
func TestIntegrationCalculationMidnightGapPreservesSQLCalendarDate(t *testing.T) {
	a, _ := importAssembly(t, calculationMidnightNow)
	if err := a.Imports.SetTimezone(t.Context(), apiUser, "America/Havana", 1); err != nil {
		t.Fatal(err)
	}
	importCalculationBody(t, a, apiUser, calculationMidnightBody(t))
	calculationMidnightCheck(t, a)
}
