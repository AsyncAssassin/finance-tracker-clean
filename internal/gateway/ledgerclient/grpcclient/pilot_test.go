package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"strings"
	"testing"
)

func TestNFCBudgetCannotBeBypassed(t *testing.T) {
	for _, pair := range [][2]string{{"café", "cafe\u0301"}, {"\u0130", "I\u0307"}, {"x" + strings.Repeat("\u0301", 29) + "\u0327", "x\u0327" + strings.Repeat("\u0301", 29)}} {
		t.Run(pair[0], func(t *testing.T) {
			c := newLedger(t)
			ctx := context.Background()
			if _, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: pair[0], Limit: 100}); err != nil {
				t.Fatal(err)
			}
			tx, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Category: pair[1], Amount: 80})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Category: pair[0], Amount: 21})
			var exceeded *ledgerclient.BudgetExceededError
			if !errors.As(err, &exceeded) {
				t.Fatalf("equivalent category bypassed budget: %v", err)
			}
			// Updating either spelling must replace the same budget.
			if _, created, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: pair[1], Limit: 90}); err != nil || created {
				t.Fatalf("equivalent budget was not updated: created=%v, error=%v", created, err)
			}
			for _, spelling := range pair {
				list, err := c.ListTransactions(ctx, user, ledgerclient.TransactionFilter{Category: spelling})
				if err != nil || len(list) != 1 || list[0].ID != tx.ID {
					t.Fatalf("filter %q missed the transaction: %v, %v", spelling, list, err)
				}
			}
		})
	}
}
func TestDuplicateImportReturnsEffectiveKey(t *testing.T) {
	c := newLedger(t)
	ctx := context.Background()
	tx, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Category: "food", Amount: 100, ExternalID: "bank-key"})
	if err != nil {
		t.Fatal(err)
	}
	row := ledgerclient.ImportRow{Line: 2, SourceID: tx.ID, Input: ledgerclient.TransactionInput{Category: "food", Amount: 100}}
	results, err := c.ImportTransactions(ctx, user, []ledgerclient.ImportRow{row})
	if err != nil {
		t.Fatal(err)
	}
	var duplicate *ledgerclient.DuplicateError
	if !errors.As(results[0].Err, &duplicate) || duplicate.ExternalID != "" || duplicate.ExistingID != tx.ID {
		t.Fatalf("owned source: %+v", results)
	}
	// A foreign source ID receives the server's derived key.
	results, err = c.ImportTransactions(ctx, user+1, []ledgerclient.ImportRow{row, row})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil || !errors.As(results[1].Err, &duplicate) || duplicate.ExternalID != fmt.Sprintf("export-%d", tx.ID) || duplicate.ExistingID != results[0].ID {
		t.Fatalf("derived key: %+v", results)
	}
}
func TestCategoryQuotaAcrossGRPCAndImport(t *testing.T) {
	c := newLedger(t)
	ctx := context.Background()
	for i := range 100 {
		if _, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: fmt.Sprintf("category-%d", i), Limit: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: "new", Limit: 1000}); !errors.Is(err, upstream.ErrQuotaExceeded) {
		t.Fatalf("quota translation: %v", err)
	}
	results, err := c.ImportTransactions(ctx, user, []ledgerclient.ImportRow{
		{Line: 2, Input: ledgerclient.TransactionInput{Category: "new", Amount: 100}},
		{Line: 3, Input: ledgerclient.TransactionInput{Category: "category-0", Amount: 100}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rowError *ledgerclient.RowError
	if !errors.As(results[0].Err, &rowError) || rowError.Code != "quota_exceeded" || results[1].Err != nil {
		t.Fatalf("quota stopped valid rows: %+v", results)
	}
}

func TestLongCombiningCategoriesAreRejectedAcrossGRPC(t *testing.T) {
	c := newLedger(t)
	ctx := context.Background()
	bad := []string{"x" + strings.Repeat("\u0301", 31) + "\u0327", "x\u0327" + strings.Repeat("\u0301", 31), "é" + strings.Repeat("\u0301", 30)}
	requireCategoryError := func(err error) {
		t.Helper()
		var v *upstream.ValidationError
		if !errors.As(err, &v) || v.Fields["category"] == "" {
			t.Fatalf("expected category validation, got %v", err)
		}
	}
	for _, category := range bad {
		_, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: category, Limit: 100})
		requireCategoryError(err)
		_, err = c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Category: category, Amount: 200})
		requireCategoryError(err)
		_, err = c.ListTransactions(ctx, user, ledgerclient.TransactionFilter{Category: category})
		requireCategoryError(err)
		_, err = c.ExportTransactionsCSV(ctx, user, ledgerclient.TransactionFilter{Category: category})
		requireCategoryError(err)
	}
	rows := []ledgerclient.ImportRow{
		{Line: 2, Input: ledgerclient.TransactionInput{Category: bad[0], Amount: 200}},
		{Line: 3, Input: ledgerclient.TransactionInput{Category: "café", Amount: 80}},
		{Line: 4, Input: ledgerclient.TransactionInput{Category: bad[1], Amount: 200}},
		{Line: 5, Input: ledgerclient.TransactionInput{Category: "cafe\u0301", Amount: 21}},
	}
	if _, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: "café", Limit: 100}); err != nil {
		t.Fatal(err)
	}
	results, err := c.ImportTransactions(ctx, user, rows)
	if err != nil || len(results) != 4 {
		t.Fatalf("import: %v", err)
	}
	requireCategoryError(results[0].Err)
	requireCategoryError(results[2].Err)
	if results[1].Err != nil {
		t.Fatal(results[1].Err)
	}
	var exceeded *ledgerclient.BudgetExceededError
	if !errors.As(results[3].Err, &exceeded) {
		t.Fatalf("valid rows lost their order: %v", results[3].Err)
	}
	list, err := c.ListTransactions(ctx, user, ledgerclient.TransactionFilter{})
	if err != nil || len(list) != 1 {
		t.Fatalf("invalid rows were written: %v, count=%d", err, len(list))
	}
}
