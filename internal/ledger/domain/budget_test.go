package domain

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

func TestNewBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		in         BudgetInput
		want       Budget
		wantFields []string // invalid fields; empty means success
	}{
		{
			name: "defaults to a monthly budget and normalizes the category",
			in:   BudgetInput{Category: "  Еда ", Limit: 500000},
			want: Budget{UserID: 7, Category: "еда", Limit: 500000, Period: PeriodMonth},
		},
		{
			name: "explicit period",
			in:   BudgetInput{Category: "ремонт", Limit: 10000000, Period: " None "},
			want: Budget{UserID: 7, Category: "ремонт", Limit: 10000000, Period: PeriodNone},
		},
		{name: "empty category", in: BudgetInput{Category: "  ", Limit: 100}, wantFields: []string{"category"}},
		{name: "zero limit", in: BudgetInput{Category: "еда", Limit: 0}, wantFields: []string{"limit"}},
		{name: "negative limit", in: BudgetInput{Category: "еда", Limit: -1}, wantFields: []string{"limit"}},
		{name: "limit above max", in: BudgetInput{Category: "еда", Limit: money.Max + 1}, wantFields: []string{"limit"}},
		{name: "bad period", in: BudgetInput{Category: "еда", Limit: 100, Period: "week"}, wantFields: []string{"period"}},
		{name: "everything wrong", in: BudgetInput{Period: "week"}, wantFields: []string{"category", "limit", "period"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewBudget(7, tc.in)
			if len(tc.wantFields) > 0 {
				assertValidationFields(t, err, tc.wantFields...)
				return
			}
			if err != nil {
				t.Fatalf("NewBudget returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("NewBudget = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBudgetLimitFor(t *testing.T) {
	t.Parallel()
	monthly := Budget{Category: "еда", Limit: 500000, Period: PeriodMonth}
	yearly := Budget{Category: "еда", Limit: 500000, Period: PeriodYear}
	allTime := Budget{Category: "еда", Limit: 500000, Period: PeriodNone}
	jan1, mar31 := civil.New(2026, time.January, 1), civil.New(2026, time.March, 31)
	if got := monthly.LimitFor(jan1, mar31); got != 1500000 {
		t.Errorf("monthly over three months = %s, want 15000.00", got)
	}
	if got := yearly.LimitFor(civil.New(2025, time.December, 31), jan1); got != 1000000 {
		t.Errorf("yearly over two years = %s, want 10000.00", got)
	}
	if got := allTime.LimitFor(civil.New(2000, time.January, 1), civil.New(2030, time.January, 1)); got != 500000 {
		t.Errorf("all-time over thirty years = %s, want 5000.00", got)
	}
}

func TestCheckBudget(t *testing.T) {
	t.Parallel()
	b := Budget{Category: "еда", Limit: 500000, Period: PeriodMonth}
	date := civil.New(2026, time.October, 3)

	if err := CheckBudget(b, date, 475050, 24950); err != nil {
		t.Fatalf("spending exactly up to the limit returned error: %v", err)
	}
	err := CheckBudget(b, date, 475050, 24951)
	var exceeded *BudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("CheckBudget returned %v, want *BudgetExceededError", err)
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Error("errors.Is(err, ErrBudgetExceeded) = false")
	}
	want := &BudgetExceededError{Category: "еда", Period: PeriodMonth, PeriodLabel: "2026-10", Limit: 500000, Spent: 475050, Amount: 24951}
	if *exceeded != *want {
		t.Errorf("error = %+v, want %+v", exceeded, want)
	}
	if exceeded.Remaining() != 24950 {
		t.Errorf("Remaining() = %s, want 249.50", exceeded.Remaining())
	}
	if got := err.Error(); got != `budget exceeded for "еда" (2026-10): spent 4750.50 + new 249.51 > limit 5000.00` {
		t.Errorf("Error() = %q", got)
	}

	// The limit was lowered below what is already spent: nothing remains.
	err = CheckBudget(b, date, 600000, 1)
	if !errors.As(err, &exceeded) || exceeded.Remaining() != 0 {
		t.Errorf("Remaining() after overspending = %v, want 0", err)
	}
}

// assertValidationFields checks that err is a *ValidationError naming
// exactly the given fields.
func assertValidationFields(t *testing.T, err error, fields ...string) {
	t.Helper()
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("error = %v, want *ValidationError for fields %v", err, fields)
	}
	if !errors.Is(err, ErrValidation) {
		t.Error("errors.Is(err, ErrValidation) = false")
	}
	if len(v.Fields) != len(fields) {
		t.Fatalf("invalid fields = %v, want %v", v.Fields, fields)
	}
	for _, f := range fields {
		if v.Fields[f] == "" {
			t.Errorf("field %q is not reported in %v", f, v.Fields)
		}
	}
}

func TestBudgetCheckCannotWrapAnAggregate(t *testing.T) {
	b := Budget{Category: "food", Limit: money.Max, Period: PeriodNone}
	if err := CheckBudget(b, MinDate, money.Money(math.MaxInt64), 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("overflow bypassed the budget: %v", err)
	}
}
