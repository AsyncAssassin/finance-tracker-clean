package postgres_test

import (
	"context"
	"errors"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/repotest"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
)

// The integration tests need a real database and are skipped without
// TEST_DATABASE_URL. They truncate the tables, so they must not run against
// the database of a running stack: make test-integration creates a separate
// finance_test database and runs them there.
var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

// connect migrates the test database once and returns a shared pool.
func connect(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping the PostgreSQL integration tests")
	}
	poolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		pool, poolErr = pgkit.Connect(ctx, url, 10*time.Second, 0)
		if poolErr != nil {
			return
		}
		poolErr = postgres.Migrate(ctx, pool)
	})
	if poolErr != nil {
		t.Fatalf("connect to the test database: %v", poolErr)
	}
	return pool
}

// freshRepo empties the tables and returns a repository on them.
func freshRepo(t *testing.T) usecase.Repository {
	t.Helper()
	p := connect(t)
	if _, err := p.Exec(context.Background(), `TRUNCATE transactions, budgets RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return postgres.New(p)
}

func TestContract(t *testing.T) {
	repotest.Run(t, freshRepo)
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := postgres.Migrate(ctx, p); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	// Two instances starting at once must not trip over each other.
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- postgres.Migrate(ctx, p) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Migrate: %v", err)
		}
	}
}

// TestDatesRoundTrip makes sure a civil date is stored and read back as
// the same calendar day whatever the time zone of the process.
func TestDatesRoundTrip(t *testing.T) {
	r := freshRepo(t)
	ctx := context.Background()
	for _, d := range []civil.Date{
		civil.New(2026, time.January, 1),
		civil.New(2024, time.February, 29),
		civil.New(2026, time.December, 31),
		domain.MinDate,
		domain.MaxDate,
	} {
		err := r.InTx(ctx, func(tx usecase.Tx) error {
			_, err := tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Amount: 1, Category: "x", Date: d})
			return err
		})
		if err != nil {
			t.Fatalf("insert %s: %v", d, err)
		}
	}
	list, err := r.ListTransactions(ctx, 1, usecase.TransactionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"9999-12-31", "2026-12-31", "2026-01-01", "2024-02-29", "0001-01-01"}
	if len(list) != len(want) {
		t.Fatalf("got %d rows, want %d", len(list), len(want))
	}
	for i, tx := range list {
		if tx.Date.String() != want[i] {
			t.Errorf("row %d: date = %s, want %s", i, tx.Date, want[i])
		}
	}
}

// A missing budget must still lock the category. Otherwise the first
// budget and another expense can commit before this unbudgeted expense.
func TestFirstBudgetWaitsForTheCategoryTransaction(t *testing.T) {
	r := freshRepo(t)
	ctx := context.Background()
	err := r.InTx(ctx, func(tx usecase.Tx) error {
		b, err := tx.BudgetForUpdate(ctx, 1, "new category")
		if err != nil || b != nil {
			t.Fatalf("initial budget: %v, %v", b, err)
		}
		for _, other := range []domain.Budget{
			{UserID: 2, Category: "new category", Limit: 1, Period: domain.PeriodNone},
			{UserID: 1, Category: "different", Limit: 1, Period: domain.PeriodNone},
		} {
			otherCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := r.UpsertBudget(otherCtx, other)
			cancel()
			if err != nil {
				t.Fatalf("unrelated budget blocked: %v", err)
			}
		}
		blockedCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, err = r.UpsertBudget(blockedCtx, domain.Budget{UserID: 1, Category: "new category", Limit: 1, Period: domain.PeriodNone})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first budget must wait for commit; got %v", err)
		}
		_, err = tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Category: "new category", Amount: 1, Date: domain.MinDate})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := usecase.New(r)
	if _, _, err := svc.UpsertBudget(ctx, 1, domain.BudgetInput{Category: "new category", Limit: 1, Period: "none"}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateTransaction(ctx, 1, domain.TransactionInput{Category: "new category", Amount: 1, Date: domain.MinDate})
	var exceeded *domain.BudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("committed expense was not counted: %v", err)
	}
}

func TestAggregateOverflow(t *testing.T) {
	r := freshRepo(t)
	ctx := context.Background()
	p := connect(t)
	_, err := p.Exec(ctx, `INSERT INTO transactions (user_id,amount_kopecks,category,occurred_on)
		SELECT 1,$1::bigint,'large','2026-10-05'::date FROM generate_series(1,$2::bigint)`,
		int64(money.Max), int64(math.MaxInt64/int64(money.Max)+1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SpentByCategory(ctx, 1, domain.MinDate, domain.MaxDate); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("category aggregate: %v", err)
	}
	err = r.InTx(ctx, func(tx usecase.Tx) error {
		_, err := tx.SpentInRange(ctx, 1, "large", domain.MinDate, domain.MaxDate)
		return err
	})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("budget aggregate: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE transactions SET category='other' WHERE id%2=0`); err != nil {
		t.Fatal(err)
	}
	_, err = usecase.New(r).Summary(ctx, 1, civil.New(2026, time.October, 1), civil.New(2026, time.October, 31))
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("report total: %v", err)
	}
}
