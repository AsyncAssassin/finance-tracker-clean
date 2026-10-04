package ledgerclient

import (
	"fmt"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// The errors below are the ones only the ledger produces. Generic errors,
// such as invalid fields or an unreachable ledger, come from the upstream
// package.

// BudgetExceededError reports a transaction rejected by the budget check.
type BudgetExceededError struct {
	Category    string
	Period      string
	PeriodLabel string
	Limit       money.Money
	Spent       money.Money
	Amount      money.Money
	Remaining   money.Money
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exceeded for %q (%s): spent %s + new %s > limit %s",
		e.Category, e.PeriodLabel, e.Spent, e.Amount, e.Limit)
}

// DuplicateError reports a transaction whose external ID is already used.
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

// RowError is a failed import row without a typed error. Code is
// "canceled" when the import stopped before the row, "internal_error"
// otherwise.
type RowError struct {
	Code    string
	Message string
}

func (e *RowError) Error() string { return e.Message }
