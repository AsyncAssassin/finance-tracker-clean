package domain

import (
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Budget limits the expenses of a user in one category. A user has at most
// one budget per category; setting it again replaces limit and period.
type Budget struct {
	UserID   int64
	Category string // normalized, see NormalizeCategory
	Limit    money.Money
	Period   Period
}

// BudgetInput is what a client sends to create or replace a budget.
type BudgetInput struct {
	Category string
	Limit    money.Money
	Period   string // empty means DefaultPeriod
}

// NewBudget validates and normalizes in for the user. Invalid input yields
// a *ValidationError listing every problem.
func NewBudget(userID int64, in BudgetInput) (Budget, error) {
	v := new(ValidationError)
	category, err := NormalizeCategory(in.Category)
	if err != nil {
		v.Add("category", err.Error())
	}
	b := Budget{
		UserID:   userID,
		Category: category,
		Limit:    in.Limit,
	}
	validateAmount(v, "limit", b.Limit)
	p, ok := ParsePeriod(in.Period)
	if !ok {
		v.Add("period", "must be one of: "+periodList())
	}
	b.Period = p
	if err := v.ErrOrNil(); err != nil {
		return Budget{}, err
	}
	return b, nil
}

// LimitFor returns the total limit of b over the date range [from, to]:
// the limit multiplied by the number of periods the range touches.
func (b Budget) LimitFor(from, to civil.Date) money.Money {
	return b.Limit * money.Money(b.Period.Count(from, to))
}

// CheckBudget verifies that a transaction of amount on date fits into b,
// given that spent has already been recorded in the same period. Spending
// exactly up to the limit is allowed. On failure it returns
// a *BudgetExceededError and the transaction must not be stored.
func CheckBudget(b Budget, date civil.Date, spent, amount money.Money) error {
	if spent <= b.Limit && amount <= b.Limit-spent {
		return nil
	}
	return &BudgetExceededError{
		Category:    b.Category,
		Period:      b.Period,
		PeriodLabel: b.Period.Label(date),
		Limit:       b.Limit,
		Spent:       spent,
		Amount:      amount,
	}
}
