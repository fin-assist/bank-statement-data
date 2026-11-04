package importing

import (
	"testing"
	"time"
)

const closureOperationSource = "operations"

func TestUnitUnreadDateClosureRequiresExplicitSourceCoverage(t *testing.T) {
	for _, source := range []string{"", closureOperationSource, "none", "unrecognized", closureTagSource} {
		t.Run(source, func(t *testing.T) {
			old, newer := closureUpload("old", 1), closureUpload("new", 2)
			newer.Period.Source = source
			for _, day := range []int{0, 15} {
				line := closureLine(old.ID, "", day)
				got, err := ResolveUnread(t.Context(), []Upload{old, newer}, []Unread{line}, nil, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if source == closureTagSource {
					want = "read"
				}
				if got[0].Resolution != want {
					t.Fatalf("source=%q day=%d: resolution=%q, want %q", source, day, got[0].Resolution, want)
				}
			}
			// Source-key evidence is independent of the coverage envelope.
			line := closureLine(old.ID, "source-key", 15)
			got, err := ResolveUnread(t.Context(), []Upload{old, newer}, []Unread{line}, []Source{{UploadID: newer.ID, AccountID: 1, FITID: line.FITID}}, time.UTC)
			if err != nil || got[0].Resolution != "read" {
				t.Fatal("FITID evidence changed", got, err)
			}
		})
	}
}

func TestUnitUnreadWithoutDateRequiresExplicitOldSourceCoverage(t *testing.T) {
	old, newer := closureUpload("old", 1), closureUpload("new", 2)
	for _, source := range []string{"", closureOperationSource} {
		old.Period.Source = source
		got, err := ResolveUnread(t.Context(), []Upload{old, newer}, []Unread{closureLine(old.ID, "", 0)}, nil, time.UTC)
		if err != nil || got[0].Resolution != "" {
			t.Fatal("unknown old source envelope became coverage proof", got, err)
		}
	}
}

func TestUnitOperationFallbackDoesNotCloseMissingSourceKey(t *testing.T) {
	old, newer := closureUpload("old", 1), closureUpload("new", 2)
	first, last := closureDay(1), closureDay(30)
	newer.Period = statementPeriod(Document{Lines: []Line{{OccurredAt: &first}, {OccurredAt: &last}}}, time.UTC)
	got, err := ResolveUnread(t.Context(), []Upload{old, newer}, []Unread{closureLine(old.ID, "", 15)}, nil, time.UTC)
	if err != nil || got[0].Resolution != "" {
		t.Fatal("first/last operations falsely proved the missing day", got, err)
	}
}
