// Package repotest checks that a Repository implementation honours the
// contract the use cases rely on. Every implementation runs the same suite:
// the in-memory store in its unit tests and PostgreSQL in the integration
// tests, so the two cannot drift apart.
package repotest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Factory returns an empty repository for one subtest.
type Factory func(t *testing.T) usecase.Repository

const user int64 = 1

var (
	sep30 = civil.New(2026, time.September, 30)
	oct2  = civil.New(2026, time.October, 2)
	oct3  = civil.New(2026, time.October, 3)
)

// Run executes the contract suite. Subtests run one after another because
// a database-backed factory hands out the same database each time.
func Run(t *testing.T, newRepo Factory) {
	t.Run("CategoryQuota", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()
		for i := range domain.MaxCategories - 1 {
			category := fmt.Sprintf("category-%d", i)
			if i%2 == 0 {
				insert(t, r, domain.Transaction{UserID: user, Amount: 100, Category: category, Date: oct3})
			} else {
				if _, err := r.UpsertBudget(ctx, domain.Budget{UserID: user, Category: category, Limit: 1000, Period: domain.PeriodMonth}); err != nil {
					t.Fatal(err)
				}
			}
		}
		results := make(chan error, 8)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() {
				category := fmt.Sprintf("new-%d", i)
				if i%2 == 0 {
					results <- r.InTx(ctx, func(tx usecase.Tx) error {
						_, err := tx.InsertTransaction(ctx, domain.Transaction{UserID: user, Amount: 100, Category: category, Date: oct3})
						return err
					})
				} else {
					_, err := r.UpsertBudget(ctx, domain.Budget{UserID: user, Category: category, Limit: 1000, Period: domain.PeriodMonth})
					results <- err
				}
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
			t.Fatalf("created %d categories at quota boundary", created)
		}
		if _, err := r.UpsertBudget(ctx, domain.Budget{UserID: user, Category: "category-0", Limit: 2000, Period: domain.PeriodMonth}); err != nil {
			t.Fatalf("budget for existing category: %v", err)
		}
		insert(t, r, domain.Transaction{UserID: user, Amount: 100, Category: "category-0", Date: oct3})
		insert(t, r, domain.Transaction{UserID: user + 1, Amount: 100, Category: "independent", Date: oct3})
	})

	t.Run("InTxRollsBackOnError", func(t *testing.T) { testRollback(t, newRepo(t)) })
	t.Run("InTxCanceledContext", func(t *testing.T) { testCanceled(t, newRepo(t)) })
	t.Run("InsertAndListNewestFirst", func(t *testing.T) { testInsertAndList(t, newRepo(t)) })
	t.Run("ListFilters", func(t *testing.T) { testFilters(t, newRepo(t)) })
	t.Run("BudgetUpsertAndLock", func(t *testing.T) { testBudgets(t, newRepo(t)) })
	t.Run("SpentSums", func(t *testing.T) { testSpent(t, newRepo(t)) })
	t.Run("ExternalIDUniquePerUser", func(t *testing.T) { testExternalID(t, newRepo(t)) })
	t.Run("ConcurrentBudgetCheck", func(t *testing.T) { testConcurrent(t, newRepo(t)) })
}

func insert(t *testing.T, r usecase.Repository, tx domain.Transaction) domain.Transaction {
	t.Helper()
	var created domain.Transaction
	err := r.InTx(context.Background(), func(x usecase.Tx) error {
		var err error
		created, err = x.InsertTransaction(context.Background(), tx)
		return err
	})
	if err != nil {
		t.Fatalf("insert %+v: %v", tx, err)
	}
	return created
}

