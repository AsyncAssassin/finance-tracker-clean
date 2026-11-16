package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
)

type budgetReads struct {
	usecase.Repository
	reads int
	err   error
}

func (r *budgetReads) ListBudgets(ctx context.Context, id int64) ([]domain.Budget, error) {
	r.reads++
	if r.err != nil {
		return nil, r.err
	}
	return r.Repository.ListBudgets(ctx, id)
}

func TestBudgetCacheAvoidsDatabaseAndExpires(t *testing.T) {
	cache, mr := redisCache(t)
	repo := &budgetReads{Repository: memory.New()}
	logs := &logBuffer{}
	s := usecase.New(repo, usecase.WithCache(cache, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	ctx := context.Background()
	for range 2 {
		list, err := s.ListBudgets(ctx, 1)
		if err != nil || list == nil || len(list) != 0 {
			t.Fatalf("empty list: %#v %v", list, err)
		}
	}
	if repo.reads != 1 {
		t.Fatalf("empty cache made %d database reads", repo.reads)
	}
	want, _, err := s.UpsertBudget(ctx, 1, domain.BudgetInput{Category: "еда", Limit: 12345, Period: "month"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := s.ListBudgets(ctx, 1)
		if err != nil || !reflect.DeepEqual(got, []domain.Budget{want}) {
			t.Fatalf("budgets: %+v %v", got, err)
		}
		got[0].Limit = 1 // a caller cannot mutate the next cached result
	}
	if repo.reads != 2 {
		t.Fatalf("cache made %d database reads", repo.reads)
	}
	other, err := s.ListBudgets(ctx, 2)
	if err != nil || len(other) != 0 {
		t.Fatalf("another account: %+v %v", other, err)
	}
	if repo.reads != 3 {
		t.Fatal("another account reused the first account's cache")
	}
	want, _, err = s.UpsertBudget(ctx, 1, domain.BudgetInput{Category: "еда", Limit: 23456, Period: "year"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ListBudgets(ctx, 1)
	if err != nil || !reflect.DeepEqual(got, []domain.Budget{want}) {
		t.Fatalf("stale budget after update: %+v %v", got, err)
	}
	mr.FastForward(21 * time.Second)
	if _, err := s.ListBudgets(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if repo.reads != 5 {
		t.Fatalf("expired cache made %d reads, want 5", repo.reads)
	}
	for _, marker := range []string{"budget cache hit", "budget cache miss"} {
		if !strings.Contains(logs.String(), marker) {
			t.Fatalf("missing %s log", marker)
		}
	}
	for _, marker := range []string{"user_id=", "еда", "12345", "23456"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("budget diagnostics expose data")
		}
	}
}

func TestBudgetCacheFailureModes(t *testing.T) {
	for _, kind := range []string{"read", "write", "invalidate", "broken", "canceled", "database"} {
		t.Run(kind, func(t *testing.T) {
			cache := &flakyCache{}
			repo := &budgetReads{Repository: memory.New()}
			s := usecase.New(repo, usecase.WithCache(cache, slog.New(slog.DiscardHandler)))
			ctx := context.Background()
			switch kind {
			case "read":
				cache.getErr = errors.New("connection refused")
			case "write":
				cache.setErr = errors.New("unavailable")
			case "invalidate":
				cache.invalidateErr = errors.New("unavailable")
			case "broken":
				cache.getErr = fmt.Errorf("%w: bad data", usecase.ErrBrokenEntry)
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				cache.getErr = context.Canceled
				_, _ = s.ListBudgets(canceled, 1)
				cache.getErr = nil
			case "database":
				repo.err = errors.New("database unavailable")
			}
			if kind == "invalidate" {
				if _, _, err := s.UpsertBudget(ctx, 1, domain.BudgetInput{Category: "food", Limit: 10}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				_, err := s.ListBudgets(ctx, 1)
				if kind == "database" {
					if !errors.Is(err, repo.err) {
						t.Fatalf("database error lost: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "read", "write":
				if cache.gets.Load() != 1 {
					t.Fatal("failing cache was not paused")
				}
			case "invalidate":
				if cache.gets.Load() != 0 {
					t.Fatal("stale cache read after failed invalidation")
				}
			case "broken":
				if cache.gets.Load() != 2 || cache.sets.Load() != 2 {
					t.Fatal("broken entry was not rebuilt")
				}
			case "canceled":
				if cache.gets.Load() != 3 {
					t.Fatal("canceled caller paused everyone's cache")
				}
			case "database":
				if cache.sets.Load() != 0 {
					t.Fatal("database error was cached")
				}
			}
		})
	}
}
