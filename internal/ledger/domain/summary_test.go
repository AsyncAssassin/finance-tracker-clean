package domain

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

func TestBuildSummary(t *testing.T) {
	t.Parallel()
	oct1, oct31 := civil.New(2026, time.October, 1), civil.New(2026, time.October, 31)
	spent := map[string]money.Money{"еда": 475050, "здоровье": 70000}
	budgets := []Budget{
		{Category: "транспорт", Limit: 250000, Period: PeriodMonth},
		{Category: "еда", Limit: 500000, Period: PeriodMonth},
	}

	got, err := BuildSummary(oct1, oct31, spent, budgets)
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{
		From:  oct1,
		To:    oct31,
		Total: 545050,
		Categories: []CategorySummary{
			{Category: "еда", Spent: 475050, Budget: &BudgetUsage{Period: PeriodMonth, Limit: 500000, Remaining: 24950, Percent: 95.01}},
			{Category: "здоровье", Spent: 70000},
			{Category: "транспорт", Spent: 0, Budget: &BudgetUsage{Period: PeriodMonth, Limit: 250000, Remaining: 250000, Percent: 0}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildSummary =\n%+v\nwant\n%+v", describe(got), describe(want))
	}
}

func TestBuildSummaryRangeAndOverspending(t *testing.T) {
	t.Parallel()
	// Two months: the monthly limit doubles, so 4750.50 of 10000 is 47.51 %.
	got, err := BuildSummary(civil.New(2026, time.September, 15), civil.New(2026, time.October, 15),
		map[string]money.Money{"еда": 475050}, []Budget{{Category: "еда", Limit: 500000, Period: PeriodMonth}})
	if err != nil {
		t.Fatal(err)
	}
	if u := got.Categories[0].Budget; u.Limit != 1000000 || u.Remaining != 524950 || u.Percent != 47.51 {
		t.Errorf("two-month usage = %+v", u)
	}

	// Spent more than the limit: nothing remains and the percentage goes above 100.
	got, err = BuildSummary(civil.New(2026, time.October, 1), civil.New(2026, time.October, 31),
		map[string]money.Money{"еда": 600000}, []Budget{{Category: "еда", Limit: 500000, Period: PeriodNone}})
	if err != nil {
		t.Fatal(err)
	}
	if u := got.Categories[0].Budget; u.Limit != 500000 || u.Remaining != 0 || u.Percent != 120 {
		t.Errorf("overspent usage = %+v", u)
	}

	empty, err := BuildSummary(civil.New(2026, time.October, 1), civil.New(2026, time.October, 31), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Total != 0 || empty.Categories == nil || len(empty.Categories) != 0 {
		t.Errorf("empty summary = %+v, want no categories and an empty slice", empty)
	}
}

func describe(s Summary) string {
	out := s.From.String() + ".." + s.To.String() + " total=" + s.Total.String()
	for _, c := range s.Categories {
		out += "\n  " + c.Category + " spent=" + c.Spent.String()
		if c.Budget != nil {
			out += " budget=" + string(c.Budget.Period) + " limit=" + c.Budget.Limit.String() +
				" remaining=" + c.Budget.Remaining.String()
			out += " percent=" + money.Money(c.Budget.Percent*100).String()
		}
	}
	return out
}

func TestSummaryCannotWrapTheTotal(t *testing.T) {
	_, err := BuildSummary(civil.New(2026, time.October, 1), civil.New(2026, time.October, 31), map[string]money.Money{
		"a": money.Money(math.MaxInt64/2 + 1), "b": money.Money(math.MaxInt64/2 + 1),
	}, nil)
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	_, err = BuildSummary(MinDate, MaxDate, nil, []Budget{{Category: "food", Limit: money.Max, Period: PeriodMonth}})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("expected budget multiplication overflow, got %v", err)
	}
}
