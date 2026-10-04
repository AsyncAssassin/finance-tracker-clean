// Package memory is an in-memory Repository for the ledger. It is the
// storage of the first iterations and the fake behind the use-case tests.
package memory

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Repo stores transactions and budgets in memory. It is safe for
// concurrent use: one mutex serializes every operation, and InTx holds it
// for the whole transaction, which gives the same isolation as a locked
// budget row in a database.
type Repo struct {
	mu      sync.Mutex
	nextID  int64
	txs     []domain.Transaction
	budgets map[int64]map[string]domain.Budget // by user, then by category
	now     func() time.Time
}

// New returns an empty Repo.
func New() *Repo {
	return &Repo{budgets: map[int64]map[string]domain.Budget{}, now: time.Now}
}

var _ usecase.Repository = (*Repo)(nil)

// InTx runs fn under the repository lock. Transactions inserted through
// the Tx are kept only when fn returns nil.
func (r *Repo) InTx(ctx context.Context, fn func(tx usecase.Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	t := &tx{repo: r}
	if err := fn(t); err != nil {
		return err
	}
	r.txs = append(r.txs, t.pending...)
	return nil
}

// ListTransactions returns copies of the transactions matching f, newest first.
func (r *Repo) ListTransactions(_ context.Context, userID int64, f usecase.TransactionFilter) ([]domain.Transaction, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []domain.Transaction
	for _, tx := range r.txs {
		if tx.UserID == userID && (f.Category == "" || tx.Category == f.Category) && inRange(tx.Date, f.From, f.To) {
			list = append(list, tx)
		}
	}
	slices.SortFunc(list, func(a, b domain.Transaction) int {
		if c := b.Date.Compare(a.Date); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	if f.Offset >= len(list) {
		return []domain.Transaction{}, nil
	}
	list = list[f.Offset:]
	if f.Limit > 0 && f.Limit < len(list) {
		list = list[:f.Limit]
	}
	return slices.Clone(list), nil
}

// UpsertBudget stores b and reports whether the category had no budget before.
func (r *Repo) UpsertBudget(_ context.Context, b domain.Budget) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkQuota(b.UserID, b.Category, nil, false); err != nil {
		return false, err
	}
	byCategory, ok := r.budgets[b.UserID]
	if !ok {
		byCategory = map[string]domain.Budget{}
		r.budgets[b.UserID] = byCategory
	}
	_, exists := byCategory[b.Category]
	byCategory[b.Category] = b
	return !exists, nil
}

// ListBudgets returns the budgets of the user sorted by category.
func (r *Repo) ListBudgets(_ context.Context, userID int64) ([]domain.Budget, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := make([]domain.Budget, 0, len(r.budgets[userID]))
	for _, b := range r.budgets[userID] {
		list = append(list, b)
	}
	slices.SortFunc(list, func(a, b domain.Budget) int {
		return strings.Compare(a.Category, b.Category)
	})
	return list, nil
}

// SpentByCategory sums the transactions of the user in [from, to] per category.
func (r *Repo) SpentByCategory(_ context.Context, userID int64, from, to civil.Date) (map[string]money.Money, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	spent := map[string]money.Money{}
	for _, tx := range r.txs {
		if tx.UserID == userID && inRange(tx.Date, from, to) {
			var err error
			spent[tx.Category], err = money.Add(spent[tx.Category], tx.Amount)
			if err != nil {
				return nil, err
			}
		}
	}
	return spent, nil
}

// tx is the view of the repository inside InTx. The lock is already held.
type tx struct {
	repo    *Repo
	pending []domain.Transaction
}

func (t *tx) BudgetForUpdate(_ context.Context, userID int64, category string) (*domain.Budget, error) {
	b, ok := t.repo.budgets[userID][category]
	if !ok {
		return nil, nil
	}
	return &b, nil
}

func (t *tx) SpentInRange(_ context.Context, userID int64, category string, from, to civil.Date) (money.Money, error) {
	var total money.Money
	for _, list := range [][]domain.Transaction{t.repo.txs, t.pending} {
		for _, tx := range list {
			if tx.UserID == userID && tx.Category == category && inRange(tx.Date, from, to) {
				var err error
				total, err = money.Add(total, tx.Amount)
				if err != nil {
					return 0, err
				}
			}
		}
	}
	return total, nil
}

func (t *tx) TransactionIDByExternalID(_ context.Context, userID int64, externalID string) (int64, error) {
	for _, list := range [][]domain.Transaction{t.repo.txs, t.pending} {
		for _, tx := range list {
			if tx.UserID == userID && tx.ExternalID == externalID {
				return tx.ID, nil
			}
		}
	}
	return 0, nil
}

func (t *tx) InsertTransaction(ctx context.Context, in domain.Transaction) (domain.Transaction, error) {
	if in.ExternalID != "" {
		if existing, _ := t.TransactionIDByExternalID(ctx, in.UserID, in.ExternalID); existing != 0 {
			return domain.Transaction{}, &domain.DuplicateError{ExternalID: in.ExternalID, ExistingID: existing}
		}
	}
	if err := t.repo.checkQuota(in.UserID, in.Category, t.pending, true); err != nil {
		return domain.Transaction{}, err
	}
	t.repo.nextID++
	in.ID = t.repo.nextID
	in.CreatedAt = t.repo.now().UTC()
	t.pending = append(t.pending, in)
	return in, nil
}

// The caller holds mu; pending rows are part of the same transaction.
func (r *Repo) checkQuota(userID int64, category string, pending []domain.Transaction, inserting bool) error {
	categories := make(map[string]bool)
	for name := range r.budgets[userID] {
		categories[name] = true
	}
	count := 0
	for _, list := range [][]domain.Transaction{r.txs, pending} {
		for _, tx := range list {
			if tx.UserID == userID {
				count++
				categories[tx.Category] = true
			}
		}
	}
	if inserting && count >= domain.MaxTransactions || !categories[category] && len(categories) >= domain.MaxCategories {
		return domain.ErrQuotaExceeded
	}
	return nil
}

// HasTransaction reports whether the user has the transaction with id.
func (r *Repo) HasTransaction(ctx context.Context, userID, id int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tx := range r.txs {
		if tx.ID == id && tx.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

// inRange reports whether d is within [from, to]; a zero bound is open.
func inRange(d, from, to civil.Date) bool {
	return (from.IsZero() || !d.Before(from)) && (to.IsZero() || !d.After(to))
}
