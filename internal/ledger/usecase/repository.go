package usecase

import (
	"context"
	"errors"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// ErrCommitUncertain means storage attempted COMMIT but could not confirm its
// outcome. The write may be durable: invalidate reports and do not retry it.
var ErrCommitUncertain = errors.New("transaction commit outcome is unknown")

// Repository is the storage the ledger needs. Implementations live under
// internal/ledger/repo and know nothing about the business rules.
type Repository interface {
	// InTx runs fn in one storage transaction and commits it when fn
	// returns nil. Everything done through tx is discarded when fn returns
	// an error, and that error is returned unchanged.
	InTx(ctx context.Context, fn func(tx Tx) error) error

	// ListTransactions returns the transactions of the user that match f,
	// newest first (by date, then by ID). A zero f.Limit means no limit;
	// the service always sets one.
	ListTransactions(ctx context.Context, userID int64, f TransactionFilter) ([]domain.Transaction, error)

	// HasTransaction reports whether the user has the transaction with id.
	HasTransaction(ctx context.Context, userID, id int64) (bool, error)

	// UpsertBudget creates the budget of b.Category or replaces the
	// existing one and reports whether it was created. It uses the same
	// category lock as Tx.BudgetForUpdate, including the first insert.
	UpsertBudget(ctx context.Context, b domain.Budget) (created bool, err error)

	// ListBudgets returns the budgets of the user sorted by category.
	ListBudgets(ctx context.Context, userID int64) ([]domain.Budget, error)

	// SpentByCategory sums the transactions of the user dated within
	// [from, to] per category. Categories without transactions are absent.
	SpentByCategory(ctx context.Context, userID int64, from, to civil.Date) (map[string]money.Money, error)
}

// Tx is the part of the storage available inside Repository.InTx.
type Tx interface {
	// BudgetForUpdate returns the budget of the category locked until the
	// end of the transaction, so concurrent budget checks of the same
	// category run one after another. It returns nil when there is none,
	// while still locking the category against UpsertBudget.
	BudgetForUpdate(ctx context.Context, userID int64, category string) (*domain.Budget, error)

	// TransactionIDByExternalID returns the ID of the user's transaction
	// with externalID, or 0 when there is none.
	TransactionIDByExternalID(ctx context.Context, userID int64, externalID string) (int64, error)

	// SpentInRange sums the transactions of the category dated within [from, to].
	SpentInRange(ctx context.Context, userID int64, category string, from, to civil.Date) (money.Money, error)

	// InsertTransaction stores tx and returns it with its ID and creation
	// time. It returns a *domain.DuplicateError when tx.ExternalID is
	// already used by the user.
	InsertTransaction(ctx context.Context, tx domain.Transaction) (domain.Transaction, error)
}

// TransactionFilter selects transactions for listing and export.
type TransactionFilter struct {
	From     civil.Date // zero means no lower bound
	To       civil.Date // zero means no upper bound
	Category string     // empty means all categories; normalized by the service
	Limit    int        // 0 means DefaultPageSize; capped at MaxPageSize
	Offset   int
}
