package memory

import (
	"context"
	"errors"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
	"math"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/repotest"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
)

func TestContract(t *testing.T) {
	repotest.Run(t, func(*testing.T) usecase.Repository { return New() })
}

func TestAggregateOverflow(t *testing.T) {
	r := New()
	for i := int64(0); i < math.MaxInt64/int64(money.Max)+1; i++ {
		r.txs = append(r.txs, domain.Transaction{UserID: 1, Category: "large", Amount: money.Max, Date: domain.MinDate})
	}
	if _, err := r.SpentByCategory(context.Background(), 1, domain.MinDate, domain.MinDate); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("category aggregate: %v", err)
	}
	err := r.InTx(context.Background(), func(tx usecase.Tx) error {
		_, err := tx.SpentInRange(context.Background(), 1, "large", domain.MinDate, domain.MinDate)
		return err
	})
	if !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("budget aggregate: %v", err)
	}
}
