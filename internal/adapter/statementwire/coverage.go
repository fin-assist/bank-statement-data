package statementwire

import (
	"strings"
	"time"

	statementdatav1 "github.com/fin-assist/contracts/gen/go/finassist/statementdata/v1"

	"github.com/fin-assist/bank-statement-data/internal/importing"
)

func toCoverage(c importing.Coverage) (*statementdatav1.ImportCoverage, error) {
	full, err := count32(c.FullMonths)
	if err != nil {
		return nil, err
	}
	before, err := count32(c.FullMonthsBefore)
	if err != nil {
		return nil, err
	}
	p := &statementdatav1.ImportCoverage{FullMonths: full, FullMonthsBefore: before}
	for _, month := range c.NewlyFullMonths {
		if !validMonth(month) {
			return nil, errProjection
		}
		p.NewlyFullMonths = append(p.NewlyFullMonths, month)
	}
	for _, month := range c.IncompleteMonths {
		if !validMonth(month.Month) || month.DataFrom.After(month.DataTo) {
			return nil, errProjection
		}
		rangePB, err := toRange(month.Range)
		if err != nil {
			return nil, err
		}
		first, err := timestamp(&month.DataFrom, true)
		if err != nil {
			return nil, err
		}
		last, err := timestamp(&month.DataTo, true)
		if err != nil {
			return nil, err
		}
		m := &statementdatav1.IncompleteMonth{Month: month.Month, Range: rangePB, DataFrom: first, DataTo: last, InProgress: month.InProgress}
		for _, gap := range month.Gaps {
			value, err := toRange(gap)
			if err != nil {
				return nil, err
			}
			m.Gaps = append(m.Gaps, value)
		}
		p.IncompleteMonths = append(p.IncompleteMonths, m)
	}
	for _, feature := range c.Features {
		code, ok := statementdatav1.Feature_value["FEATURE_"+strings.ToUpper(feature.Feature)]
		if !ok || code == 0 {
			return nil, errProjection
		}
		f := &statementdatav1.FeatureAvailability{Feature: statementdatav1.Feature(code), Open: feature.Open, OpenedNow: feature.OpenedNow}
		if feature.RequiredFullMonths != nil {
			value, err := count32(*feature.RequiredFullMonths)
			if err != nil {
				return nil, err
			}
			f.RequiredFullMonths = &value
		}
		if feature.RequiredMonthsWithData != nil {
			value, err := count32(*feature.RequiredMonthsWithData)
			if err != nil {
				return nil, err
			}
			f.RequiredMonthsWithData = &value
		}
		p.Features = append(p.Features, f)
	}
	return p, nil
}

func fromCoverage(p *statementdatav1.ImportCoverage) (importing.Coverage, error) {
	if p == nil {
		return importing.Coverage{}, errProjection
	}
	c := importing.Coverage{FullMonths: int(p.FullMonths), FullMonthsBefore: int(p.FullMonthsBefore), NewlyFullMonths: append([]string(nil), p.NewlyFullMonths...)}
	for _, month := range p.IncompleteMonths {
		if month == nil {
			return importing.Coverage{}, errProjection
		}
		r, err := fromRange(month.Range)
		if err != nil {
			return importing.Coverage{}, err
		}
		first, err := fromTimestamp(month.DataFrom, true)
		if err != nil {
			return importing.Coverage{}, err
		}
		last, err := fromTimestamp(month.DataTo, true)
		if err != nil {
			return importing.Coverage{}, err
		}
		m := importing.IncompleteMonth{Month: month.Month, Range: r, DataFrom: *first, DataTo: *last, InProgress: month.InProgress}
		for _, gap := range month.Gaps {
			value, err := fromRange(gap)
			if err != nil {
				return importing.Coverage{}, err
			}
			m.Gaps = append(m.Gaps, value)
		}
		c.IncompleteMonths = append(c.IncompleteMonths, m)
	}
	for _, feature := range p.Features {
		if feature == nil {
			return importing.Coverage{}, errProjection
		}
		name, ok := statementdatav1.Feature_name[int32(feature.Feature)]
		if !ok || feature.Feature == 0 {
			return importing.Coverage{}, errProjection
		}
		f := importing.FeatureAvailability{Feature: strings.ToLower(strings.TrimPrefix(name, "FEATURE_")), Open: feature.Open, OpenedNow: feature.OpenedNow}
		if feature.RequiredFullMonths != nil {
			value := int(*feature.RequiredFullMonths)
			f.RequiredFullMonths = &value
		}
		if feature.RequiredMonthsWithData != nil {
			value := int(*feature.RequiredMonthsWithData)
			f.RequiredMonthsWithData = &value
		}
		c.Features = append(c.Features, f)
	}
	return c, nil
}

func validMonth(raw string) bool {
	month, err := time.Parse("2006-01", raw)
	return err == nil && month.Year() >= 1 && month.Year() <= 9999 && month.Format("2006-01") == raw
}
