package postgres_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
	"github.com/AsyncAssassin/finance-tracker-clean/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCategoryMigrationNormalizesOrRefusesInvalidNames(t *testing.T) {
	admin := connect(t)
	ctx := context.Background()
	// U+0958 expands to U+0915 U+093C in NFC.
	atLimit := strings.Repeat("a", domain.MaxCategoryLen-2) + "\u0958"
	tooLong := strings.Repeat("a", domain.MaxCategoryLen-1) + "\u0958"
	tests := []struct {
		name          string
		collision     bool
		extraCategory string
		wantError     string
	}{
		{name: "normalize"},
		{name: "collision", collision: true, wantError: "canonical category collision"},
		{name: "at length limit", extraCategory: atLimit},
		{name: "expands past length limit", extraCategory: tooLong, wantError: "invalid existing category"},
		{name: "30 marks", extraCategory: "x" + strings.Repeat("\u0301", 30)},
		{name: "31 marks", extraCategory: "x" + strings.Repeat("\u0301", 31), wantError: "invalid existing category"},
		{name: "precomposed at limit", extraCategory: "é" + strings.Repeat("\u0301", 29)},
		{name: "precomposed over limit", extraCategory: "é" + strings.Repeat("\u0301", 30), wantError: "invalid existing category"},
		{name: "other Unicode block", extraCategory: "x" + strings.Repeat("\u1ab0", 31), wantError: "invalid existing category"},
		{name: "explicit separator", extraCategory: "x" + strings.Repeat("\u0301", 30) + "\u034f\u0301"},
	}
	for _, table := range []string{"budgets", "transactions"} {
		for _, tc := range tests {
			t.Run(table+"/"+tc.name, func(t *testing.T) {
				// A real Goose provider exercises rollback and version tracking together.
				if _, err := admin.Exec(ctx, "CREATE SCHEMA nfc_migration_test"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := admin.Exec(ctx, "DROP SCHEMA nfc_migration_test CASCADE"); err != nil {
						t.Error(err)
					}
				})
				cfg := admin.Config().Copy()
				cfg.ConnConfig.RuntimeParams["search_path"] = "nfc_migration_test"
				p, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(p.Close)
				// Only the initial SQL schema; the application registers Go migration 2.
				init, err := fs.ReadFile(migrations.Ledger(), "00001_init.sql")
				if err != nil {
					t.Fatal(err)
				}
				if err := pgkit.Migrate(ctx, p, fstest.MapFS{"00001_init.sql": {Data: init}}, "goose_ledger_version"); err != nil {
					t.Fatal(err)
				}
				if _, err := p.Exec(ctx, "INSERT INTO budgets(user_id,category,limit_kopecks,period) VALUES(1,$1,100,'month')", "cafe\u0301"); err != nil {
					t.Fatal(err)
				}
				category := "cafe\u0301"
				if tc.collision {
					category = "café"
				}
				if _, err := p.Exec(ctx, "INSERT INTO transactions(user_id,category,amount_kopecks,occurred_on) VALUES(1,$1,50,'2026-10-03')", category); err != nil {
					t.Fatal(err)
				}
				if tc.extraCategory != "" {
					query := "INSERT INTO transactions(user_id,category,amount_kopecks,occurred_on) VALUES(2,$1,50,'2026-10-03')"
					if table == "budgets" {
						query = "INSERT INTO budgets(user_id,category,limit_kopecks,period) VALUES(2,$1,100,'month')"
					}
					if _, err := p.Exec(ctx, query, tc.extraCategory); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := func() string {
					var data string
					err := p.QueryRow(ctx, `SELECT jsonb_build_object(
'budgets',(SELECT jsonb_agg(to_jsonb(b) ORDER BY user_id,category) FROM budgets b),
'transactions',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM transactions t),
'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM goose_ledger_version v))::text`).Scan(&data)
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
				before := snapshot()
				err = postgres.Migrate(ctx, p)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("error=%v, want %s", err, tc.wantError)
					}
					if strings.Contains(err.Error(), category) || tc.extraCategory != "" && strings.Contains(err.Error(), tc.extraCategory) {
						t.Fatal("migration disclosed category")
					}
					if snapshot() != before {
						t.Fatal("failed migration changed rows or version")
					}
					// Operator resolves the fixture; the same migration can then retry.
					if _, err := p.Exec(ctx, "UPDATE transactions SET category='cafe' WHERE user_id=1; UPDATE budgets SET category='cafe' WHERE user_id=1"); err != nil {
						t.Fatal(err)
					}
					if _, err := p.Exec(ctx, "UPDATE "+table+" SET category='reviewed' WHERE user_id=2"); err != nil {
						t.Fatal(err)
					}
					if err := postgres.Migrate(ctx, p); err != nil {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				var version int
				if err := p.QueryRow(ctx, "SELECT max(version_id) FROM goose_ledger_version WHERE is_applied").Scan(&version); err != nil {
					t.Fatal(err)
				}
				if version != 2 {
					t.Fatalf("version=%d", version)
				}
				rows, err := p.Query(ctx, "SELECT category FROM budgets UNION ALL SELECT category FROM transactions")
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				for rows.Next() {
					var stored string
					if err := rows.Scan(&stored); err != nil {
						t.Fatal(err)
					}
					normalized, err := domain.NormalizeCategory(stored)
					if err != nil || normalized != stored {
						t.Fatalf("stored category differs from runtime key: %v", err)
					}
					if tc.extraCategory == atLimit && strings.HasPrefix(stored, "a") && utf8.RuneCountInString(stored) != 64 {
						t.Fatal("boundary length changed")
					}
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
