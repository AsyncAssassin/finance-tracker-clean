package postgres_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/repotest"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
)

// The integration tests need a real database and are skipped without
// TEST_DATABASE_URL. They truncate the users table, so they must not run
// against the database of a running stack; make test-integration runs them
// in a separate finance_test database.
var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
	poolErr  error
)

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

func TestContract(t *testing.T) {
	repotest.Run(t, func(t *testing.T) usecase.Repository {
		p := connect(t)
		if _, err := p.Exec(context.Background(), `TRUNCATE users RESTART IDENTITY`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return postgres.New(p)
	})
}

func TestMigrateIsIdempotent(t *testing.T) {
	p := connect(t)
	if err := postgres.Migrate(context.Background(), p); err != nil {
		t.Errorf("second Migrate: %v", err)
	}
}
