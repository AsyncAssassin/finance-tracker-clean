package grpcserver

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

func TestAggregateOverflowIsReportedAsInvalidInput(t *testing.T) {
	h := &handler{}
	err := fmt.Errorf("sum spent: %w", money.ErrOverflow)
	if got := status.Code(h.status(context.Background(), err)); got != codes.InvalidArgument {
		t.Fatalf("code=%v", got)
	}
	row := h.rowResult(context.Background(), 2, domain.Transaction{}, err)
	if row.GetError().GetCode() != "invalid_request" {
		t.Fatalf("row=%v", row)
	}
}
