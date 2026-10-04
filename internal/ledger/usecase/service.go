// Package usecase implements the operations of the ledger on top of the
// domain rules and a Repository. It is the only place that decides what
// happens in which storage transaction.
package usecase

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

// Paging and range limits.
const (
	DefaultPageSize = 100
	MaxPageSize     = 1000
	ExportLimit     = 100_000 // transactions in one CSV export
	MaxSummaryYears = 10      // longest report range
)

// Service runs the ledger operations.
type Service struct {
	repo Repository
	now  func() time.Time
	reportCache
	// importSlots bounds the import rows stored at once, see WithImportWorkers.
	importSlots chan struct{}
}

// Option configures a Service.
type Option func(*Service)

// WithClock replaces the clock used for default dates, mainly in tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// New returns a Service backed by repo.
func New(repo Repository, opts ...Option) *Service {
	s := &Service{repo: repo, now: time.Now, importSlots: make(chan struct{}, DefaultImportWorkers)}
	s.log = slog.Default()
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateTransaction validates in, checks the budget of its category and
// stores the transaction. The check and the insert run in one storage
// transaction with the budget row locked, so two concurrent requests cannot
// both pass the check. A transaction over the budget returns
// a *domain.BudgetExceededError and is not stored.
func (s *Service) CreateTransaction(ctx context.Context, userID int64, in domain.TransactionInput) (domain.Transaction, error) {
	created, err := s.createTransaction(ctx, userID, in)
	if err != nil {
		return domain.Transaction{}, err
	}
	s.invalidateReports(ctx, userID)
	return created, nil
}

// createTransaction is CreateTransaction without the report cache
// invalidation, which Import does once for all rows.
func (s *Service) createTransaction(ctx context.Context, userID int64, in domain.TransactionInput) (domain.Transaction, error) {
	tx, err := domain.NewTransaction(userID, in, civil.Of(s.now()))
	if err != nil {
		return domain.Transaction{}, err
	}
	var created domain.Transaction
	err = s.repo.InTx(ctx, func(r Tx) error {
		budget, err := r.BudgetForUpdate(ctx, userID, tx.Category)
		if err != nil {
			return fmt.Errorf("lock budget: %w", err)
		}
		// A resent transaction is reported as the duplicate it is, even
		// when it would no longer fit into the budget. The check comes
		// after the lock, so a concurrent insert of the same category has
		// committed by now.
		if tx.ExternalID != "" {
			existing, err := r.TransactionIDByExternalID(ctx, userID, tx.ExternalID)
			if err != nil {
				return err
			}
			if existing != 0 {
				return &domain.DuplicateError{ExternalID: tx.ExternalID, ExistingID: existing}
			}
		}
		if budget != nil {
			from, to := budget.Period.Bounds(tx.Date)
			spent, err := r.SpentInRange(ctx, userID, tx.Category, from, to)
			if err != nil {
				return fmt.Errorf("sum spent: %w", err)
			}
			if err := domain.CheckBudget(*budget, tx.Date, spent, tx.Amount); err != nil {
				return err
			}
		}
		created, err = r.InsertTransaction(ctx, tx)
		return err
	})
	if err != nil {
		return domain.Transaction{}, err
	}
	return created, nil
}

// ListTransactions returns the transactions of the user matching f,
// newest first.
func (s *Service) ListTransactions(ctx context.Context, userID int64, f TransactionFilter) ([]domain.Transaction, error) {
	f, err := normalizeFilter(f, DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListTransactions(ctx, userID, f)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	if list == nil {
		list = []domain.Transaction{}
	}
	return list, nil
}

// ExportCSV returns the transactions matching f as a CSV document with the
// columns id, date, category, amount, description and external_id. f.Limit and f.Offset
// are ignored: the export covers up to ExportLimit transactions.
func (s *Service) ExportCSV(ctx context.Context, userID int64, f TransactionFilter) ([]byte, error) {
	f.Limit, f.Offset = ExportLimit, 0
	f, err := normalizeFilter(f, ExportLimit, ExportLimit)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.ListTransactions(ctx, userID, f)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, list); err != nil {
		return nil, fmt.Errorf("write csv: %w", err)
	}
	return buf.Bytes(), nil
}

// UpsertBudget validates in and creates or replaces the budget of its
// category. The bool reports whether the budget was created.
func (s *Service) UpsertBudget(ctx context.Context, userID int64, in domain.BudgetInput) (domain.Budget, bool, error) {
	b, err := domain.NewBudget(userID, in)
	if err != nil {
		return domain.Budget{}, false, err
	}
	created, err := s.repo.UpsertBudget(ctx, b)
	if err != nil {
		return domain.Budget{}, false, fmt.Errorf("upsert budget: %w", err)
	}
	s.invalidateReports(ctx, userID)
	return b, created, nil
}

// ListBudgets returns the budgets of the user sorted by category.
func (s *Service) ListBudgets(ctx context.Context, userID int64) ([]domain.Budget, error) {
	list, err := s.repo.ListBudgets(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	if list == nil {
		list = []domain.Budget{}
	}
	return list, nil
}

// Summary returns the expense report for [from, to], from the report cache
// when one is configured (see WithReportCache). Both dates are required and
// the range may not exceed MaxSummaryYears.
func (s *Service) Summary(ctx context.Context, userID int64, from, to civil.Date) (Report, error) {
	v := new(domain.ValidationError)
	if from.IsZero() {
		v.Add("from", "is required")
	}
	if to.IsZero() {
		v.Add("to", "is required")
	}
	if !from.IsZero() && !to.IsZero() {
		validateRange(v, from, to)
		if to.After(from.AddMonths(12 * MaxSummaryYears)) {
			v.Add("to", fmt.Sprintf("range must not exceed %d years", MaxSummaryYears))
		}
	}
	if err := v.ErrOrNil(); err != nil {
		return Report{}, err
	}
	return s.cachedSummary(ctx, userID, from, to, func() (domain.Summary, error) {
		return s.buildSummary(ctx, userID, from, to)
	})
}

// buildSummary builds the report from the database.
func (s *Service) buildSummary(ctx context.Context, userID int64, from, to civil.Date) (domain.Summary, error) {
	spent, err := s.repo.SpentByCategory(ctx, userID, from, to)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("sum spent: %w", err)
	}
	budgets, err := s.repo.ListBudgets(ctx, userID)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("list budgets: %w", err)
	}
	return domain.BuildSummary(from, to, spent, budgets)
}

// normalizeFilter validates f and applies defaultLimit to a zero limit and
// maxLimit as the cap, so a client cannot ask for more rows than the
// operation allows.
func normalizeFilter(f TransactionFilter, defaultLimit, maxLimit int) (TransactionFilter, error) {
	v := new(domain.ValidationError)
	if strings.TrimSpace(f.Category) == "" {
		f.Category = ""
	} else {
		category, err := domain.NormalizeCategory(f.Category)
		if err != nil {
			v.Add("category", err.Error())
		}
		f.Category = category
	}
	if !f.From.IsZero() && !f.To.IsZero() {
		validateRange(v, f.From, f.To)
	}
	switch {
	case f.Limit < 0:
		v.Add("limit", "must not be negative")
	case f.Limit == 0:
		f.Limit = defaultLimit
	case f.Limit > maxLimit:
		f.Limit = maxLimit
	}
	if f.Offset < 0 {
		v.Add("offset", "must not be negative")
	}
	if err := v.ErrOrNil(); err != nil {
		return TransactionFilter{}, err
	}
	return f, nil
}

func validateRange(v *domain.ValidationError, from, to civil.Date) {
	if to.Before(from) {
		v.Add("to", "must not be before from")
	}
}
