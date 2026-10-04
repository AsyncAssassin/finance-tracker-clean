package domain

import (
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

func TestParsePeriod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want Period
		ok   bool
	}{
		{"", PeriodMonth, true},
		{"month", PeriodMonth, true},
		{" Month ", PeriodMonth, true},
		{"YEAR", PeriodYear, true},
		{"none", PeriodNone, true},
		{"week", "week", false},
		{"all time", "all time", false},
	}
	for _, tc := range tests {
		got, ok := ParsePeriod(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParsePeriod(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if Period("week").Valid() {
		t.Error("Period(\"week\").Valid() = true")
	}
}

func TestPeriodBoundsAndLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		p         Period
		d         civil.Date
		from, to  civil.Date
		wantLabel string
	}{
		{"month", PeriodMonth, civil.New(2026, time.February, 15), civil.New(2026, time.February, 1), civil.New(2026, time.February, 28), "2026-02"},
		{"leap month", PeriodMonth, civil.New(2024, time.February, 10), civil.New(2024, time.February, 1), civil.New(2024, time.February, 29), "2024-02"},
		{"december", PeriodMonth, civil.New(2026, time.December, 31), civil.New(2026, time.December, 1), civil.New(2026, time.December, 31), "2026-12"},
		{"year", PeriodYear, civil.New(2026, time.June, 15), civil.New(2026, time.January, 1), civil.New(2026, time.December, 31), "2026"},
		{"none", PeriodNone, civil.New(2026, time.June, 15), MinDate, MaxDate, "all time"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			from, to := tc.p.Bounds(tc.d)
			if !from.Equal(tc.from) || !to.Equal(tc.to) {
				t.Errorf("Bounds(%s) = [%s, %s], want [%s, %s]", tc.d, from, to, tc.from, tc.to)
			}
			if got := tc.p.Label(tc.d); got != tc.wantLabel {
				t.Errorf("Label(%s) = %q, want %q", tc.d, got, tc.wantLabel)
			}
		})
	}
}

func TestPeriodCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		p        Period
		from, to civil.Date
		want     int64
	}{
		{"same day", PeriodMonth, civil.New(2026, time.October, 3), civil.New(2026, time.October, 3), 1},
		{"same month", PeriodMonth, civil.New(2026, time.October, 1), civil.New(2026, time.October, 31), 1},
		{"adjacent months", PeriodMonth, civil.New(2026, time.January, 31), civil.New(2026, time.February, 1), 2},
		{"across new year", PeriodMonth, civil.New(2025, time.December, 15), civil.New(2026, time.January, 15), 2},
		{"whole year", PeriodMonth, civil.New(2026, time.January, 1), civil.New(2026, time.December, 31), 12},
		{"reversed", PeriodMonth, civil.New(2026, time.March, 1), civil.New(2026, time.January, 1), 3},
		{"one year", PeriodYear, civil.New(2026, time.January, 1), civil.New(2026, time.December, 31), 1},
		{"two years", PeriodYear, civil.New(2025, time.December, 31), civil.New(2026, time.January, 1), 2},
		{"none", PeriodNone, civil.New(2000, time.January, 1), civil.New(2030, time.January, 1), 1},
	}
	for _, tc := range tests {
		if got := tc.p.Count(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: %s.Count(%s, %s) = %d, want %d", tc.name, tc.p, tc.from, tc.to, got, tc.want)
		}
	}
}
