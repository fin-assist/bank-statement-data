package operations

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/domain"
	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func ref[T any](v T) *T { return &v }
func moment(s string) time.Time {
	v, e := time.Parse(time.RFC3339, s)
	if e != nil {
		panic(e)
	}
	return v
}

var errStorage = errors.New("storage unavailable")

const paymentRef = "019c7714-3b77-74d1-9866-61f484aae201"
const merchantRef = "019c7714-3b77-74d1-9866-61f484aae202"

type memory struct {
	importing.Tombstones
	state                                                     Snapshot
	meta                                                      Metadata
	alive                                                     bool
	fail                                                      string
	reads, writes, guards, locks, saves, changes, assignments int
	from, to                                                  *time.Time
	locatedAt                                                 time.Time
	locatedID                                                 string
	actual                                                    *time.Time
	dismissed                                                 bool
	streamOptions                                             StreamOptions
	visits, snapshotRows, selections                          int
	streamCancel                                              context.CancelFunc
}

func (m *memory) Run(c context.Context, fn func(context.Context) error) error {
	m.writes++
	if m.fail == "write" {
		return errStorage
	}
	return fn(c)
}
func (m *memory) ReadOperations(c context.Context, fn func(context.Context) error) error {
	m.reads++
	if m.fail == "read" {
		return errStorage
	}
	return fn(c)
}
func (m *memory) Guard(context.Context, string) (bool, error) {
	m.guards++
	if m.fail == "guard" {
		return false, errStorage
	}
	return m.alive, nil
}
func (m *memory) Metadata(context.Context, string) (Metadata, error) {
	if m.fail == "metadata" {
		return Metadata{}, errStorage
	}
	return m.meta, nil
}
func (m *memory) Snapshot(_ context.Context, _ string, from, to *time.Time) (Snapshot, error) {
	m.from, m.to = from, to
	if m.fail == "snapshot" {
		return Snapshot{}, errStorage
	}
	s := m.state
	s.Operations = append([]importing.Operation(nil), s.Operations...)
	if from != nil || to != nil {
		pairs := map[string]bool{}
		inside := func(at time.Time) bool { return (from == nil || !at.Before(*from)) && (to == nil || at.Before(*to)) }
		for _, op := range s.Operations {
			if inside(op.At) && op.PairID != "" {
				pairs[op.PairID] = true
			}
		}
		filtered := s.Operations[:0]
		for _, op := range s.Operations {
			if inside(op.At) || op.PairID != "" && pairs[op.PairID] {
				filtered = append(filtered, op)
			}
		}
		s.Operations = filtered
	}
	m.snapshotRows = len(s.Operations)
	return s, nil
}

func (m *memory) SelectionKey(_ context.Context, _, kind, id string) (string, error) {
	m.selections++
	if m.fail == "selection" {
		return "", errStorage
	}
	key := ""
	var exists bool
	switch kind {
	case domain.ReferenceKindRegular:
		key, exists = m.state.Regular[id]
	case domain.ReferenceKindMerchant:
		key, exists = m.state.Merchants[id]
	}
	if !exists {
		return "", ErrSelection
	}
	return key, nil
}

