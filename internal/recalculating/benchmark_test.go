package recalculating

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func BenchmarkFIN357ThirtyYearCalendar(b *testing.B) {
	zone, _ := time.LoadLocation("Europe/Moscow")
	from := time.Date(1996, 1, 1, 12, 0, 0, 0, zone)
	to := time.Date(2025, 12, 31, 12, 0, 0, 0, zone)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, zone)
	for _, monthly := range []int{0, 100} {
		b.Run(fmt.Sprintf("%d_ops_per_month", monthly), func(b *testing.B) {
			snapshot := importing.Snapshot{TimeZone: zone.String(), Categories: map[string]importing.Category{"cat_food": {ID: "cat_food", Name: "Продукты", Icon: "tag", Expense: true, Income: true}}, Uploads: []importing.Upload{{ID: "file", Status: "done", Period: &importing.Period{From: from, To: to}}}}
			for m := from; !m.After(to); m = m.AddDate(0, 1, 0) {
				for day := range monthly {
					snapshot.Operations = append(snapshot.Operations, importing.Operation{ID: fmt.Sprint(len(snapshot.Operations) + 1), At: m.AddDate(0, 0, day%28), AmountMinor: 100000, Currency: "RUB", Direction: "expense", Category: "cat_food", BankCategory: "cat_food", Description: "Store"})
				}
			}
			repo := &repoFake{lease: Lease{User: "u", Queue: []QueueRow{{Month: "2025-12-01", Generation: 1}}}, active: true, snapshot: snapshot}
			publish := &publisherFake{}
			service, err := New(&txFake{}, repo, guardFake{live: true}, publish, func() time.Time { return now }, coverageFake{ready: true})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				repo.active = true
				repo.lease.Recalculating = false
				publish.values = nil
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				err := service.Recalculate(ctx, "u")
				cancel()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
