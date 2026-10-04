package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

const user int64 = 1

var (
	today     = civil.New(2026, time.October, 3)
	lastMonth = civil.New(2026, time.September, 30)
	oct1      = civil.New(2026, time.October, 1)
	oct31     = civil.New(2026, time.October, 31)
)

func newService(t *testing.T) *usecase.Service {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC) }
	return usecase.New(memory.New(), usecase.WithClock(clock))
}

func setBudget(t *testing.T, s *usecase.Service, category string, limit money.Money, period string) {
	t.Helper()
	if _, _, err := s.UpsertBudget(context.Background(), user, domain.BudgetInput{Category: category, Limit: limit, Period: period}); err != nil {
		t.Fatalf("UpsertBudget(%s): %v", category, err)
	}
}

func create(s *usecase.Service, amount money.Money, category string, date civil.Date) (domain.Transaction, error) {
	return s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: amount, Category: category, Date: date})
}

func TestCreateTransactionWithoutBudget(t *testing.T) {
	t.Parallel()
	s := newService(t)
	tx, err := s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: 125050, Category: " Еда ", Description: " продукты "})
	if err != nil {
		t.Fatal(err)
	}
	if tx.ID != 1 || tx.UserID != user || tx.Category != "еда" || tx.Description != "продукты" || !tx.Date.Equal(today) || tx.CreatedAt.IsZero() {
		t.Errorf("created transaction = %+v", tx)
	}
	list, err := s.ListTransactions(context.Background(), user, usecase.TransactionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != tx {
		t.Errorf("ListTransactions = %+v, want the created transaction", list)
	}
}

func TestCreateTransactionChecksTheBudget(t *testing.T) {
	t.Parallel()
	s := newService(t)
	setBudget(t, s, "Еда", 500000, "")

	steps := []struct {
		name          string
		amount        money.Money
		date          civil.Date
		wantRemaining money.Money // when rejected
		wantErr       bool
	}{
		{name: "first purchase", amount: 125050, date: today},
		{name: "second purchase", amount: 350000, date: today},
		{name: "over the limit", amount: 50000, date: today, wantErr: true, wantRemaining: 24950},
		{name: "exactly up to the limit", amount: 24950, date: today},
		{name: "even one kopeck more", amount: 1, date: today, wantErr: true, wantRemaining: 0},
		{name: "last month has its own limit", amount: 50000, date: lastMonth},
	}
	for _, st := range steps {
		_, err := create(s, st.amount, "еда", st.date)
		if !st.wantErr {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", st.name, err)
			}
			continue
		}
		var exceeded *domain.BudgetExceededError
		if !errors.As(err, &exceeded) {
			t.Fatalf("%s: error = %v, want *BudgetExceededError", st.name, err)
		}
		if exceeded.Remaining() != st.wantRemaining || exceeded.PeriodLabel != "2026-10" {
			t.Errorf("%s: error = %+v, want remaining %s in 2026-10", st.name, exceeded, st.wantRemaining)
		}
	}

	list, err := s.ListTransactions(context.Background(), user, usecase.TransactionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("stored %d transactions, want 4 (rejected ones must not be stored)", len(list))
	}
	// Newest first: October (by ID descending), then September.
	if list[0].Amount != 24950 || list[3].Date != lastMonth {
		t.Errorf("order = %+v", list)
	}
}

func TestBudgetPeriods(t *testing.T) {
	t.Parallel()
	s := newService(t)
	setBudget(t, s, "ремонт", 100000, "none")
	setBudget(t, s, "отпуск", 100000, "year")

	if _, err := create(s, 100000, "ремонт", lastMonth); err != nil {
		t.Fatal(err)
	}
	if _, err := create(s, 1, "ремонт", today); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Errorf("all-time budget: next month error = %v, want ErrBudgetExceeded", err)
	}
	if _, err := create(s, 100000, "отпуск", civil.New(2026, time.January, 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := create(s, 1, "отпуск", today); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Errorf("yearly budget: same year error = %v, want ErrBudgetExceeded", err)
	}
	if _, err := create(s, 100000, "отпуск", civil.New(2027, time.January, 5)); err != nil {
		t.Errorf("yearly budget: next year error = %v, want nil", err)
	}
}

func TestCreateTransactionValidation(t *testing.T) {
	t.Parallel()
	s := newService(t)
	_, err := s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: 0, Category: ""})
	var v *domain.ValidationError
	if !errors.As(err, &v) || v.Fields["amount"] == "" || v.Fields["category"] == "" {
		t.Fatalf("error = %v, want validation error for amount and category", err)
	}
	list, _ := s.ListTransactions(context.Background(), user, usecase.TransactionFilter{})
	if len(list) != 0 {
		t.Error("invalid transaction was stored")
	}
}

