// Package pgkit connects the services to PostgreSQL and applies their goose
// migrations in a separate process. Each service keeps its own migrations and its own
// goose version table, so they evolve independently in one database.
package pgkit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Timeouts of one connection attempt and the pause between attempts.
const (
	attemptTimeout = 5 * time.Second
	retryInterval  = time.Second
)

// Connect opens a connection pool for url and waits up to wait for the
// database to answer, retrying every second: in Docker Compose the
// database may still be starting when the service comes up. Every attempt
// is bounded by attemptTimeout, and errors that retrying cannot fix, such
// as a wrong password, stop the wait at once. Credentials may come from
// the URL or from the libpq variables PGUSER and PGPASSWORD. maxConns sets
// the pool size; 0 keeps the pgx default, max(4, number of CPUs).
func Connect(ctx context.Context, url string, wait time.Duration, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("invalid DATABASE_URL: check connection parameters")
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = attemptTimeout
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		err = ping(ctx, pool)
		if err == nil {
			return pool, nil
		}
		if permanent(err) || ctx.Err() != nil || time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("ping database: %w", err)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

func ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	return pool.Ping(ctx)
}

// permanent reports errors that retrying cannot fix: wrong credentials or
// a database that does not exist. The codes are PostgreSQL SQLSTATEs.
func permanent(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "28P01", // invalid_password
		"28000", // invalid_authorization_specification
		"3D000": // invalid_catalog_name
		return true
	}
	return false
}

// Migrate applies the pending migrations in fsys through pool and records
// them in versionTable. It is safe to call at every start: applied
// versions are skipped, and a session-level advisory lock keeps two
// instances starting at the same time from running the same migration
// twice.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, versionTable string, goMigrations ...*goose.Migration) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close() // closes only the database/sql wrapper, not the pool

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("goose locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys,
		goose.WithTableName(versionTable),
		goose.WithSessionLocker(locker),
		goose.WithGoMigrations(goMigrations...),
	)
	if err != nil {
		return fmt.Errorf("goose provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		var partial *goose.PartialError
		if errors.As(err, &partial) {
			return fmt.Errorf("migrate: %s failed: %w", partial.Failed.Source.Path, partial.Err)
		}
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
