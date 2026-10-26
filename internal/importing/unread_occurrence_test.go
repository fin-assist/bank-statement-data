package importing_test

import (
	"testing"
	"time"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func TestUnitUnreadKeepsSameLineFactsInStableOccurrenceOrder(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	state := importing.Snapshot{TimeZone: "UTC", Uploads: []importing.Upload{{ID: "upload", Status: "done", UploadedAt: now}}, Unread: []importing.Unread{
		{UploadID: "upload", Number: 1, Occurrence: 2, Reason: "bad_amount"},
		{UploadID: "upload", Number: 1, Occurrence: 1, Reason: "no_date"},
	}}
	f := newImportReadFixture(t, state, now)
	rows, _, err := f.service.Unread(t.Context(), "actor", importing.Scope{UploadID: "upload"})
	if err != nil || len(rows) != 2 || rows[0].Number != 1 || rows[1].Number != 1 || rows[0].Occurrence != 1 || rows[1].Occurrence != 2 {
		t.Fatal("physical facts collapsed or lost their stable order", rows, err)
	}
}
