package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
)

// normalizeCategories is migration 2. It shares the API's Unicode rules;
// PostgreSQL NFC alone does not implement the same accepted input contract.
// Down is intentionally a no-op: old spellings require the pre-migration backup.
func normalizeCategories(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "LOCK TABLE budgets, transactions IN ACCESS EXCLUSIVE MODE"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT user_id, category FROM budgets UNION SELECT user_id, category FROM transactions")
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct {
		userID   int64
		category string
	}
	type change struct {
		userID        int64
		before, after string
	}
	seen := make(map[key]bool)
	var changes []change
	for rows.Next() {
		var userID int64
		var category string
		if err := rows.Scan(&userID, &category); err != nil {
			return err
		}
		normalized, err := domain.NormalizeCategory(category)
		if err != nil {
			// Never include the category or user ID in migration diagnostics.
			return errors.New("invalid existing category: review category rules before migration")
		}
		k := key{userID, normalized}
		if seen[k] {
			return errors.New("canonical category collision: review existing spellings and budgets before migration")
		}
		seen[k] = true
		if category != normalized {
			changes = append(changes, change{userID, category, normalized})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// All categories have been validated before changing any row. Goose commits
	// these updates and its version together, or rolls the entire migration back.
	for _, c := range changes {
		for _, table := range []string{"budgets", "transactions"} {
			if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET category=$1 WHERE user_id=$2 AND category=$3", c.after, c.userID, c.before); err != nil {
				return err
			}
		}
	}
	return nil
}