func TestCreateTransactionExternalID(t *testing.T) {
	t.Parallel()
	s := newService(t)
	first, err := s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: 100, Category: "еда", ExternalID: "row-1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: 200, Category: "еда", ExternalID: "row-1"})
	var dup *domain.DuplicateError
	if !errors.As(err, &dup) || dup.ExistingID != first.ID || dup.ExternalID != "row-1" {
		t.Fatalf("second submission error = %v, want DuplicateError pointing at #%d", err, first.ID)
	}
	if _, err := s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: 200, Category: "еда", ExternalID: "row-2"}); err != nil {
		t.Errorf("different external id: %v", err)
	}
	// Another user may reuse the same external ID.
	if _, err := s.CreateTransaction(context.Background(), user+1, domain.TransactionInput{Amount: 200, Category: "еда", ExternalID: "row-1"}); err != nil {
		t.Errorf("same external id for another user: %v", err)
	}
}

func TestUpsertBudget(t *testing.T) {
	t.Parallel()
	s := newService(t)
	ctx := context.Background()
	b, created, err := s.UpsertBudget(ctx, user, domain.BudgetInput{Category: " Еда ", Limit: 500000})
	if err != nil || !created {
		t.Fatalf("first upsert = (%+v, %v, %v), want created", b, created, err)
	}
	if b.Category != "еда" || b.Period != domain.PeriodMonth {
		t.Errorf("budget = %+v", b)
	}
	b, created, err = s.UpsertBudget(ctx, user, domain.BudgetInput{Category: "еда", Limit: 100000, Period: "none"})
	if err != nil || created {
		t.Fatalf("second upsert = (%+v, %v, %v), want updated", b, created, err)
	}
	list, err := s.ListBudgets(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Limit != 100000 || list[0].Period != domain.PeriodNone {
		t.Errorf("ListBudgets = %+v, want one replaced budget", list)
	}
	if _, _, err := s.UpsertBudget(ctx, user, domain.BudgetInput{Category: "еда", Limit: 0}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("invalid budget error = %v, want ErrValidation", err)
	}
	other, err := s.ListBudgets(ctx, user+1)
	if err != nil || len(other) != 0 {
		t.Errorf("budgets of another user = %+v, %v", other, err)
	}
}

func TestListTransactionsFilter(t *testing.T) {
	t.Parallel()
	s := newService(t)
	for _, tx := range []struct {
		amount   money.Money
		category string
		date     civil.Date
	}{
		{100, "еда", lastMonth},
		{200, "еда", today},
		{300, "транспорт", today},
		{400, "еда", civil.New(2026, time.October, 2)},
	} {
		if _, err := create(s, tx.amount, tx.category, tx.date); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	tests := []struct {
		name        string
		filter      usecase.TransactionFilter
		wantAmounts []money.Money
		wantErr     bool
	}{
		{name: "all newest first", filter: usecase.TransactionFilter{}, wantAmounts: []money.Money{300, 200, 400, 100}},
		{name: "category is normalized", filter: usecase.TransactionFilter{Category: " ЕДА "}, wantAmounts: []money.Money{200, 400, 100}},
		{name: "from and to are inclusive", filter: usecase.TransactionFilter{From: civil.New(2026, time.October, 2), To: today}, wantAmounts: []money.Money{300, 200, 400}},
		{name: "only from", filter: usecase.TransactionFilter{From: today}, wantAmounts: []money.Money{300, 200}},
		{name: "only to", filter: usecase.TransactionFilter{To: lastMonth}, wantAmounts: []money.Money{100}},
		{name: "limit and offset", filter: usecase.TransactionFilter{Limit: 2, Offset: 1}, wantAmounts: []money.Money{200, 400}},
		{name: "offset past the end", filter: usecase.TransactionFilter{Offset: 10}, wantAmounts: []money.Money{}},
		{name: "to before from", filter: usecase.TransactionFilter{From: today, To: lastMonth}, wantErr: true},
		{name: "negative limit", filter: usecase.TransactionFilter{Limit: -1}, wantErr: true},
		{name: "negative offset", filter: usecase.TransactionFilter{Offset: -1}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list, err := s.ListTransactions(ctx, user, tc.filter)
			if tc.wantErr {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("error = %v, want ErrValidation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make([]money.Money, len(list))
			for i, tx := range list {
				got[i] = tx.Amount
			}
			if len(got) != len(tc.wantAmounts) {
				t.Fatalf("amounts = %v, want %v", got, tc.wantAmounts)
			}
			for i := range got {
				if got[i] != tc.wantAmounts[i] {
					t.Fatalf("amounts = %v, want %v", got, tc.wantAmounts)
				}
			}
		})
	}
}

func TestSummary(t *testing.T) {
	t.Parallel()
	s := newService(t)
	setBudget(t, s, "еда", 500000, "month")
	setBudget(t, s, "транспорт", 250000, "month")
	for _, tx := range []struct {
		amount   money.Money
		category string
		date     civil.Date
	}{
		{125050, "еда", today},
		{350000, "еда", today},
		{70000, "здоровье", today},
		{99900, "еда", lastMonth}, // outside the range
	} {
		if _, err := create(s, tx.amount, tx.category, tx.date); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	got, err := s.Summary(ctx, user, oct1, oct31)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 545050 || len(got.Categories) != 3 {
		t.Fatalf("summary = %+v", got)
	}
	food := got.Categories[0]
	if food.Category != "еда" || food.Spent != 475050 || food.Budget == nil || food.Budget.Percent != 95.01 || food.Budget.Remaining != 24950 {
		t.Errorf("food row = %+v / %+v", food, food.Budget)
	}
	if got.Categories[1].Category != "здоровье" || got.Categories[1].Budget != nil {
		t.Errorf("health row = %+v", got.Categories[1])
	}
	if tr := got.Categories[2]; tr.Category != "транспорт" || tr.Spent != 0 || tr.Budget.Remaining != 250000 {
		t.Errorf("transport row = %+v / %+v", tr, tr.Budget)
	}

	for name, bad := range map[string][2]civil.Date{
		"missing from":   {civil.Date{}, oct31},
		"missing to":     {oct1, civil.Date{}},
		"to before from": {oct31, oct1},
		"too long":       {oct1, civil.New(2036, time.October, 2)},
	} {
		if _, err := s.Summary(ctx, user, bad[0], bad[1]); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want ErrValidation", name, err)
		}
	}
	if _, err := s.Summary(ctx, user, oct1, civil.New(2036, time.October, 1)); err != nil {
		t.Errorf("ten years exactly: %v", err)
	}
}

func TestExportCSV(t *testing.T) {
	t.Parallel()
	s := newService(t)
	if _, err := s.CreateTransaction(context.Background(), user, domain.TransactionInput{Amount: 125050, Category: "еда", Description: `продукты, "на неделю"`, Date: today}); err != nil {
		t.Fatal(err)
	}
	if _, err := create(s, 200000, "транспорт", today); err != nil {
		t.Fatal(err)
	}
	data, err := s.ExportCSV(context.Background(), user, usecase.TransactionFilter{Limit: 1}) // the limit is ignored
	if err != nil {
		t.Fatal(err)
	}
	want := "id,date,category,amount,description,external_id\n" +
		"2,2026-10-03,транспорт,2000.00,,\n" +
		"1,2026-10-03,еда,1250.50,\"продукты, \"\"на неделю\"\"\",\n"
	if string(data) != want {
		t.Errorf("CSV =\n%s\nwant\n%s", data, want)
	}
	empty, err := s.ExportCSV(context.Background(), user+1, usecase.TransactionFilter{})
	if err != nil || string(empty) != "id,date,category,amount,description,external_id\n" {
		t.Errorf("empty export = %q, %v", empty, err)
	}
}

func TestExportCSVEscapesFormulas(t *testing.T) {
	t.Parallel()
	s := newService(t)
	for _, in := range []domain.TransactionInput{
		{Amount: 100, Category: "-еда", Description: `=HYPERLINK("http://evil";"click")`, Date: today},
		{Amount: 200, Category: "@mentions", Description: "+7 999", Date: today},
		{Amount: 300, Category: "еда", Description: "обычный текст", Date: today},
	} {
		if _, err := s.CreateTransaction(context.Background(), user, in); err != nil {
			t.Fatal(err)
		}
	}
	data, err := s.ExportCSV(context.Background(), user, usecase.TransactionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := "id,date,category,amount,description,external_id\n" +
		"3,2026-10-03,еда,3.00,обычный текст,\n" +
		"2,2026-10-03,'@mentions,2.00,'+7 999,\n" +
		"1,2026-10-03,'-еда,1.00,\"'=HYPERLINK(\"\"http://evil\"\";\"\"click\"\")\",\n"
	if string(data) != want {
		t.Errorf("CSV =\n%s\nwant\n%s", data, want)
	}
}

// TestConcurrentBudgetCheck runs many creates against one budget at once:
// exactly as many as fit must be accepted. Run with -race.
func TestConcurrentBudgetCheck(t *testing.T) {
	t.Parallel()
	s := newService(t)
	setBudget(t, s, "еда", 1000, "none")

	const workers, amount = 50, 100
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := create(s, amount, "еда", today)
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
	sum, err := s.Summary(context.Background(), user, oct1, oct31)
	if err != nil || sum.Total != 1000 {
		t.Errorf("total spent = %v (%v), want exactly the limit", sum.Total, err)
	}
}

// TestResentTransactionIsADuplicateEvenOverTheBudget covers a resend after
// the budget filled up: the answer must point at the stored transaction,
// not claim the budget is exceeded.
func TestResentTransactionIsADuplicateEvenOverTheBudget(t *testing.T) {
	t.Parallel()
	s := newService(t)
	setBudget(t, s, "еда", 1000, "month")
	ctx := context.Background()
	in := domain.TransactionInput{Amount: 800, Category: "еда", Date: today, ExternalID: "bank-1"}
	first, err := s.CreateTransaction(ctx, user, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateTransaction(ctx, user, in) // 800 + 800 would exceed 1000
	var dup *domain.DuplicateError
	if !errors.As(err, &dup) || dup.ExistingID != first.ID {
		t.Errorf("resend = %v, want a duplicate of #%d", err, first.ID)
	}
}