func (m *memory) Stream(ctx context.Context, _ string, opts StreamOptions, visit func(Row) error) error {
	m.streamOptions = opts
	if m.fail == "stream" {
		return errStorage
	}
	if m.streamCancel != nil {
		m.streamCancel()
	}
	if m.fail == "stream-empty" {
		return visit(Row{})
	}
	zone, err := time.LoadLocation(opts.TimeZone)
	if err != nil {
		return ErrState
	}
	type stats struct {
		amounts      []int64
		months       []string
		count, total int64
	}
	regular := map[string]stats{}
	small := map[string]stats{}
	window := map[string]bool{}
	for _, month := range opts.RegularMonths {
		window[month] = true
	}
	inside := func(at time.Time, from, to *time.Time) bool {
		return (from == nil || !at.Before(*from)) && (to == nil || at.Before(*to))
	}
	for _, op := range m.state.Operations {
		if op.Direction != "expense" || op.Currency != "RUB" || op.PairID != "" || op.Category == importing.OwnTransferCategory || op.Category == importing.TransfersCategory || op.Description == "" {
			continue
		}
		month := op.At.In(zone).Format("2006-01")
		if window[month] {
			r := regular[op.Description]
			r.count++
			if r.count <= int64(len(opts.RegularMonths)) {
				r.amounts = append(r.amounts, op.AmountMinor)
				r.months = append(r.months, month)
			}
			regular[op.Description] = r
		}
		if opts.Small && inside(op.At, opts.SmallFrom, opts.SmallTo) {
			key := month + "\x00" + op.Description
			r := small[key]
			r.count++
			r.total += op.AmountMinor
			small[key] = r
		}
	}
	ops := append([]importing.Operation(nil), m.state.Operations...)
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].At.Equal(ops[j].At) {
			return ops[i].ID > ops[j].ID
		}
		return ops[i].At.After(ops[j].At)
	})
	for _, op := range ops {
		if op.PairID != "" && op.Direction == "income" || !inside(op.At, opts.From, opts.To) {
			continue
		}
		m.visits++
		row := Row{Facts: []importing.Operation{op}, Details: map[string]Detail{op.ID: m.state.Details[op.ID]}, Dismissed: m.state.Dismissed[op.Description]}
		if op.PairID != "" {
			for _, peer := range m.state.Operations {
				if peer.PairID == op.PairID && peer.Direction == "income" {
					row.Facts = append(row.Facts, peer)
					row.Details[peer.ID] = m.state.Details[peer.ID]
				}
			}
		}
		if m.fail == "stream-many" {
			row.Facts = append(row.Facts, op)
		}
		if m.fail == "stream-project" {
			row.Details = nil
		}
		r := regular[op.Description]
		if r.count <= int64(len(opts.RegularMonths)) {
			row.RegularAmounts, row.RegularMonths = r.amounts, r.months
		}
		g := small[op.At.In(zone).Format("2006-01")+"\x00"+op.Description]
		row.SmallCount, row.SmallTotal = g.count, g.total
		if err = visit(row); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (m *memory) Locate(_ context.Context, _ string, id string, at time.Time) (time.Time, error) {
	m.locatedID, m.locatedAt = id, at
	if m.fail == "locate" {
		return time.Time{}, errStorage
	}
	if m.actual != nil {
		return *m.actual, nil
	}
	for _, op := range m.state.Operations {
		if op.ID == id && op.PairID == "" || op.PairID == id && op.Direction == "expense" {
			return op.At, nil
		}
	}
	return time.Time{}, ErrNotFound
}
func (m *memory) LockUser(context.Context, string, bool) (string, error) {
	m.locks++
	if m.fail == "lock" {
		return "", errStorage
	}
	return m.state.TimeZone, nil
}
func (m *memory) Enqueue(context.Context, string, time.Time) error {
	m.saves++
	if m.fail == "save" {
		return errStorage
	}
	return nil
}

func (m *memory) SaveRules(_ context.Context, _ string, ops []importing.Operation, _ time.Time) error {
	m.saves++
	if m.fail == "save" {
		return errStorage
	}
	m.state.Operations = append([]importing.Operation(nil), ops...)
	return nil
}
func (m *memory) Changed(context.Context, string, time.Time) error {
	m.changes++
	if m.fail == "changed" {
		return errStorage
	}
	return nil
}
func (m *memory) SetDismissed(_ context.Context, _, _ string, d bool, _ time.Time) (bool, error) {
	m.assignments++
	if m.fail == "dismiss" {
		return false, errStorage
	}
	changed := m.dismissed != d
	m.dismissed = d
	return changed, nil
}
func fixture() *memory {
	cats := []Category{
		{Category: importing.Category{ID: importing.OwnTransferCategory, Name: "Между своими счетами", Icon: "transfer", Expense: true, Income: true}, Assignable: true, System: true},
		{Category: importing.Category{ID: importing.RefundCategory, Name: "Возврат", Icon: "repeat", Income: true}, Assignable: true, System: true},
		{Category: importing.Category{ID: importing.UncategorizedCategory, Name: "Без категории", Icon: "tag", Expense: true, Income: true}, System: true},
		{Category: importing.Category{ID: importing.TransfersCategory, Name: "Переводы", Icon: "users", Expense: true, Income: true, BankName: "Переводы"}, Assignable: true},
		{Category: importing.Category{ID: "food", Name: "Продукты", Icon: "cart", Expense: true, BankName: "Супермаркеты"}, Assignable: true},
		{Category: importing.Category{ID: "salary", Name: "Зарплата", Icon: "income", Income: true, BankName: "Зарплата"}, Assignable: true},
		{Category: importing.Category{ID: "taxi", Name: "Такси", Icon: "car", Expense: true, BankName: "Такси"}, Assignable: true},
	}
	m := &memory{alive: true, state: Snapshot{Snapshot: importing.Snapshot{TimeZone: "Europe/Moscow", Categories: map[string]importing.Category{}}, Accounts: map[int64]Account{1: {TypeName: "Текущий", Mask: ref("··4821"), Spoken: "текущий счёт"}, 2: {TypeName: "Накопительный", Mask: ref("··0734"), Spoken: "накопительный счёт"}}, Details: map[string]Detail{}, Catalog: cats, Regular: map[string]string{paymentRef: "Cloud"}, Merchants: map[string]string{merchantRef: "Coffee"}, Dismissed: map[string]bool{}}}
	for _, c := range cats {
		m.state.Categories[c.ID] = c.Category
	}
	m.meta.TimeZone = m.state.TimeZone
	return m
}
func (m *memory) add(id, at, kind, cat, name string, amount int64) {
	op := importing.Operation{ID: id, At: moment(at), Direction: kind, Category: cat, BankCategory: cat, Description: name, AmountMinor: amount, Currency: "RUB", AccountID: 1, AccountKey: "one", FITID: id}
	m.state.Operations = append(m.state.Operations, op)
	m.state.Details[id] = Detail{Status: "posted"}
	if m.meta.First == nil || op.At.Before(*m.meta.First) {
		m.meta.First = ref(op.At)
	}
	if m.meta.Last == nil || op.At.After(*m.meta.Last) {
		m.meta.Last = ref(op.At)
	}
}
func service(t *testing.T, m *memory, at string) *Service {
	t.Helper()
	s, e := New(m, m, m, func() time.Time { return moment(at) }, func() (string, error) { return "new-pair", nil })
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestUnitServiceDependencies(t *testing.T) {
	m := fixture()
	now := time.Now
	id := func() (string, error) { return "id", nil }
	for _, args := range []struct {
		tx      Transactions
		repo    Repository
		deleted importing.Tombstones
		now     func() time.Time
		id      func() (string, error)
	}{{nil, m, m, now, id}, {m, nil, m, now, id}, {m, m, nil, now, id}, {m, m, m, nil, id}, {m, m, m, now, nil}} {
		if _, e := New(args.tx, args.repo, args.deleted, args.now, args.id); e == nil {
			t.Fatal("nil dependency accepted")
		}
	}
}

func TestUnitFeedWindowCursorSummaryAndFreshness(t *testing.T) {
	m := fixture()
	m.add("old", "2025-01-02T12:00:00+03:00", "expense", "food", "Old", 3)
	m.add("expense", "2026-09-25T12:00:00+03:00", "expense", "food", "Shop", 1000)
	m.add("refund", "2026-09-26T12:00:00+03:00", "income", importing.RefundCategory, "Refund", 200)
	m.state.Operations[2].RefundTarget = "food"
	m.add("income", "2026-09-26T13:00:00+03:00", "income", "salary", "Pay", 5000)
	m.state.Uploads = []importing.Upload{{ID: "one", Status: "done", UploadedAt: moment("2026-09-27T10:00:00+03:00"), Period: &importing.Period{From: moment("2026-07-01T00:00:00+03:00"), To: moment("2026-09-26T00:00:00+03:00")}}, {ID: "processing", Status: "processing", UploadedAt: moment("2026-09-28T10:00:00+03:00")}, {ID: "failed", Status: "failed", UploadedAt: moment("2026-10-10T10:00:00+03:00")}}
	s := service(t, m, "2026-10-10T23:59:59+03:00")
	out, e := s.Feed(t.Context(), "user", nil)
	if e != nil {
		t.Fatal(e)
	}
	if out.Range.From.Format("2006-01-02") != "2026-07-01" || out.Range.To.Format("2006-01-02") != "2026-10-01" || out.NextBefore == nil || !out.NextBefore.Equal(out.Range.From) || len(out.Items) != 3 || out.Items[0].ID != "income" {
		t.Fatalf("window/order: %+v", out)
	}
	if len(out.Summary) != 3 || out.Summary[0].Expense != 800 || out.Summary[0].Income != 5000 || out.Summary[0].DataTo == nil || !out.Summary[0].DataTo.Equal(moment("2026-09-26T13:00:00+03:00")) || out.Summary[1].DataTo != nil {
		t.Fatalf("monthly accounting: %+v", out.Summary)
	}
	if out.State.Stale || !out.State.DataTo.Equal(moment("2026-09-27T00:00:00+03:00")) || !out.State.LastUpload.Equal(moment("2026-09-28T10:00:00+03:00")) {
		t.Fatalf("freshness: %+v", out.State)
	}
	s = service(t, m, "2026-10-11T00:00:00+03:00")
	out, e = s.Feed(t.Context(), "user", out.NextBefore)
	if e != nil || !out.State.Stale || out.Range.From.Month() != time.April || len(out.Items) != 0 || out.NextBefore == nil {
		t.Fatalf("empty intermediate cursor: %+v %v", out, e)
	}
}

func TestUnitFeedEmptyCoverageAndErrors(t *testing.T) {
	m := fixture()
	s := service(t, m, "2026-10-27T10:00:00+03:00")
	out, e := s.Feed(t.Context(), "user", nil)
	if e != nil || len(out.Items) != 0 || out.NextBefore != nil || !out.State.Stale || out.State.DataTo != nil || out.Range.From.Month() != time.August {
		t.Fatalf("empty: %+v %v", out, e)
	}
	m.state.Uploads = []importing.Upload{{ID: "empty", Status: "done", Period: &importing.Period{From: moment("2026-10-01T00:00:00+03:00"), To: moment("2026-10-27T00:00:00+03:00")}}}
	out, e = s.Feed(t.Context(), "user", nil)
	if e != nil || out.State.Stale || out.State.LastOperation != nil || out.State.DataTo == nil || len(out.Summary) != 1 {
		t.Fatalf("covered empty: %+v %v", out, e)
	}
	_, e = s.Feed(t.Context(), "user", ref(moment("2026-10-02T00:00:00+03:00")))
	var input *importing.InputError
	if !errors.As(e, &input) || input.Field != "before" {
		t.Fatalf("cursor validation: %v", e)
	}
	_, e = s.Feed(t.Context(), "user", ref(moment("0001-01-01T00:00:00Z")))
	if !errors.As(e, &input) {
		t.Fatalf("cursor year boundary: %v", e)
	}
	for _, failure := range []string{"read", "metadata", "snapshot"} {
		m.fail = failure
		_, e = s.Feed(t.Context(), "user", nil)
		if !errors.Is(e, errStorage) {
			t.Fatalf("%s: %v", failure, e)
		}
	}
	m.fail = ""
	m.meta.TimeZone = "invalid"
	_, e = s.Feed(t.Context(), "user", nil)
	if !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
	m.meta.TimeZone = "Europe/Moscow"
	m.state.TimeZone = "invalid"
	_, e = s.Feed(t.Context(), "user", nil)
	if !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
}

func TestUnitFeedAnchorsAtVisiblePairDebitRatherThanLaterCredit(t *testing.T) {
	m := pairFixture()
	s := service(t, m, "2026-10-27T12:00:00+03:00")
	out, err := s.Feed(t.Context(), "user", nil)
	if err != nil || len(out.Items) != 1 || out.Items[0].ID != "pair" || out.Range.To.Format("2006-01-02") != "2026-10-01" || out.Range.From.Format("2006-01-02") != "2026-07-01" || out.NextBefore != nil || !out.State.LastOperation.Equal(m.state.Operations[1].At) {
		t.Fatalf("cross-month pair created empty latest portion or lost raw freshness timestamp: %+v %v", out, err)
	}
}

func TestUnitGetAndCategories(t *testing.T) {
	m := fixture()
	m.add("op", "2026-09-25T12:00:00.123456+03:00", "expense", "food", "Пятёрочка", 1000)
	s := service(t, m, "2026-10-27T12:00:00+03:00")
	stale := moment("2026-01-01T00:00:00Z")
	out, zone, e := s.Get(t.Context(), "user", "op", stale)
	if e != nil || out.ID != "op" || zone != "Europe/Moscow" || out.Description == nil || out.Account.Spoken == "" || !m.locatedAt.Equal(stale) || !m.from.Equal(out.At) || !m.to.Equal(out.At.Add(time.Microsecond)) {
		t.Fatalf("fallback/detail: %+v %s %v", out, zone, e)
	}
	cats, e := s.Categories(t.Context(), "user")
	if e != nil || len(cats) != len(m.state.Catalog) || m.from.Year() != 1 || !m.from.Equal(*m.to) {
		t.Fatalf("categories: %+v %v", cats, e)
	}
	_, _, e = s.Get(t.Context(), "user", "foreign", stale)
	if !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	m.actual = ref(moment("2026-09-26T00:00:00Z"))
	_, _, e = s.Get(t.Context(), "user", "op", stale)
	if !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	m.actual = nil
	for _, failure := range []string{"read", "locate", "snapshot"} {
		m.fail = failure
		_, _, e = s.Get(t.Context(), "user", "op", stale)
		if !errors.Is(e, errStorage) {
			t.Fatalf("%s: %v", failure, e)
		}
	}
	m.fail = "snapshot"
	if _, e = s.Categories(t.Context(), "user"); !errors.Is(e, errStorage) {
		t.Fatal(e)
	}
	m.fail = ""
	delete(m.state.Accounts, 1)
	if _, _, e = s.Get(t.Context(), "user", "op", stale); !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
}

func TestUnitChangeOrdinaryIdempotencyAndDependentRefund(t *testing.T) {
	m := fixture()
	m.add("buy", "2026-08-20T12:00:00+03:00", "expense", "food", "Shop", 1000)
	m.add("refund", "2026-09-01T12:00:00+03:00", "income", importing.RefundCategory, "Shop", 300)
	m.state.Operations[1].RefundTarget = "food"
	m.state.Operations[1].BankCategory = "food"
	s := service(t, m, "2026-10-27T12:00:00+03:00")
	out, e := s.Change(t.Context(), "user", "buy", "taxi", m.state.Operations[0].At)
	if e != nil || len(out.Items) != 1 || out.Items[0].Category.ID != "taxi" || !out.Items[0].Manual || out.ReplacedID != nil || m.saves != 1 || m.changes != 1 || m.guards != 1 || m.locks != 1 {
		t.Fatalf("change: %+v %v %+v", out, e, m)
	}
	if m.state.Operations[1].RefundTarget != "taxi" {
		t.Fatalf("dependent refund not reclassified: %+v", m.state.Operations[1])
	}
	out, e = s.Change(t.Context(), "user", "buy", "taxi", m.state.Operations[0].At)
	if e != nil || m.saves != 1 || m.changes != 1 || len(out.Items) != 1 {
		t.Fatalf("idempotent assignment: %+v %v", out, e)
	}
	_, e = s.Change(t.Context(), "user", "buy", "salary", m.state.Operations[0].At)
	if !errors.Is(e, ErrCategory) {
		t.Fatal(e)
	}
	_, e = s.Change(t.Context(), "user", "buy", importing.UncategorizedCategory, m.state.Operations[0].At)
	if !errors.Is(e, ErrCategory) {
		t.Fatal(e)
	}
}

func pairFixture() *memory {
	m := fixture()
	m.add("debit", "2026-09-30T23:59:00+03:00", "expense", importing.OwnTransferCategory, "Между своими счетами", 1000)
	m.add("credit", "2026-10-01T00:01:00+03:00", "income", importing.OwnTransferCategory, "Между своими счетами", 1000)
	for i := range m.state.Operations {
		m.state.Operations[i].PairID = "pair"
		m.state.Operations[i].BankCategory = importing.TransfersCategory
	}
	m.state.Operations[1].AccountID = 2
	m.state.Operations[1].AccountKey = "two"
	m.meta.FirstVisible = ref(m.state.Operations[0].At)
	m.meta.LastVisible = ref(m.state.Operations[0].At)
	return m
}

func TestUnitChangePairSplitAndMerge(t *testing.T) {
	for _, category := range []string{"food", "salary", importing.TransfersCategory} {
		t.Run(category, func(t *testing.T) {
			m := pairFixture()
			s := service(t, m, "2026-10-27T12:00:00+03:00")
			out, e := s.Change(t.Context(), "user", "pair", category, m.state.Operations[0].At)
			if e != nil || len(out.Items) != 2 || out.ReplacedID == nil || *out.ReplacedID != "pair" {
				t.Fatalf("split: %+v %v", out, e)
			}
			manual := 0
			for _, op := range m.state.Operations {
				if op.PairID != "" || !op.Split {
					t.Fatalf("pair retained: %+v", op)
				}
				if op.ManualCategory != "" {
					manual++
					if op.ManualCategory != category || category == "salary" && op.Direction != "income" || category != "salary" && op.Direction != "expense" {
						t.Fatalf("wrong assigned side: %+v", op)
					}
				}
			}
			if manual != 1 {
				t.Fatalf("manual sides: %d", manual)
			}
		})
	}
	m := pairFixture()
	for i := range m.state.Operations {
		m.state.Operations[i].PairID = ""
		m.state.Operations[i].Category = importing.TransfersCategory
		m.state.Operations[i].Description = "Transfer"
	}
	s := service(t, m, "2026-10-27T12:00:00+03:00")
	out, e := s.Change(t.Context(), "user", "debit", importing.OwnTransferCategory, m.state.Operations[0].At)
	if e != nil || len(out.Items) != 1 || out.Items[0].Kind != "own_transfer" || out.Items[0].ID != "new-pair" || out.ReplacedID == nil || *out.ReplacedID != "debit" {
		t.Fatalf("merge: %+v %v", out, e)
	}
	m = pairFixture()
	s = service(t, m, "2026-10-27T12:00:00+03:00")
	out, e = s.Change(t.Context(), "user", "pair", importing.OwnTransferCategory, m.state.Operations[0].At)
	if e != nil || out.ReplacedID != nil || len(out.Items) != 1 || m.saves != 0 {
		t.Fatalf("own-transfer reassign: %+v %v", out, e)
	}
}

func TestUnitChangeAndDismissFailures(t *testing.T) {
	for _, failure := range []string{"write", "guard", "lock", "locate", "snapshot", "save", "changed"} {
		t.Run("change_"+failure, func(t *testing.T) {
			m := fixture()
			m.add("op", "2026-09-01T12:00:00Z", "expense", "food", "Shop", 10)
			m.fail = failure
			s := service(t, m, "2026-10-27T12:00:00Z")
			_, e := s.Change(t.Context(), "user", "op", "taxi", m.state.Operations[0].At)
			if !errors.Is(e, errStorage) {
				t.Fatal(e)
			}
		})
	}
	m := fixture()
	m.alive = false
	s := service(t, m, "2026-10-27T12:00:00Z")
	_, e := s.Change(t.Context(), "user", "id", "food", time.Time{})
	if !errors.Is(e, importing.ErrDeleted) || m.locks != 0 || m.saves != 0 {
		t.Fatalf("deleted guard: %v %+v", e, m)
	}
	m = fixture()
	m.actual = ref(moment("2026-09-01T12:00:00Z"))
	s = service(t, m, "2026-10-27T12:00:00Z")
	_, e = s.Change(t.Context(), "user", "missing", "food", time.Time{})
	if !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	for _, failure := range []string{"write", "guard", "lock", "dismiss", "save", "changed"} {
		t.Run("dismiss_"+failure, func(t *testing.T) {
			m := fixture()
			m.fail = failure
			s := service(t, m, "2026-10-27T12:00:00Z")
			if e := s.Dismiss(t.Context(), "user", "payment", true); !errors.Is(e, errStorage) {
				t.Fatal(e)
			}
		})
	}
	m = fixture()
	s = service(t, m, "2026-10-27T12:00:00Z")
	for _, d := range []bool{true, true, false, false} {
		if e = s.Dismiss(t.Context(), "user", "payment", d); e != nil {
			t.Fatal(e)
		}
	}
	if m.saves != 2 || m.changes != 2 || m.assignments != 4 || m.dismissed {
		t.Fatalf("dismiss assignments/idempotency: %+v", m)
	}
	m.alive = false
	if e = s.Dismiss(t.Context(), "user", "payment", true); !errors.Is(e, importing.ErrDeleted) || m.assignments != 4 {
		t.Fatalf("late dismissal: %v", e)
	}
}

func TestUnitProjectPairsRefundsAndCorruptState(t *testing.T) {
	m := pairFixture()
	m.state.Operations[0].ManualCategory = importing.OwnTransferCategory
	m.state.Details["credit"] = Detail{Status: "pending"}
	out, e := project(t.Context(), m.state, true)
	if e != nil || len(out) != 1 || out[0].ID != "pair" || out[0].Status != "pending" || out[0].At != m.state.Operations[0].At || out[0].Account != nil || out[0].FromAccount == nil || out[0].ToAccount == nil || out[0].Description != nil || out[0].Manual || out[0].NoteDetails == nil {
		t.Fatalf("pair: %+v %v", out, e)
	}
	for _, change := range []func(*memory){func(m *memory) { m.state.TimeZone = "bad" }, func(m *memory) { delete(m.state.Categories, importing.OwnTransferCategory) }, func(m *memory) { m.state.Operations[0].AmountMinor = -1 }, func(m *memory) { delete(m.state.Accounts, 1) }, func(m *memory) { delete(m.state.Details, "debit") }, func(m *memory) { m.state.Operations = m.state.Operations[:1] }, func(m *memory) { m.state.Operations[1].AmountMinor++ }, func(m *memory) { delete(m.state.Accounts, 2) }} {
		bad := pairFixture()
		change(bad)
		if _, e = project(t.Context(), bad.state, true); !errors.Is(e, ErrState) {
			t.Fatalf("bad project accepted: %+v %v", bad.state, e)
		}
	}
	m = fixture()
	m.add("refund", "2026-09-01T12:00:00Z", "income", importing.RefundCategory, "Shop", 10)
	m.state.Operations[0].RefundTarget = "food"
	out, e = project(t.Context(), m.state, true)
	if e != nil || out[0].Note == nil || *out[0].Note != "возврат · Продукты" || out[0].NoteDetails == nil || *out[0].NoteDetails != "Уменьшает расходы «Продукты» в сентябре" {
		t.Fatalf("refund: %+v %v", out, e)
	}
	m.state.Operations[0].RefundTarget = importing.UncategorizedCategory
	out, e = project(t.Context(), m.state, true)
	if e != nil || out[0].Note == nil || *out[0].Note != "возврат" || out[0].NoteDetails == nil || *out[0].NoteDetails != "Уменьшает общие расходы в сентябре" {
		t.Fatalf("unmatched refund: %+v %v", out, e)
	}
	m.state.Operations[0].RefundTarget = "food"
	delete(m.state.Categories, "food")
	if _, e = project(t.Context(), m.state, true); !errors.Is(e, ErrState) {
		t.Fatal(e)
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e = project(c, pairFixture().state, false); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestUnitChangeCalculationAndProjectionFailure(t *testing.T) {
	for _, mutate := range []func(*memory, *Service){func(m *memory, _ *Service) { m.state.Operations[0].BankCategory = "missing" }, func(m *memory, _ *Service) { delete(m.state.Accounts, 1) }, func(m *memory, s *Service) {
		m.state.Operations[0].Category = importing.OwnTransferCategory
		m.state.Operations[0].Description = "Между своими счетами"
		m.state.Operations[1].Category = importing.OwnTransferCategory
		m.state.Operations[1].Description = "Между своими счетами"
		s.newID = func() (string, error) { return "", errStorage }
	}} {
		m := pairFixture()
		for i := range m.state.Operations {
			m.state.Operations[i].PairID = ""
		}
		s := service(t, m, "2026-10-27T12:00:00Z")
		mutate(m, s)
		if _, e := s.Change(t.Context(), "user", "debit", importing.OwnTransferCategory, m.state.Operations[0].At); e == nil || m.saves != 0 {
			t.Fatalf("unsafe change persisted: %v", e)
		}
	}
}