func testRollback(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	boom := errors.New("boom")
	err := r.InTx(ctx, func(x usecase.Tx) error {
		if _, err := x.InsertTransaction(ctx, domain.Transaction{UserID: user, Amount: 100, Category: "еда", Date: oct3}); err != nil {
			return err
		}
		spent, err := x.SpentInRange(ctx, user, "еда", civil.Date{}, civil.Date{})
		if err != nil || spent != 100 {
			t.Errorf("the pending insert is not visible inside the transaction: %d, %v", spent, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx error = %v, want boom unchanged", err)
	}
	list, err := r.ListTransactions(ctx, user, usecase.TransactionFilter{Limit: 10})
	if err != nil || len(list) != 0 {
		t.Errorf("rolled back transaction is visible: %+v, %v", list, err)
	}
}

func testCanceled(t *testing.T, r usecase.Repository) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.InTx(ctx, func(usecase.Tx) error {
		t.Error("fn must not run with a canceled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func testInsertAndList(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	first := insert(t, r, domain.Transaction{UserID: user, Amount: 125050, Category: "еда", Description: "продукты", Date: oct2, ExternalID: "row-1"})
	second := insert(t, r, domain.Transaction{UserID: user, Amount: 200, Category: "транспорт", Date: oct3})
	third := insert(t, r, domain.Transaction{UserID: user, Amount: 300, Category: "еда", Date: oct3})
	insert(t, r, domain.Transaction{UserID: user + 1, Amount: 400, Category: "еда", Date: oct3})

	if first.ID == 0 || second.ID <= first.ID || third.ID <= second.ID {
		t.Errorf("ids are not increasing: %d, %d, %d", first.ID, second.ID, third.ID)
	}
	if first.CreatedAt.IsZero() {
		t.Error("CreatedAt is not set")
	}
	list, err := r.ListTransactions(ctx, user, usecase.TransactionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].ID != third.ID || list[1].ID != second.ID || list[2].ID != first.ID {
		t.Fatalf("list order = %v, want newest first by date then id", ids(list))
	}
	got := list[2]
	if got.Amount != 125050 || got.Category != "еда" || got.Description != "продукты" || !got.Date.Equal(oct2) || got.ExternalID != "row-1" || got.UserID != user {
		t.Errorf("stored transaction = %+v", got)
	}
	if list[0].ExternalID != "" {
		t.Errorf("empty external id came back as %q", list[0].ExternalID)
	}
}

func testFilters(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	a := insert(t, r, domain.Transaction{UserID: user, Amount: 100, Category: "еда", Date: sep30})
	b := insert(t, r, domain.Transaction{UserID: user, Amount: 200, Category: "еда", Date: oct3})
	c := insert(t, r, domain.Transaction{UserID: user, Amount: 300, Category: "транспорт", Date: oct3})
	d := insert(t, r, domain.Transaction{UserID: user, Amount: 400, Category: "еда", Date: oct2})

	tests := []struct {
		name   string
		filter usecase.TransactionFilter
		want   []int64
	}{
		{"category", usecase.TransactionFilter{Category: "еда", Limit: 10}, []int64{b.ID, d.ID, a.ID}},
		{"from and to inclusive", usecase.TransactionFilter{From: oct2, To: oct3, Limit: 10}, []int64{c.ID, b.ID, d.ID}},
		{"only from", usecase.TransactionFilter{From: oct3, Limit: 10}, []int64{c.ID, b.ID}},
		{"only to", usecase.TransactionFilter{To: sep30, Limit: 10}, []int64{a.ID}},
		{"limit and offset", usecase.TransactionFilter{Limit: 2, Offset: 1}, []int64{b.ID, d.ID}},
		{"zero limit means no limit", usecase.TransactionFilter{}, []int64{c.ID, b.ID, d.ID, a.ID}},
		{"offset past the end", usecase.TransactionFilter{Limit: 10, Offset: 10}, []int64{}},
		{"other user", usecase.TransactionFilter{Limit: 10}, nil},
	}
	for _, tc := range tests {
		who := user
		if tc.name == "other user" {
			who = user + 1
			tc.want = []int64{}
		}
		list, err := r.ListTransactions(ctx, who, tc.filter)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := ids(list); !equal(got, tc.want) {
			t.Errorf("%s: ids = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func testBudgets(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	created, err := r.UpsertBudget(ctx, domain.Budget{UserID: user, Category: "еда", Limit: 500000, Period: domain.PeriodMonth})
	if err != nil || !created {
		t.Fatalf("first upsert = created %v, %v", created, err)
	}
	created, err = r.UpsertBudget(ctx, domain.Budget{UserID: user, Category: "еда", Limit: 100000, Period: domain.PeriodNone})
	if err != nil || created {
		t.Fatalf("second upsert = created %v, %v", created, err)
	}
	if _, err := r.UpsertBudget(ctx, domain.Budget{UserID: user, Category: "авто", Limit: 1, Period: domain.PeriodYear}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpsertBudget(ctx, domain.Budget{UserID: user + 1, Category: "еда", Limit: 7, Period: domain.PeriodMonth}); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListBudgets(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Budget{
		{UserID: user, Category: "авто", Limit: 1, Period: domain.PeriodYear},
		{UserID: user, Category: "еда", Limit: 100000, Period: domain.PeriodNone},
	}
	if len(list) != 2 || list[0] != want[0] || list[1] != want[1] {
		t.Errorf("ListBudgets = %+v, want %+v", list, want)
	}

	err = r.InTx(ctx, func(x usecase.Tx) error {
		b, err := x.BudgetForUpdate(ctx, user, "еда")
		if err != nil || b == nil || *b != want[1] {
			t.Errorf("BudgetForUpdate = %+v, %v, want %+v", b, err, want[1])
		}
		if b != nil {
			b.Limit = 1 // must not leak into the store
		}
		none, err := x.BudgetForUpdate(ctx, user, "нет")
		if err != nil || none != nil {
			t.Errorf("missing budget = %+v, %v, want nil, nil", none, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	list, _ = r.ListBudgets(ctx, user)
	if len(list) != 2 || list[1].Limit != 100000 {
		t.Errorf("budget changed through the pointer: %+v", list)
	}
}

func testSpent(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	insert(t, r, domain.Transaction{UserID: user, Amount: 100, Category: "еда", Date: sep30})
	insert(t, r, domain.Transaction{UserID: user, Amount: 200, Category: "еда", Date: oct2})
	insert(t, r, domain.Transaction{UserID: user, Amount: 300, Category: "еда", Date: oct3})
	insert(t, r, domain.Transaction{UserID: user, Amount: 400, Category: "транспорт", Date: oct3})
	insert(t, r, domain.Transaction{UserID: user + 1, Amount: 500, Category: "еда", Date: oct3})

	by, err := r.SpentByCategory(ctx, user, oct2, oct3)
	if err != nil {
		t.Fatal(err)
	}
	if len(by) != 2 || by["еда"] != 500 || by["транспорт"] != 400 {
		t.Errorf("SpentByCategory = %v", by)
	}
	by, _ = r.SpentByCategory(ctx, user+2, oct2, oct3)
	if len(by) != 0 {
		t.Errorf("SpentByCategory for a user without transactions = %v", by)
	}
	err = r.InTx(ctx, func(x usecase.Tx) error {
		for _, tc := range []struct {
			from, to civil.Date
			want     money.Money
		}{
			{oct2, oct3, 500},
			{domain.MinDate, domain.MaxDate, 600},
			{civil.Date{}, civil.Date{}, 600},
			{oct3, oct3, 300},
			{civil.New(2026, time.November, 1), civil.New(2026, time.November, 30), 0},
		} {
			got, err := x.SpentInRange(ctx, user, "еда", tc.from, tc.to)
			if err != nil || got != tc.want {
				t.Errorf("SpentInRange(%s, %s) = %d, %v, want %d", tc.from, tc.to, got, err, tc.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func testExternalID(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	first := insert(t, r, domain.Transaction{UserID: user, Amount: 100, Category: "еда", Date: oct3, ExternalID: "row-1"})
	err := r.InTx(ctx, func(x usecase.Tx) error {
		_, err := x.InsertTransaction(ctx, domain.Transaction{UserID: user, Amount: 200, Category: "еда", Date: oct3, ExternalID: "row-1"})
		return err
	})
	var dup *domain.DuplicateError
	if !errors.As(err, &dup) || dup.ExistingID != first.ID || dup.ExternalID != "row-1" {
		t.Fatalf("duplicate error = %v, want DuplicateError pointing at #%d", err, first.ID)
	}
	insert(t, r, domain.Transaction{UserID: user, Amount: 300, Category: "еда", Date: oct3, ExternalID: "row-2"})
	insert(t, r, domain.Transaction{UserID: user + 1, Amount: 400, Category: "еда", Date: oct3, ExternalID: "row-1"})
	insert(t, r, domain.Transaction{UserID: user, Amount: 500, Category: "еда", Date: oct3})
	insert(t, r, domain.Transaction{UserID: user, Amount: 600, Category: "еда", Date: oct3})
	list, _ := r.ListTransactions(ctx, user, usecase.TransactionFilter{Limit: 10})
	if len(list) != 4 {
		t.Errorf("stored %d transactions for the user, want 4", len(list))
	}

	// Lookup by external ID, including a row inserted in the same
	// transaction and the IDs of other users.
	err = r.InTx(ctx, func(x usecase.Tx) error {
		pending, err := x.InsertTransaction(ctx, domain.Transaction{UserID: user, Amount: 1, Category: "еда", Date: oct3, ExternalID: "row-3"})
		if err != nil {
			return err
		}
		for _, tc := range []struct {
			user int64
			id   string
			want int64
		}{{user, "row-1", first.ID}, {user, "row-3", pending.ID}, {user, "missing", 0}, {user + 2, "row-1", 0}} {
			got, err := x.TransactionIDByExternalID(ctx, tc.user, tc.id)
			if err != nil || got != tc.want {
				t.Errorf("TransactionIDByExternalID(%d, %q) = %d, %v; want %d", tc.user, tc.id, got, err, tc.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		user, id int64
		want     bool
	}{{user, first.ID, true}, {user + 1, first.ID, false}, {user, first.ID + 1000, false}} {
		got, err := r.HasTransaction(ctx, tc.user, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("HasTransaction(%d, %d) = %v, %v; want %v", tc.user, tc.id, got, err, tc.want)
		}
	}
}

// testConcurrent drives the real use case against the repository: with a
// limit of 1000 and 50 parallel transactions of 100, exactly 10 may pass.
func testConcurrent(t *testing.T, r usecase.Repository) {
	ctx := context.Background()
	svc := usecase.New(r)
	if _, _, err := svc.UpsertBudget(ctx, user, domain.BudgetInput{Category: "еда", Limit: 1000, Period: "none"}); err != nil {
		t.Fatal(err)
	}
	const workers = 50
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.CreateTransaction(ctx, user, domain.TransactionInput{Amount: 100, Category: "еда", Date: oct3})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var accepted, rejected int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, domain.ErrBudgetExceeded):
			rejected++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if accepted != 10 || rejected != workers-10 {
		t.Errorf("accepted %d, rejected %d; want 10 and %d", accepted, rejected, workers-10)
	}
	by, _ := r.SpentByCategory(ctx, user, oct3, oct3)
	if by["еда"] != 1000 {
		t.Errorf("spent = %d, want exactly the limit", by["еда"])
	}
}

func ids(list []domain.Transaction) []int64 {
	out := make([]int64, len(list))
	for i, tx := range list {
		out[i] = tx.ID
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
