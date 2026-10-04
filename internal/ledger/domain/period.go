package domain

import (
	"fmt"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

// Period says how long one budget limit lasts.
type Period string

// Supported periods.
const (
	PeriodMonth Period = "month" // the limit starts over every calendar month
	PeriodYear  Period = "year"  // the limit starts over every calendar year
	PeriodNone  Period = "none"  // one limit for all time
)

// DefaultPeriod applies when a budget is created without a period.
const DefaultPeriod = PeriodMonth

// Periods lists the supported periods in display order.
var Periods = []Period{PeriodMonth, PeriodYear, PeriodNone}

// The date range that bounds a budget without a period.
var (
	MinDate = civil.New(1, time.January, 1)
	MaxDate = civil.New(9999, time.December, 31)
)

// ParsePeriod normalizes s and reports whether it names a supported period.
// An empty string means DefaultPeriod.
func ParsePeriod(s string) (Period, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return DefaultPeriod, true
	}
	p := Period(s)
	return p, p.Valid()
}

// Valid reports whether p is one of the supported periods.
func (p Period) Valid() bool {
	switch p {
	case PeriodMonth, PeriodYear, PeriodNone:
		return true
	}
	return false
}

// Bounds returns the first and last day of the period of p that contains d.
func (p Period) Bounds(d civil.Date) (from, to civil.Date) {
	switch p {
	case PeriodMonth:
		return civil.New(d.Year(), d.Month(), 1), civil.New(d.Year(), d.Month()+1, 0)
	case PeriodYear:
		return civil.New(d.Year(), time.January, 1), civil.New(d.Year(), time.December, 31)
	default:
		return MinDate, MaxDate
	}
}

// Label names the period of p that contains d: "2026-10" for a month,
// "2026" for a year and "all time" for no period.
func (p Period) Label(d civil.Date) string {
	switch p {
	case PeriodMonth:
		return fmt.Sprintf("%04d-%02d", d.Year(), int(d.Month()))
	case PeriodYear:
		return fmt.Sprintf("%04d", d.Year())
	default:
		return "all time"
	}
}

// Count returns how many periods of p the date range [from, to] touches.
// A range inside one month counts as one month even if it is a single day.
func (p Period) Count(from, to civil.Date) int64 {
	if to.Before(from) {
		from, to = to, from
	}
	switch p {
	case PeriodMonth:
		return int64(to.Year()*12+int(to.Month())) - int64(from.Year()*12+int(from.Month())) + 1
	case PeriodYear:
		return int64(to.Year()-from.Year()) + 1
	default:
		return 1
	}
}

func periodList() string {
	names := make([]string, len(Periods))
	for i, p := range Periods {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}
