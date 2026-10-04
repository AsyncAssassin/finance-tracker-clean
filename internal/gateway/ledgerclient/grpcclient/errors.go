package grpcclient

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// translate converts a gRPC status from the ledger into an error. The two
// statuses with ledger details become the ledger's own errors, both 409 in
// HTTP; everything else goes through upstream.FromStatus.
//
//	gRPC code            status detail                   error
//	FAILED_PRECONDITION  ledger.v1.BudgetExceeded        *ledgerclient.BudgetExceededError
//	ALREADY_EXISTS       ledger.v1.DuplicateTransaction  *ledgerclient.DuplicateError
func translate(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	// The code is checked first, so details are decoded only when a
	// ledger detail can be there.
	switch st.Code() {
	case codes.FailedPrecondition:
		if d, ok := upstream.Detail[*ledgerv1.BudgetExceeded](st); ok {
			return budgetExceeded(d)
		}
	case codes.AlreadyExists:
		if d, ok := upstream.Detail[*ledgerv1.DuplicateTransaction](st); ok {
			return &ledgerclient.DuplicateError{ExternalID: d.GetExternalId(), ExistingID: d.GetExistingId()}
		}
	}
	return upstream.FromStatus("ledger", err)
}

func budgetExceeded(d *ledgerv1.BudgetExceeded) *ledgerclient.BudgetExceededError {
	return &ledgerclient.BudgetExceededError{
		Category:    d.GetCategory(),
		Period:      d.GetPeriod(),
		PeriodLabel: d.GetPeriodLabel(),
		Limit:       money.Money(d.GetLimitKopecks()),
		Spent:       money.Money(d.GetSpentKopecks()),
		Amount:      money.Money(d.GetAmountKopecks()),
		Remaining:   money.Money(d.GetRemainingKopecks()),
	}
}

// rowError converts the error of an import row into the error types the
// HTTP layer knows. The server supplies the effective idempotency key.
func rowError(e *ledgerv1.RowError) error {
	switch e.GetCode() {
	case "validation_error":
		return &upstream.ValidationError{Fields: e.GetFields()}
	case "budget_exceeded":
		if e.GetBudget() != nil {
			return budgetExceeded(e.GetBudget())
		}
	case "already_exists":
		return &ledgerclient.DuplicateError{ExternalID: e.GetExternalId(), ExistingID: e.GetExistingId()}
	}
	return &ledgerclient.RowError{Code: e.GetCode(), Message: e.GetMessage()}
}
