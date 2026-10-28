// Package accounting owns the shared, versioned financial definition catalog.
package accounting

import "time"

const (
	PairWindow                     = 72 * time.Hour
	RefundWindow                   = 60 * 24 * time.Hour
	RegularWindow                  = 3
	RegularTolerancePercent  int64 = 15
	TypicalWindow                  = 6
	StaleDays                      = 14
	CategoryNameLimit              = 64
	NotableNumerator         int64 = 3
	NotableDenominator       int64 = 2
	ForecastDay                    = 10
	NotableMinimumMinor      int64 = 100000
	SmallMinimumCount              = 9
	SmallMaximumAverageMinor int64 = 50000
)

// Feature is an import availability gate, also used by metric locks.
type Feature struct {
	Name                       string
	FullMonths, MonthsWithData int
}

// Features returns an independent copy: consumers cannot mutate the catalog.
func Features() []Feature {
	return []Feature{
		{"comparison", 1, 0}, {"monthly_chart", 0, 2}, {"typical", 2, 0},
		{"regular_payments", 2, 0}, {"notable_spending", 3, 0}, {"small_frequent", 1, 0}, {"year_forecast", 12, 0},
	}
}

// Threshold returns the shared history requirements for a known metric.
func Threshold(name string) Feature {
	for _, f := range Features() {
		if f.Name == name {
			return f
		}
	}
	return Feature{Name: name}
}

// FeeWord matches whole, normalized words only; callers split on non-letters.
func FeeWord(word string) bool {
	for _, form := range feeWords() {
		if word == form {
			return true
		}
	}
	return false
}

func feeWords() []string {
	return []string{"комиссия", "комиссии", "комиссию", "комиссией", "комиссий", "комиссиям", "комиссиями", "комиссиях",
		"процент", "процента", "проценту", "процентом", "проценте", "проценты", "процентов", "процентам", "процентами", "процентах"}
}
