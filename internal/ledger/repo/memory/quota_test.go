package memory

import (
	"context"
	"errors"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"testing"
)

func TestTransactionQuotaIncludesPendingAndAllowsRetry(t *testing.T) {
	r := New()
	ctx := context.Background()
	for i := range domain.MaxTransactions - 1 {
		r.txs = append(r.txs, domain.Transaction{ID: int64(i + 1), UserID: 1, Category: "food", Date: domain.MinDate})
	}
	r.nextID = int64(len(r.txs))
	err := r.InTx(ctx, func(tx usecase.Tx) error {
		_, err := tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Amount: 100, Category: "food", Date: domain.MinDate, ExternalID: "last"})
		if err != nil {
			return err
		}
		_, err = tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Amount: 100, Category: "food", Date: domain.MinDate})
		if !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("pending quota: %v", err)
		}
		_, err = tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Amount: 100, Category: "other", Date: domain.MinDate, ExternalID: "last"})
		var duplicate *domain.DuplicateError
		if !errors.As(err, &duplicate) {
			t.Fatalf("retry at quota: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.txs) != domain.MaxTransactions {
		t.Fatalf("rows=%d", len(r.txs))
	}
}
