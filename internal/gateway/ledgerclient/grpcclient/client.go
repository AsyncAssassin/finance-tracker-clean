// Package grpcclient implements ledgerclient.Client by calling the ledger
// service over gRPC. It converts the gateway's types to messages and the
// gRPC statuses of the ledger back to the errors of ledgerclient, which
// the HTTP layer maps to status codes (see translate).
package grpcclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Client talks to the ledger over one gRPC connection, which is safe for
// concurrent use and reconnects by itself.
type Client struct {
	conn          *grpc.ClientConn
	api           ledgerv1.LedgerServiceClient
	timeout       time.Duration
	importTimeout time.Duration
}

var _ ledgerclient.Client = (*Client)(nil)

// New returns a client for the ledger at target, such as "ledger:9090".
// It does not connect yet, so the gateway starts even while the ledger is
// down and answers 502 until it is up. Every call is limited to timeout.
// opts are added to the defaults of grpckit.Dial; tests use them to dial an
// in-memory listener.
func New(target string, timeout time.Duration, opts ...grpc.DialOption) (*Client, error) {
	conn, err := grpckit.Dial(target, timeout, opts...)
	if err != nil {
		return nil, fmt.Errorf("ledger client for %q: %w", target, err)
	}
	return &Client{conn: conn, api: ledgerv1.NewLedgerServiceClient(conn), timeout: timeout, importTimeout: ledgerclient.DefaultImportTimeout}, nil
}

// WithImportTimeout sets the timeout of ImportTransactions and returns c.
func (c *Client) WithImportTimeout(d time.Duration) *Client {
	c.importTimeout = d
	return c
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) CreateTransaction(ctx context.Context, userID int64, in ledgerclient.TransactionInput) (ledgerclient.Transaction, error) {
	resp, err := c.api.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{
		UserId:        userID,
		AmountKopecks: int64(in.Amount),
		Category:      in.Category,
		Description:   in.Description,
		Date:          in.Date.String(),
		ExternalId:    in.ExternalID,
	})
	if err != nil {
		return ledgerclient.Transaction{}, translate(err)
	}
	return fromTransaction(resp.GetTransaction())
}

func (c *Client) ListTransactions(ctx context.Context, userID int64, f ledgerclient.TransactionFilter) ([]ledgerclient.Transaction, error) {
	resp, err := c.api.ListTransactions(ctx, &ledgerv1.ListTransactionsRequest{UserId: userID, Filter: toFilter(f)})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]ledgerclient.Transaction, len(resp.GetTransactions()))
	for i, tx := range resp.GetTransactions() {
		if out[i], err = fromTransaction(tx); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *Client) ImportTransactions(ctx context.Context, userID int64, rows []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
	req := &ledgerv1.ImportTransactionsRequest{UserId: userID, Rows: make([]*ledgerv1.ImportRow, len(rows))}
	for i, r := range rows {
		req.Rows[i] = &ledgerv1.ImportRow{
			Line:          int32(r.Line),
			AmountKopecks: int64(r.Input.Amount),
			Category:      r.Input.Category,
			Description:   r.Input.Description,
			Date:          r.Input.Date.String(),
			ExternalId:    r.Input.ExternalID,
			SourceId:      r.SourceID,
		}
	}
	resp, err := c.api.ImportTransactions(grpckit.WithCallTimeout(ctx, c.importTimeout), req)
	if err != nil {
		return nil, translate(err)
	}
	if len(resp.GetResults()) != len(rows) {
		return nil, fmt.Errorf("ledger sent %d import results for %d rows", len(resp.GetResults()), len(rows))
	}
	out := make([]ledgerclient.ImportResult, len(rows))
	for i, r := range resp.GetResults() {
		res := ledgerclient.ImportResult{Line: int(r.GetLine())}
		switch o := r.GetOutcome().(type) {
		case *ledgerv1.ImportRowResult_Id:
			res.ID = o.Id
		case *ledgerv1.ImportRowResult_Error:
			res.Err = rowError(o.Error)
		default:
			res.Err = &ledgerclient.RowError{Code: "internal_error", Message: "ledger sent no outcome for the row"}
		}
		out[i] = res
	}
	return out, nil
}

// ExportTransactionsCSV collects the streamed chunks into one document.
// The timeout covers the whole stream, so it is set here rather than in an
// interceptor, which would have to outlive its call to cancel it.
func (c *Client) ExportTransactionsCSV(ctx context.Context, userID int64, f ledgerclient.TransactionFilter) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stream, err := c.api.ExportTransactionsCSV(ctx, &ledgerv1.ExportTransactionsCSVRequest{UserId: userID, Filter: toFilter(f)})
	if err != nil {
		return nil, translate(err)
	}
	var buf bytes.Buffer
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, translate(err)
		}
		buf.Write(msg.GetChunk())
	}
}

