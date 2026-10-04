package grpcserver

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// exportChunkSize is the size of one part of a streamed CSV export, well
// below the 4 MB default message limit of gRPC.
const exportChunkSize = 64 << 10

// handler implements the LedgerService on top of the use cases. It only
// converts messages and errors; the rules live in the use cases.
type handler struct {
	ledgerv1.UnimplementedLedgerServiceServer
	svc *usecase.Service
	log *slog.Logger
}

func (h *handler) CreateTransaction(ctx context.Context, req *ledgerv1.CreateTransactionRequest) (*ledgerv1.CreateTransactionResponse, error) {
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	date := parseDate(v, "date", req.GetDate())
	if err := v.ErrOrNil(); err != nil {
		return nil, h.status(ctx, err)
	}
	tx, err := h.svc.CreateTransaction(ctx, user, domain.TransactionInput{
		Amount:      money.Money(req.GetAmountKopecks()),
		Category:    req.GetCategory(),
		Description: req.GetDescription(),
		Date:        date,
		ExternalID:  req.GetExternalId(),
	})
	if err != nil {
		return nil, h.status(ctx, err)
	}
	return &ledgerv1.CreateTransactionResponse{Transaction: toTransaction(tx)}, nil
}

func (h *handler) ListTransactions(ctx context.Context, req *ledgerv1.ListTransactionsRequest) (*ledgerv1.ListTransactionsResponse, error) {
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	f := toFilter(v, req.GetFilter())
	if err := v.ErrOrNil(); err != nil {
		return nil, h.status(ctx, err)
	}
	list, err := h.svc.ListTransactions(ctx, user, f)
	if err != nil {
		return nil, h.status(ctx, err)
	}
	out := make([]*ledgerv1.Transaction, len(list))
	for i, tx := range list {
		out[i] = toTransaction(tx)
	}
	return &ledgerv1.ListTransactionsResponse{Transactions: out}, nil
}

func (h *handler) ExportTransactionsCSV(req *ledgerv1.ExportTransactionsCSVRequest, stream ledgerv1.LedgerService_ExportTransactionsCSVServer) error {
	ctx := stream.Context()
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	f := toFilter(v, req.GetFilter())
	if err := v.ErrOrNil(); err != nil {
		return h.status(ctx, err)
	}
	data, err := h.svc.ExportCSV(ctx, user, f)
	if err != nil {
		return h.status(ctx, err)
	}
	for len(data) > 0 {
		n := min(len(data), exportChunkSize)
		if err := stream.Send(&ledgerv1.ExportTransactionsCSVResponse{Chunk: data[:n]}); err != nil {
			return err // the stream is broken; its status is already set
		}
		data = data[n:]
	}
	return nil
}

func (h *handler) UpsertBudget(ctx context.Context, req *ledgerv1.UpsertBudgetRequest) (*ledgerv1.UpsertBudgetResponse, error) {
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	if err := v.ErrOrNil(); err != nil {
		return nil, h.status(ctx, err)
	}
	b, created, err := h.svc.UpsertBudget(ctx, user, domain.BudgetInput{
		Category: req.GetCategory(),
		Limit:    money.Money(req.GetLimitKopecks()),
		Period:   req.GetPeriod(),
	})
	if err != nil {
		return nil, h.status(ctx, err)
	}
	return &ledgerv1.UpsertBudgetResponse{Budget: toBudget(b), Created: created}, nil
}

func (h *handler) ListBudgets(ctx context.Context, req *ledgerv1.ListBudgetsRequest) (*ledgerv1.ListBudgetsResponse, error) {
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	if err := v.ErrOrNil(); err != nil {
		return nil, h.status(ctx, err)
	}
	list, err := h.svc.ListBudgets(ctx, user)
	if err != nil {
		return nil, h.status(ctx, err)
	}
	out := make([]*ledgerv1.Budget, len(list))
	for i, b := range list {
		out[i] = toBudget(b)
	}
	return &ledgerv1.ListBudgetsResponse{Budgets: out}, nil
}

func (h *handler) GetSummary(ctx context.Context, req *ledgerv1.GetSummaryRequest) (*ledgerv1.GetSummaryResponse, error) {
	v := new(domain.ValidationError)
	user := userID(v, req.GetUserId())
	from := parseDate(v, "from", req.GetFrom())
	to := parseDate(v, "to", req.GetTo())
	if err := v.ErrOrNil(); err != nil {
		return nil, h.status(ctx, err)
	}
	s, err := h.svc.Summary(ctx, user, from, to)
	if err != nil {
		return nil, h.status(ctx, err)
	}
	if s.Cache != usecase.CacheOff {
		// The header is informational; failing to send it must not fail
		// the call.
		_ = grpc.SetHeader(ctx, metadata.Pairs(ledgerv1.CacheHeader, string(s.Cache)))
	}
	out := &ledgerv1.GetSummaryResponse{
		From:         s.From.String(),
		To:           s.To.String(),
		TotalKopecks: int64(s.Total),
		Categories:   make([]*ledgerv1.CategorySummary, len(s.Categories)),
	}
	for i, row := range s.Categories {
		cs := &ledgerv1.CategorySummary{Category: row.Category, SpentKopecks: int64(row.Spent)}
		if u := row.Budget; u != nil {
			cs.Budget = &ledgerv1.BudgetUsage{
				Period:           string(u.Period),
				LimitKopecks:     int64(u.Limit),
				RemainingKopecks: int64(u.Remaining),
				Percent:          u.Percent,
			}
		}
		out.Categories[i] = cs
	}
	return out, nil
}

// userID checks the user ID the gateway passed. Zero means the caller
// forgot to set it; acting on it would mix the data of unknown users.
func userID(v *domain.ValidationError, id int64) int64 {
	if id <= 0 {
		v.Add("user_id", "must be a positive integer")
	}
	return id
}

// parseDate reads an optional YYYY-MM-DD field; an empty string is the
// zero Date.
func parseDate(v *domain.ValidationError, field, s string) civil.Date {
	if s == "" {
		return civil.Date{}
	}
	d, err := civil.Parse(s)
	if err != nil {
		v.Add(field, "must be a date in the form YYYY-MM-DD")
	}
	return d
}

func toFilter(v *domain.ValidationError, f *ledgerv1.TransactionFilter) usecase.TransactionFilter {
	return usecase.TransactionFilter{
		From:     parseDate(v, "from", f.GetFrom()),
		To:       parseDate(v, "to", f.GetTo()),
		Category: f.GetCategory(),
		Limit:    int(f.GetLimit()),
		Offset:   int(f.GetOffset()),
	}
}

func toTransaction(tx domain.Transaction) *ledgerv1.Transaction {
	return &ledgerv1.Transaction{
		Id:            tx.ID,
		AmountKopecks: int64(tx.Amount),
		Category:      tx.Category,
		Description:   tx.Description,
		Date:          tx.Date.String(),
		ExternalId:    tx.ExternalID,
		CreatedAt:     timestamppb.New(tx.CreatedAt),
	}
}

func toBudget(b domain.Budget) *ledgerv1.Budget {
	return &ledgerv1.Budget{Category: b.Category, LimitKopecks: int64(b.Limit), Period: string(b.Period)}
}
