package accounting

import "testing"

func TestUnitCatalogHasIndependentSharedHistoryGates(t *testing.T) {
	features := Features()
	features[0].FullMonths = 99
	if Threshold("comparison").FullMonths != 1 || Threshold("monthly_chart").MonthsWithData != 2 || Threshold("notable_spending").FullMonths != 3 || Threshold("unknown").FullMonths != 0 {
		t.Fatal("shared gates changed")
	}
	if !FeeWord("комиссию") || !FeeWord("процентом") || FeeWord("комиссионный") {
		t.Fatal("whole forms changed")
	}
}