func (c *Client) UpsertBudget(ctx context.Context, userID int64, in ledgerclient.BudgetInput) (ledgerclient.Budget, bool, error) {
	resp, err := c.api.UpsertBudget(ctx, &ledgerv1.UpsertBudgetRequest{
		UserId:       userID,
		Category:     in.Category,
		LimitKopecks: int64(in.Limit),
		Period:       in.Period,
	})
	if err != nil {
		return ledgerclient.Budget{}, false, translate(err)
	}
	if resp.GetBudget() == nil {
		return ledgerclient.Budget{}, false, errors.New("ledger sent no budget")
	}
	return fromBudget(resp.GetBudget()), resp.GetCreated(), nil
}

func (c *Client) ListBudgets(ctx context.Context, userID int64) ([]ledgerclient.Budget, error) {
	resp, err := c.api.ListBudgets(ctx, &ledgerv1.ListBudgetsRequest{UserId: userID})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]ledgerclient.Budget, len(resp.GetBudgets()))
	for i, b := range resp.GetBudgets() {
		out[i] = fromBudget(b)
	}
	return out, nil
}

func (c *Client) Summary(ctx context.Context, userID int64, from, to civil.Date) (ledgerclient.Summary, error) {
	var header metadata.MD
	resp, err := c.api.GetSummary(ctx, &ledgerv1.GetSummaryRequest{UserId: userID, From: from.String(), To: to.String()}, grpc.Header(&header))
	if err != nil {
		return ledgerclient.Summary{}, translate(err)
	}
	out := ledgerclient.Summary{
		Total:      money.Money(resp.GetTotalKopecks()),
		Categories: make([]ledgerclient.CategorySummary, len(resp.GetCategories())),
	}
	if v := header.Get(ledgerv1.CacheHeader); len(v) > 0 {
		out.Cache = v[0]
	}
	if out.From, err = parseDate("from", resp.GetFrom()); err != nil {
		return ledgerclient.Summary{}, err
	}
	if out.To, err = parseDate("to", resp.GetTo()); err != nil {
		return ledgerclient.Summary{}, err
	}
	for i, row := range resp.GetCategories() {
		cs := ledgerclient.CategorySummary{Category: row.GetCategory(), Spent: money.Money(row.GetSpentKopecks())}
		if u := row.GetBudget(); u != nil {
			cs.Budget = &ledgerclient.BudgetUsage{
				Period:    u.GetPeriod(),
				Limit:     money.Money(u.GetLimitKopecks()),
				Remaining: money.Money(u.GetRemainingKopecks()),
				Percent:   u.GetPercent(),
			}
		}
		out.Categories[i] = cs
	}
	return out, nil
}

func toFilter(f ledgerclient.TransactionFilter) *ledgerv1.TransactionFilter {
	return &ledgerv1.TransactionFilter{
		From:     f.From.String(),
		To:       f.To.String(),
		Category: f.Category,
		Limit:    clampInt32(f.Limit),
		Offset:   clampInt32(f.Offset),
	}
}

// clampInt32 keeps a page parameter within int32 without wrapping around:
// a huge limit stays huge, which the ledger caps, rather than turning
// negative.
func clampInt32(n int) int32 {
	return int32(max(min(n, 1<<31-1), -1<<31))
}

// fromTransaction converts a transaction from the ledger. A missing or
// malformed one is a bug of the ledger and becomes an internal error.
func fromTransaction(tx *ledgerv1.Transaction) (ledgerclient.Transaction, error) {
	if tx == nil {
		return ledgerclient.Transaction{}, errors.New("ledger sent no transaction")
	}
	date, err := parseDate("transaction date", tx.GetDate())
	if err != nil {
		return ledgerclient.Transaction{}, err
	}
	out := ledgerclient.Transaction{
		ID:          tx.GetId(),
		Amount:      money.Money(tx.GetAmountKopecks()),
		Category:    tx.GetCategory(),
		Description: tx.GetDescription(),
		Date:        date,
		ExternalID:  tx.GetExternalId(),
	}
	if ts := tx.GetCreatedAt(); ts != nil {
		out.CreatedAt = ts.AsTime()
	}
	return out, nil
}

func fromBudget(b *ledgerv1.Budget) ledgerclient.Budget {
	return ledgerclient.Budget{Category: b.GetCategory(), Limit: money.Money(b.GetLimitKopecks()), Period: b.GetPeriod()}
}

// parseDate reads a date the ledger sent. A malformed one is a bug of the
// ledger, so it becomes an internal error rather than a client error.
func parseDate(field, s string) (civil.Date, error) {
	if s == "" {
		return civil.Date{}, nil
	}
	d, err := civil.Parse(s)
	if err != nil {
		return civil.Date{}, fmt.Errorf("ledger sent a bad %s: %w", field, err)
	}
	return d, nil
}
