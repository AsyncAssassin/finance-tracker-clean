// Package postgres is the PostgreSQL Repository of the ledger. Queries go
// through pgx; the schema comes from the goose migrations embedded in the
// migrations package and is applied by cmd/migrate.
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
	"github.com/AsyncAssassin/finance-tracker-clean/migrations"
)

// versionTable is the goose bookkeeping table of the ledger. Each service
// keeps its own, so their migrations do not interfere.
const versionTable = "goose_ledger_version"

// Migrate applies the pending ledger migrations through pool; see
// pgkit.Migrate.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return pgkit.Migrate(ctx, pool, migrations.Ledger(), versionTable,
		goose.NewGoMigration(2, &goose.GoFunc{RunTx: normalizeCategories}, nil))
}
