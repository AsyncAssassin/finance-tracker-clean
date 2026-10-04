package grpcserver

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// status converts an error of the use cases into a gRPC status. The
// ledger's own errors carry typed details; the rest is grpckit.Status:
//
//	*domain.BudgetExceededError → FAILED_PRECONDITION + ledger.v1.BudgetExceeded
//	*domain.DuplicateError      → ALREADY_EXISTS + ledger.v1.DuplicateTransaction
//	domain.ErrNotFound          → NOT_FOUND
//	*domain.ValidationError     → INVALID_ARGUMENT + google.rpc.BadRequest
//	context errors              → CANCELLED, DEADLINE_EXCEEDED
//	anything else               → INTERNAL with a generic message
func (h *handler) status(ctx context.Context, err error) error {
	var (
		exceeded  *domain.BudgetExceededError
		duplicate *domain.DuplicateError
	)
	switch {
	case errors.Is(err, domain.ErrQuotaExceeded):
		return grpckit.WithDetails(codes.ResourceExhausted, domain.ErrQuotaExceeded.Error(),
			&errdetails.ErrorInfo{Reason: "STORAGE_QUOTA", Domain: "finance-tracker"})
	case errors.Is(err, money.ErrOverflow):
		return status.Error(codes.InvalidArgument, money.ErrOverflow.Error())
	case errors.As(err, &exceeded):
		return grpckit.WithDetails(codes.FailedPrecondition, exceeded.Error(), &ledgerv1.BudgetExceeded{
			Category:         exceeded.Category,
			Period:           string(exceeded.Period),
			PeriodLabel:      exceeded.PeriodLabel,
			LimitKopecks:     int64(exceeded.Limit),
			SpentKopecks:     int64(exceeded.Spent),
			AmountKopecks:    int64(exceeded.Amount),
			RemainingKopecks: int64(exceeded.Remaining()),
		})
	case errors.As(err, &duplicate):
		return grpckit.WithDetails(codes.AlreadyExists, duplicate.Error(), &ledgerv1.DuplicateTransaction{
			ExternalId: duplicate.ExternalID,
			ExistingId: duplicate.ExistingID,
		})
	case errors.Is(err, domain.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return grpckit.Status(ctx, h.log, err)
	}
}
