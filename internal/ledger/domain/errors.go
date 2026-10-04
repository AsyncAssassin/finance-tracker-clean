package domain

import (
	"errors"
	"fmt"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/validation"
)

// Sentinel errors. errors.Is matches them through the typed errors below.
var (
	ErrValidation     = validation.ErrValidation
	ErrBudgetExceeded = errors.New("budget exceeded")
	ErrNotFound       = errors.New("not found")
	ErrAlreadyExists  = errors.New("already exists")
	ErrQuotaExceeded  = errors.New("account storage quota exceeded")
)

// Pilot limits bound data retained by one account, independent of request size.
const MaxTransactions = 10000
const MaxCategories = 100

// ValidationError lists the invalid fields of a request and the reason for
// each. errors.Is(err, ErrValidation) matches it. It is the error type
// shared by all services, so their transports report fields the same way.
type ValidationError = validation.Error

// BudgetExceededError reports a transaction that does not fit into the
// budget of its category. errors.Is(err, ErrBudgetExceeded) matches it.
type BudgetExceededError struct {
	Category    string
	Period      Period
	PeriodLabel string      // the period the transaction falls into, such as "2026-10"
	Limit       money.Money // budget limit for one period
	Spent       money.Money // already spent in that period
	Amount      money.Money // amount of the rejected transaction
}

// Remaining returns how much of the limit is still available, never negative.
func (e *BudgetExceededError) Remaining() money.Money {
	return max(0, e.Limit-e.Spent)
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exceeded for %q (%s): spent %s + new %s > limit %s",
		e.Category, e.PeriodLabel, e.Spent, e.Amount, e.Limit)
}

// Is makes errors.Is(err, ErrBudgetExceeded) report true.
func (e *BudgetExceededError) Is(target error) bool { return target == ErrBudgetExceeded }

// DuplicateError reports a transaction whose external ID the user has
// already used. errors.Is(err, ErrAlreadyExists) matches it.
type DuplicateError struct {
	ExternalID string
	ExistingID int64
}

func (e *DuplicateError) Error() string {
	if e.ExternalID == "" {
		return fmt.Sprintf("transaction already exists as #%d", e.ExistingID)
	}
	return fmt.Sprintf("transaction with external_id %q already exists as #%d", e.ExternalID, e.ExistingID)
}

// Is makes errors.Is(err, ErrAlreadyExists) report true.
func (e *DuplicateError) Is(target error) bool { return target == ErrAlreadyExists }
