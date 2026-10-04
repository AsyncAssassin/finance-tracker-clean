// Package civil provides a calendar date without a time of day or a time
// zone, so the day of an expense never shifts between zones.
package civil

import (
	"errors"
	"fmt"
	"time"
)

// Layout is the text form of a Date: YYYY-MM-DD.
const Layout = time.DateOnly

// ErrInvalid is wrapped by every error returned by Parse.
var ErrInvalid = errors.New("invalid date")

// Date is a calendar date. The zero Date means "not set"; see IsZero.
//
// Internally the day is kept as noon UTC, not midnight: the zero time.Time
// is 0001-01-01 00:00 UTC, so with midnight the first day of year 1 would
// be indistinguishable from "not set".
type Date struct {
	t time.Time // 12:00 UTC on the date
}

// New returns the date for the given year, month and day. Values outside
// their usual ranges are normalized like in time.Date, so New(2026, 2, 30)
// is March 2, 2026 and New(2026, 3, 0) is the last day of February.
func New(year int, month time.Month, day int) Date {
	return Date{time.Date(year, month, day, 12, 0, 0, 0, time.UTC)}
}

// Of returns the calendar date of t in the location of t.
func Of(t time.Time) Date {
	y, m, d := t.Date()
	return New(y, m, d)
}

// Parse reads a date written as YYYY-MM-DD. Years start at 0001: year 0
// is accepted by time.Parse but is not a calendar date anyone records.
func Parse(s string) (Date, error) {
	t, err := time.Parse(Layout, s)
	if err != nil || t.Year() < 1 {
		return Date{}, fmt.Errorf("%w: %q is not a date in the form YYYY-MM-DD", ErrInvalid, s)
	}
	return Of(t), nil
}

// IsZero reports whether d is the zero Date.
func (d Date) IsZero() bool { return d.t.IsZero() }

// Time returns 00:00 UTC on the date, for storage and arithmetic.
func (d Date) Time() time.Time {
	y, m, day := d.t.Date()
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

// Year returns the year of d.
func (d Date) Year() int { return d.t.Year() }

// Month returns the month of d.
func (d Date) Month() time.Month { return d.t.Month() }

// Day returns the day of the month of d.
func (d Date) Day() int { return d.t.Day() }

// AddDays returns the date n days after d; n may be negative.
func (d Date) AddDays(n int) Date { return Date{d.t.AddDate(0, 0, n)} }

// AddMonths returns the date n months after d, normalized like time.AddDate.
func (d Date) AddMonths(n int) Date { return Date{d.t.AddDate(0, n, 0)} }

// Before reports whether d is before o.
func (d Date) Before(o Date) bool { return d.t.Before(o.t) }

// After reports whether d is after o.
func (d Date) After(o Date) bool { return d.t.After(o.t) }

// Equal reports whether d and o are the same date.
func (d Date) Equal(o Date) bool { return d.t.Equal(o.t) }

// Compare returns -1, 0 or +1 depending on whether d is before, equal to
// or after o.
func (d Date) Compare(o Date) int { return d.t.Compare(o.t) }

// String formats d as YYYY-MM-DD; the zero Date formats as an empty string.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.t.Format(Layout)
}

// MarshalText formats d as YYYY-MM-DD, which is also the JSON form.
func (d Date) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// UnmarshalText parses YYYY-MM-DD; an empty text gives the zero Date.
func (d *Date) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*d = Date{}
		return nil
	}
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
