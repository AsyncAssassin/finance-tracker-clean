package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"sync"
	"testing"
)

func TestTransactionQuotaAcrossConcurrentWriters(t *testing.T) {
	r := freshRepo(t)
	ctx := context.Background()
	p := connect(t)
	_, err := p.Exec(ctx, "INSERT INTO transactions(user_id,amount_kopecks,category,occurred_on,external_id) SELECT 1,100,'food','2026-10-03','seed-'||g FROM generate_series(1,$1) g", domain.MaxTransactions-1)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			results <- r.InTx(ctx, func(tx usecase.Tx) error {
				_, err := tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Amount: 100, Category: "food", Date: domain.MinDate, ExternalID: fmt.Sprintf("last-%d", i)})
				return err
			})
		})
	}
	wg.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created=%d", created)
	}
	err = r.InTx(ctx, func(tx usecase.Tx) error {
		_, err := tx.InsertTransaction(ctx, domain.Transaction{UserID: 1, Amount: 100, Category: "other", Date: domain.MinDate, ExternalID: "seed-1"})
		return err
	})
	var duplicate *domain.DuplicateError
	if !errors.As(err, &duplicate) {
		t.Fatalf("retry at quota: %v", err)
	}
	var count int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM transactions WHERE user_id=1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != domain.MaxTransactions {
		t.Fatalf("rows=%d", count)
	}
	err = r.InTx(ctx, func(tx usecase.Tx) error {
		_, err := tx.InsertTransaction(ctx, domain.Transaction{UserID: 2, Amount: 100, Category: "other", Date: domain.MinDate})
		return err
	})
	if err != nil {
		t.Fatalf("another user's quota: %v", err)
	}
}
