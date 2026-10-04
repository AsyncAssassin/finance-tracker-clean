package grpcserver

import (
	"context"
	"errors"
	"time"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
)

// deadlineMargin is how long before the caller's deadline an import stops,
// so its results, including the rows already stored, reach the caller
// instead of a bare DEADLINE_EXCEEDED that would invite storing them twice.
const deadlineMargin = 2 * time.Second

func (h *handler) ImportTransactions(ctx context.Context, req *ledgerv1.ImportTransactionsRequest) (*ledgerv1.ImportTransactionsResponse, error) {
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	if err := v.ErrOrNil(); err != nil {
		return nil, h.status(ctx, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		margin := min(deadlineMargin, time.Until(deadline)/10)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-margin))
		defer cancel()
	}
	results := make([]*ledgerv1.ImportRowResult, len(req.GetRows()))
	rows := make([]usecase.ImportRow, 0, len(req.GetRows()))
	index := make([]int, 0, len(req.GetRows())) // position in results of each row passed on
	for i, r := range req.GetRows() {
		// A bad date fails its row, not the import.
		dv := new(domain.ValidationError)
		date := parseDate(dv, "date", r.GetDate())
		if err := dv.ErrOrNil(); err != nil {
			results[i] = h.rowResult(ctx, int(r.GetLine()), domain.Transaction{}, err)
			continue
		}
		rows = append(rows, usecase.ImportRow{Line: int(r.GetLine()), SourceID: r.GetSourceId(), Input: domain.TransactionInput{
			Amount:      money.Money(r.GetAmountKopecks()),
			Category:    r.GetCategory(),
			Description: r.GetDescription(),
			Date:        date,
			ExternalID:  r.GetExternalId(),
		}})
		index = append(index, i)
	}
	imported, err := h.svc.Import(ctx, user, rows)
	if err != nil {
		return nil, h.status(ctx, err)
	}
	for j, r := range imported {
		results[index[j]] = h.rowResult(ctx, r.Line, r.Transaction, r.Err)
	}
	return &ledgerv1.ImportTransactionsResponse{Results: results}, nil
}

// rowResult converts the outcome of one row. Its error codes are those of
// the HTTP API, so the gateway passes them on as they are.
func (h *handler) rowResult(ctx context.Context, line int, tx domain.Transaction, err error) *ledgerv1.ImportRowResult {
	r := &ledgerv1.ImportRowResult{Line: int32(line)}
	if err == nil {
		r.Outcome = &ledgerv1.ImportRowResult_Id{Id: tx.ID}
		return r
	}
	e := &ledgerv1.RowError{Message: err.Error()}
	var (
		validation *domain.ValidationError
		exceeded   *domain.BudgetExceededError
		duplicate  *domain.DuplicateError
		stopped    *usecase.ImportStoppedError
	)
	switch {
	case errors.Is(err, domain.ErrQuotaExceeded):
		e.Code, e.Message = "quota_exceeded", domain.ErrQuotaExceeded.Error()
	case errors.As(err, &stopped) && stopped.Line != line:
		e.Code, e.Message = "canceled", stopped.Error()
	case errors.Is(err, money.ErrOverflow):
		e.Code, e.Message = "invalid_request", money.ErrOverflow.Error()
	case errors.As(err, &validation):
		e.Code, e.Fields = "validation_error", validation.Fields
	case errors.As(err, &exceeded):
		e.Code = "budget_exceeded"
		e.Budget = &ledgerv1.BudgetExceeded{
			Category:         exceeded.Category,
			Period:           string(exceeded.Period),
			PeriodLabel:      exceeded.PeriodLabel,
			LimitKopecks:     int64(exceeded.Limit),
			SpentKopecks:     int64(exceeded.Spent),
			AmountKopecks:    int64(exceeded.Amount),
			RemainingKopecks: int64(exceeded.Remaining()),
		}
	case errors.As(err, &duplicate):
		e.Code, e.ExistingId = "already_exists", duplicate.ExistingID
		e.ExternalId = duplicate.ExternalID
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		e.Code, e.Message = "canceled", "not stored: the import was stopped before this row (time limit or the client left)"
	default:
		h.log.ErrorContext(ctx, "import row failed", "line", line, "error", err, "request_id", requestid.From(ctx))
		e.Code, e.Message = "internal_error", "internal error"
	}
	r.Outcome = &ledgerv1.ImportRowResult_Error{Error: e}
	return r
}
