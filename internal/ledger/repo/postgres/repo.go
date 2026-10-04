package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Repo implements usecase.Repository on a pgx connection pool.
type Repo struct {
	pool *pgxpool.Pool
}

// New returns a Repo using pool. The caller owns the pool.
func New(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

var _ usecase.Repository = (*Repo)(nil)

// InTx runs fn in one database transaction. The transaction is committed
// when fn returns nil and rolled back otherwise; fn's error is returned
// unchanged, so domain errors keep their type.
func (r *Repo) InTx(ctx context.Context, fn func(tx usecase.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := fn(&txRepo{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const transactionColumns = `id, user_id, amount_kopecks, category, description, occurred_on, COALESCE(external_id, ''), created_at`

// ListTransactions returns the transactions matching f, newest first.
// A Limit of zero means no limit, like in the memory repository.
func (r *Repo) ListTransactions(ctx context.Context, userID int64, f usecase.TransactionFilter) ([]domain.Transaction, error) {
	lo, hi := rangeBounds(f.From, f.To)
	query := `SELECT ` + transactionColumns + ` FROM transactions WHERE user_id = $1 AND occurred_on BETWEEN $2 AND $3`
	args := []any{userID, lo, hi}
	if f.Category != "" {
		args = append(args, f.Category)
		query += " AND category = $" + strconv.Itoa(len(args))
	}
	var limit *int // NULL means LIMIT ALL
	if f.Limit > 0 {
		limit = &f.Limit
	}
	args = append(args, limit, max(0, f.Offset))
	query += fmt.Sprintf(" ORDER BY occurred_on DESC, id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanTransaction)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.Transaction{}
	}
	return list, nil
}

// UpsertBudget inserts the budget or replaces the limit and period of the
// existing one. xmax = 0 is true only for a freshly inserted row.
func (r *Repo) UpsertBudget(ctx context.Context, b domain.Budget) (bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := lockCategory(ctx, tx, b.UserID, b.Category); err != nil {
		return false, err
	}
	if err := lockQuota(ctx, tx, b.UserID); err != nil {
		return false, err
	}
	if err := checkCategoryQuota(ctx, tx, b.UserID, b.Category); err != nil {
		return false, err
	}
	var created bool
	err = tx.QueryRow(ctx, `
		INSERT INTO budgets (user_id, category, limit_kopecks, period)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, category) DO UPDATE
		SET limit_kopecks = EXCLUDED.limit_kopecks, period = EXCLUDED.period, updated_at = now()
		RETURNING (xmax = 0)`,
		b.UserID, b.Category, b.Limit, b.Period).Scan(&created)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return created, nil
}

// ListBudgets returns the budgets of the user sorted by category.
func (r *Repo) ListBudgets(ctx context.Context, userID int64) ([]domain.Budget, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT user_id, category, limit_kopecks, period FROM budgets WHERE user_id = $1 ORDER BY category`, userID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Budget, error) {
		var b domain.Budget
		err := row.Scan(&b.UserID, &b.Category, &b.Limit, &b.Period)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []domain.Budget{}
	}
	return list, nil
}

// SpentByCategory sums the transactions of the user in [from, to] per category.
func (r *Repo) SpentByCategory(ctx context.Context, userID int64, from, to civil.Date) (map[string]money.Money, error) {
	lo, hi := rangeBounds(from, to)
	rows, err := r.pool.Query(ctx, `
		SELECT category, SUM(amount_kopecks)::bigint
		FROM transactions
		WHERE user_id = $1 AND occurred_on BETWEEN $2 AND $3
		GROUP BY category`,
		userID, lo, hi)
	if err != nil {
		return nil, aggregateError(err)
	}
	defer rows.Close()
	spent := map[string]money.Money{}
	for rows.Next() {
		var category string
		var sum money.Money
		if err := rows.Scan(&category, &sum); err != nil {
			return nil, aggregateError(err)
		}
		spent[category] = sum
	}
	return spent, aggregateError(rows.Err())
}

// txRepo is the view of the database inside InTx.
type txRepo struct {
	tx pgx.Tx
}

// HasTransaction reports whether the user has the transaction with id.
func (r *Repo) HasTransaction(ctx context.Context, userID, id int64) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM transactions WHERE id = $1 AND user_id = $2)`, id, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check transaction: %w", err)
	}
	return exists, nil
}

// BudgetForUpdate locks the category even when its first budget is absent.
func (t *txRepo) BudgetForUpdate(ctx context.Context, userID int64, category string) (*domain.Budget, error) {
	if err := lockCategory(ctx, t.tx, userID, category); err != nil {
		return nil, err
	}
	var b domain.Budget
	err := t.tx.QueryRow(ctx,
		`SELECT user_id, category, limit_kopecks, period FROM budgets WHERE user_id = $1 AND category = $2 FOR UPDATE`,
		userID, category).Scan(&b.UserID, &b.Category, &b.Limit, &b.Period)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (t *txRepo) SpentInRange(ctx context.Context, userID int64, category string, from, to civil.Date) (money.Money, error) {
	var sum money.Money
	lo, hi := rangeBounds(from, to)
	err := t.tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_kopecks), 0)::bigint
		FROM transactions
		WHERE user_id = $1 AND category = $2 AND occurred_on BETWEEN $3 AND $4`,
		userID, category, lo, hi).Scan(&sum)
	return sum, aggregateError(err)
}

func (t *txRepo) TransactionIDByExternalID(ctx context.Context, userID int64, externalID string) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx,
		`SELECT id FROM transactions WHERE user_id = $1 AND external_id = $2`, userID, externalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find by external id: %w", err)
	}
	return id, nil
}

// InsertTransaction stores tx. ON CONFLICT DO NOTHING keeps the transaction
// usable after a duplicate external_id, so the existing ID can be looked
// up and reported in the DuplicateError.
func (t *txRepo) InsertTransaction(ctx context.Context, tx domain.Transaction) (domain.Transaction, error) {
	if err := lockQuota(ctx, t.tx, tx.UserID); err != nil {
		return domain.Transaction{}, err
	}
	// Recheck after the quota lock: another category may have inserted the
	// same key since the service's earlier idempotency check.
	if tx.ExternalID != "" {
		id, err := t.TransactionIDByExternalID(ctx, tx.UserID, tx.ExternalID)
		if err != nil {
			return domain.Transaction{}, err
		}
		if id != 0 {
			return domain.Transaction{}, &domain.DuplicateError{ExternalID: tx.ExternalID, ExistingID: id}
		}
	}
	var count int
	if err := t.tx.QueryRow(ctx, "SELECT count(*) FROM transactions WHERE user_id=$1", tx.UserID).Scan(&count); err != nil {
		return domain.Transaction{}, err
	}
	if count >= domain.MaxTransactions {
		return domain.Transaction{}, domain.ErrQuotaExceeded
	}
	if err := checkCategoryQuota(ctx, t.tx, tx.UserID, tx.Category); err != nil {
		return domain.Transaction{}, err
	}
	var externalID *string
	if tx.ExternalID != "" {
		externalID = &tx.ExternalID
	}
	err := t.tx.QueryRow(ctx, `
		INSERT INTO transactions (user_id, amount_kopecks, category, description, occurred_on, external_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, external_id) WHERE external_id IS NOT NULL DO NOTHING
		RETURNING id, created_at`,
		tx.UserID, tx.Amount, tx.Category, tx.Description, tx.Date.Time(), externalID).Scan(&tx.ID, &tx.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, err := t.TransactionIDByExternalID(ctx, tx.UserID, tx.ExternalID)
		if err != nil {
			return domain.Transaction{}, fmt.Errorf("find duplicate: %w", err)
		}
		return domain.Transaction{}, &domain.DuplicateError{ExternalID: tx.ExternalID, ExistingID: existing}
	}
	if err != nil {
		return domain.Transaction{}, err
	}
	tx.CreatedAt = tx.CreatedAt.UTC()
	return tx, nil
}

func scanTransaction(row pgx.CollectableRow) (domain.Transaction, error) {
	var tx domain.Transaction
	var occurredOn time.Time
	err := row.Scan(&tx.ID, &tx.UserID, &tx.Amount, &tx.Category, &tx.Description, &occurredOn, &tx.ExternalID, &tx.CreatedAt)
	if err != nil {
		return domain.Transaction{}, err
	}
	tx.Date = civil.Of(occurredOn)
	tx.CreatedAt = tx.CreatedAt.UTC()
	return tx, nil
}

// rangeBounds turns open bounds into the widest dates PostgreSQL accepts,
// so one BETWEEN query serves every case.
func rangeBounds(from, to civil.Date) (time.Time, time.Time) {
	if from.IsZero() {
		from = domain.MinDate
	}
	if to.IsZero() {
		to = domain.MaxDate
	}
	return from.Time(), to.Time()
}

// The transaction-scoped lock also covers absent rows and is shared with
// UpsertBudget. A hash collision only serializes unrelated categories.
func lockCategory(ctx context.Context, tx pgx.Tx, userID int64, category string) error {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(userID, 10) + "\x00" + category))
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(h.Sum64()))
	return err
}

func aggregateError(err error) error {
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && pgerr.Code == "22003" {
		return money.ErrOverflow
	}
	return err
}

// Two-int advisory locks occupy a separate PostgreSQL key space from the
// category locks. All writes check quotas while holding this per-user lock.
func lockQuota(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", int32(userID>>32), int32(userID))
	return err
}

func checkCategoryQuota(ctx context.Context, tx pgx.Tx, userID int64, category string) error {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM budgets WHERE user_id=$1 AND category=$2)
		OR EXISTS (SELECT FROM transactions WHERE user_id=$1 AND category=$2)`, userID, category).Scan(&exists)
	if err != nil || exists {
		return err
	}
	var count int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT category FROM budgets WHERE user_id=$1
		UNION SELECT category FROM transactions WHERE user_id=$1) AS categories`, userID).Scan(&count)
	if err != nil {
		return err
	}
	if count >= domain.MaxCategories {
		return domain.ErrQuotaExceeded
	}
	return nil
}
