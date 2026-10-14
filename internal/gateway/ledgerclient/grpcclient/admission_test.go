package grpcclient

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

type observedRepository struct {
	usecase.Repository
	reached chan struct{}
}

func (r *observedRepository) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	err := r.Repository.InTx(ctx, fn)
	r.reached <- struct{}{}
	return err
}
func (r *observedRepository) UpsertBudget(ctx context.Context, b domain.Budget) (bool, error) {
	created, err := r.Repository.UpsertBudget(ctx, b)
	r.reached <- struct{}{}
	return created, err
}
func (r *observedRepository) ListBudgets(ctx context.Context, id int64) ([]domain.Budget, error) {
	budgets, err := r.Repository.ListBudgets(ctx, id)
	r.reached <- struct{}{}
	return budgets, err
}

type delayedInvalidation struct {
	release       chan struct{}
	started       chan struct{}
	once          sync.Once
	invalidations atomic.Int32
}

func (*delayedInvalidation) Get(context.Context, int64, civil.Date, civil.Date) (domain.Summary, string, bool, error) {
	return domain.Summary{}, "key", false, nil
}
func (*delayedInvalidation) Set(context.Context, string, domain.Summary) error { return nil }
func (*delayedInvalidation) TTL() time.Duration                                { return time.Minute }
func (c *delayedInvalidation) Invalidate(context.Context, int64) error {
	// Hold the first call deterministically; no external Redis or timer is needed.
	c.once.Do(func() { close(c.started) })
	<-c.release
	c.invalidations.Add(1)
	return nil
}

func TestCanceledCallsRemainBoundedUntilLedgerFinishes(t *testing.T) {
	repo := &observedRepository{Repository: memory.New(), reached: make(chan struct{}, 64)}
	cache := &delayedInvalidation{release: make(chan struct{}), started: make(chan struct{})}
	log := slog.New(slog.DiscardHandler)
	srv := grpcserver.New(usecase.New(repo, usecase.WithReportCache(cache, log)), log)
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	var once sync.Once
	release := func() { once.Do(func() { close(cache.release) }) }
	t.Cleanup(func() { release(); srv.Shutdown(context.Background()) })
	c := dial(t, ln, 5*time.Second)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	date := civil.New(2026, time.October, 14)
	for i := range 32 {
		callCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			var err error
			switch i % 4 {
			case 0:
				_, err = c.CreateTransaction(callCtx, user, ledgerclient.TransactionInput{Category: "food", Amount: 1, Date: date})
			case 1:
				_, _, err = c.UpsertBudget(callCtx, user, ledgerclient.BudgetInput{Category: "food", Limit: 10000})
			case 2:
				_, err = c.ImportTransactions(callCtx, user, []ledgerclient.ImportRow{{Line: 2, Input: ledgerclient.TransactionInput{Category: "food", Amount: 1, Date: date}}})
			case 3:
				_, err = c.Summary(callCtx, user, date, date)
			}
			done <- err
		}()
		select {
		case <-repo.reached:
		case <-ctx.Done():
			cancel()
			t.Fatal("operation did not reach storage")
		}
		if i == 0 {
			select {
			case <-cache.started:
			case <-ctx.Done():
				cancel()
				t.Fatal("invalidation did not start")
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled call: %v", err)
		}
	}
	for range 8 {
		_, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: "food", Limit: 20000})
		if !errors.Is(err, upstream.ErrBusy) {
			t.Fatalf("overload did not reject before storage: %v", err)
		}
	}
	if len(repo.reached) != 0 {
		t.Fatal("rejected work reached storage")
	}
	if _, err := grpc_health_v1.NewHealthClient(c.conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	release()
	for cache.invalidations.Load() != 24 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if cache.invalidations.Load() != 24 {
		t.Fatal("committed writes did not complete invalidation")
	}
	if _, err := c.ListBudgets(ctx, user); err != nil {
		t.Fatalf("admission did not recover: %v", err)
	}
}
