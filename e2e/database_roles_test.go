//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestDatabaseRoles uses the isolated Compose database. HTTP tests exercise
// permitted operations; these checks verify the privileges an attacker lacks.
func TestDatabaseRoles(t *testing.T) {
	url := os.Getenv("TEST_ROLE_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_ROLE_DATABASE_URL is not set")
	}
	for _, service := range []string{"auth", "ledger"} {
		for _, kind := range []string{"app", "migrator"} {
			t.Run(service+"_"+kind, func(t *testing.T) {
				cfg, err := pgx.ParseConfig(url)
				if err != nil {
					t.Fatal("invalid test database URL")
				}
				key := "AUTH_DB_PASSWORD"
				if service == "ledger" {
					key = "LEDGER_DB_PASSWORD"
				}
				if kind == "migrator" {
					if service == "auth" {
						key = "AUTH_MIGRATION_PASSWORD"
					} else {
						key = "LEDGER_MIGRATION_PASSWORD"
					}
				}
				cfg.User, cfg.Password = service+"_"+kind, os.Getenv(key)
				if cfg.Password == "" {
					t.Fatal(key + " is required")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				conn, err := pgx.ConnectConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close(ctx)
				var schema string
				var elevated bool
				err = conn.QueryRow(ctx, `SELECT current_schema(), rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&schema, &elevated)
				if err != nil {
					t.Fatal(err)
				}
				if schema != service || elevated {
					t.Fatalf("schema=%s elevated=%v", schema, elevated)
				}
				denied := []string{
					"CREATE TABLE public.forbidden(id integer)",
					"CREATE TEMP TABLE forbidden(id integer)",
					"CREATE SCHEMA forbidden",
					"SET ROLE finance",
				}
				if service == "auth" {
					denied = append(denied, "SELECT * FROM ledger.transactions", "SET ROLE ledger_migrator")
				} else {
					denied = append(denied, "SELECT * FROM auth.users", "SET ROLE auth_migrator")
				}
				if kind == "app" {
					denied = append(denied, "CREATE TABLE forbidden(id integer)", "SELECT * FROM goose_"+service+"_version", "SET ROLE "+service+"_migrator")
					if service == "auth" {
						denied = append(denied, "UPDATE users SET email=email", "DELETE FROM users", "TRUNCATE users")
					} else {
						denied = append(denied, "UPDATE transactions SET description=description", "DELETE FROM transactions", "TRUNCATE budgets")
					}
				}
				for _, query := range denied {
					tx, err := conn.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(ctx, query)
					_ = tx.Rollback(ctx)
					var pgerr *pgconn.PgError
					if !errors.As(err, &pgerr) || pgerr.Code != "42501" {
						t.Errorf("%s: expected insufficient_privilege, got %v", query, err)
					}
				}
				if kind == "migrator" {
					tx, err := conn.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(ctx, "CREATE TABLE migration_permission_probe(id integer)")
					_ = tx.Rollback(ctx)
					if err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}
