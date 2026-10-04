// Package postgres is the PostgreSQL Repository of accounts. The schema
// comes from the goose migrations in migrations/auth, applied with Migrate
// at service startup and tracked in their own version table.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
	"github.com/AsyncAssassin/finance-tracker-clean/migrations"
)

// versionTable is the goose bookkeeping table of the auth service.
const versionTable = "goose_auth_version"

// Migrate applies the pending auth migrations through pool; see
// pgkit.Migrate.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pgkit.Migrate(ctx, pool, migrations.Auth(), versionTable)
}

// Repo stores accounts in the users table.
type Repo struct {
	pool *pgxpool.Pool
}

// New returns a Repo on pool.
func New(pool *pgxpool.Pool) *Repo {
	return &Repo{pool: pool}
}

var _ usecase.Repository = (*Repo)(nil)

func (r *Repo) CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error) {
	u := domain.User{Email: email, PasswordHash: passwordHash}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return domain.User{}, fmt.Errorf("begin registration: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// The quota and insert share one lock across all Auth instances.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1179931218, 1)"); err != nil {
		return domain.User{}, err
	}
	var taken bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM users WHERE email=$1)", email).Scan(&taken); err != nil {
		return domain.User{}, err
	}
	if taken {
		return domain.User{}, domain.ErrEmailTaken
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return domain.User{}, err
	}
	if count >= domain.MaxAccounts {
		return domain.User{}, domain.ErrAccountQuota
	}
	// A duplicate is an expected result, not a database error whose DETAIL
	// would expose the account email in PostgreSQL logs.
	err = tx.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, $2)
		 ON CONFLICT (email) DO NOTHING RETURNING id, created_at`,
		email, passwordHash,
	).Scan(&u.ID, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrEmailTaken
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("insert user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.User{}, fmt.Errorf("commit registration: %w", err)
	}
	return u, nil
}

func (r *Repo) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	var u domain.User
	err := r.pool.QueryRow(ctx,
		`SELECT id, email, password_hash, created_at FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("select user: %w", err)
	}
	return u, nil
}
