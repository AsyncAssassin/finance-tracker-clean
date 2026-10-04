// Command migrate applies one service's schema with its migration credentials.
// Runtime services never run DDL or receive the migration password.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	auth "github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/postgres"
	ledger "github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/config"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
)

func main() {
	service := flag.String("service", "", "schema to migrate: auth or ledger")
	flag.Parse()
	if err := run(*service); err != nil {
		fmt.Fprintln(os.Stderr, "migration failed:", err)
		os.Exit(1)
	}
}

func run(service string) error {
	migrate := auth.Migrate
	grants := `GRANT SELECT, INSERT ON users TO auth_app;
 GRANT USAGE ON SEQUENCE users_id_seq TO auth_app;`
	switch service {
	case "auth":
	case "ledger":
		migrate = ledger.Migrate
		grants = `GRANT SELECT, INSERT, UPDATE ON budgets TO ledger_app;
  GRANT SELECT, INSERT ON transactions TO ledger_app;
  GRANT USAGE ON SEQUENCE transactions_id_seq TO ledger_app;`
	default:
		return fmt.Errorf("service must be auth or ledger")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	wait, err := config.Duration("DB_CONNECT_WAIT", 30*time.Second)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgkit.Connect(ctx, url, wait, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Refuse an administrator or the other service's credentials.
	var role, schema string
	if err := pool.QueryRow(ctx, "SELECT current_user, current_schema()").Scan(&role, &schema); err != nil {
		return err
	}
	if role != service+"_migrator" || schema != service {
		return fmt.Errorf("expected the %s migration role and schema", service)
	}
	if err := migrate(ctx, pool); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, grants); err != nil {
		return fmt.Errorf("grant runtime access: %w", err)
	}
	fmt.Println(service + " migrations complete")
	return nil
}
