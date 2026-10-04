// Package ledgerclient defines how the gateway talks to the ledger. The
// gateway depends only on this interface and these types; the grpcclient
// subpackage implements it over gRPC. Keeping the ledger's own domain types
// out of the gateway keeps the two services independent, and the HTTP
// layer is tested with a fake Client.
package ledgerclient

import (
	"context"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Client is the ledger as seen by the gateway. Every method is scoped to
// the user whose ID the gateway took from the request.
type Client interface {
	CreateTransaction(ctx context.Context, userID int64, in TransactionInput) (Transaction, error)
	ListTransactions(ctx context.Context, userID int64, f TransactionFilter) ([]Transaction, error)
	ExportTransactionsCSV(ctx context.Context, userID int64, f TransactionFilter) ([]byte, error)
	// ImportTransactions stores rows and returns a result for each, in the
	// same order. Errors of single rows are in the results; the error is
	// for the import as a whole.
	ImportTransactions(ctx context.Context, userID int64, rows []ImportRow) ([]ImportResult, error)
	// UpsertBudget creates or replaces the budget of a category; the bool
	// reports whether it was created.
	UpsertBudget(ctx context.Context, userID int64, in BudgetInput) (Budget, bool, error)
	ListBudgets(ctx context.Context, userID int64) ([]Budget, error)
	Summary(ctx context.Context, userID int64, from, to civil.Date) (Summary, error)
}

// TransactionInput describes a transaction to create.
type TransactionInput struct {
	Amount      money.Money
	Category    string
	Description string
	Date        civil.Date // zero means today
	ExternalID  string
}

// Transaction is a stored expense. The JSON tags define the HTTP API form.
type Transaction struct {
	ID          int64       `json:"id"`
	Amount      money.Money `json:"amount"`
	Category    string      `json:"category"`
	Description string      `json:"description"`
	Date        civil.Date  `json:"date"`
	ExternalID  string      `json:"external_id,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
}

// ImportRow is one transaction of an import and its line in the file.
type ImportRow struct {
	Line  int
	Input TransactionInput
	// SourceID is the id column of an exported file, 0 when absent: the
	// ledger does not store again a transaction the user already has.
	SourceID int64
}

// DefaultImportTimeout bounds the ledger call of an import when nothing
// else is configured: thousands of rows take longer than an ordinary
// call. The gateway gives the reading of the body the same time on its
// own, so an import request may take longer than one timeout in all.
const DefaultImportTimeout = time.Minute

// ImportResult is the outcome of one row: the ID of the stored transaction,
// or why it was not stored: *upstream.ValidationError, *BudgetExceededError,
// *DuplicateError or *RowError.
type ImportResult struct {
	Line int
	ID   int64
	Err  error
}

// TransactionFilter selects transactions for listing and export.
type TransactionFilter struct {
	From, To civil.Date // zero means unbounded
	Category string     // empty means all
	Limit    int        // 0 means the ledger's default page size
	Offset   int
}

// BudgetInput describes a budget to create or replace.
type BudgetInput struct {
	Category string
	Limit    money.Money
	Period   string // "month" (default), "year" or "none"
}

// Budget is a stored budget.
type Budget struct {
	Category string      `json:"category"`
	Limit    money.Money `json:"limit"`
	Period   string      `json:"period"`
}

// Summary is the expense report for a date range.
type Summary struct {
	From       civil.Date        `json:"from"`
	To         civil.Date        `json:"to"`
	Total      money.Money       `json:"total"`
	Categories []CategorySummary `json:"categories"`
	// Cache tells how the ledger served the report: "HIT", "MISS" or ""
	// without a cache. It travels in the X-Cache header, not in the body.
	Cache string `json:"-"`
}

// CategorySummary is one row of the report.
type CategorySummary struct {
	Category string       `json:"category"`
	Spent    money.Money  `json:"spent"`
	Budget   *BudgetUsage `json:"budget"` // null when the category has no budget
}

// BudgetUsage compares the expenses of a category with its budget.
type BudgetUsage struct {
	Period    string      `json:"period"`
	Limit     money.Money `json:"limit"` // for the whole range
	Remaining money.Money `json:"remaining"`
	Percent   float64     `json:"percent"`
}
