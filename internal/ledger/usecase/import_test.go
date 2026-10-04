package usecase_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

func row(line int, amount money.Money, category, externalID string) usecase.ImportRow {
	return usecase.ImportRow{Line: line, Input: domain.TransactionInput{Amount: amount, Category: category, Date: today, ExternalID: externalID}}
}

// outcome shortens a result to compare runs: "ok" or the kind of error.
func outcome(r usecase.ImportResult) string {
	var (
		v *domain.ValidationError
		b *domain.BudgetExceededError
		d *domain.DuplicateError
		s *usecase.ImportStoppedError
	)
	switch {
	case r.Err == nil:
		return "ok"
	case errors.As(r.Err, &s):
		return "stopped"
	case errors.As(r.Err, &v):
		return "invalid"
	case errors.As(r.Err, &b):
		return "budget"
	case errors.As(r.Err, &d):
		return "duplicate"
	case errors.Is(r.Err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}

func outcomes(results []usecase.ImportResult) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = outcome(r)
	}
	return out
}

func importer(t *testing.T, repo usecase.Repository, workers int) *usecase.Service {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC) }
	return usecase.New(repo, usecase.WithClock(clock), usecase.WithImportWorkers(workers))
}

func TestImport(t *testing.T) {
	t.Parallel()
	s := importer(t, memory.New(), 4)
	ctx := context.Background()
	old, err := s.CreateTransaction(ctx, user, domain.TransactionInput{Amount: 1, Category: "прочее", ExternalID: "old-1"})
	if err != nil {
		t.Fatal(err)
	}
	rows := []usecase.ImportRow{
		row(2, 1000, "еда", "r2"),
		row(3, 0, "еда", "r3"),            // invalid amount
		row(4, 500, "транспорт", "old-1"), // stored before
		row(5, 700, "кафе", "r2"),         // repeats line 2
		row(6, 300, "здоровье", ""),
	}
	results, err := s.Import(ctx, user, rows)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcomes(results), []string{"ok", "invalid", "duplicate", "duplicate", "ok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	for i, r := range results {
		if r.Line != rows[i].Line {
			t.Errorf("result %d has line %d, want %d", i, r.Line, rows[i].Line)
		}
	}
	var d *domain.DuplicateError
	if errors.As(results[2].Err, &d); d == nil || d.ExistingID != old.ID {
		t.Errorf("line 4: %v, want a duplicate of #%d", results[2].Err, old.ID)
	}
	if errors.As(results[3].Err, &d); d == nil || d.ExistingID != results[0].Transaction.ID {
		t.Errorf("line 5: %v, want a duplicate of line 2 (#%d)", results[3].Err, results[0].Transaction.ID)
	}
}

func TestCorrectedRowWithTheSameExternalIDIsStored(t *testing.T) {
	t.Parallel()
	s := importer(t, memory.New(), 4)
	results, err := s.Import(context.Background(), user, []usecase.ImportRow{
		row(2, 0, "еда", "A"),  // rejected: no amount
		row(5, 50, "еда", "A"), // the corrected row
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(results); !reflect.DeepEqual(got, []string{"invalid", "ok"}) {
		t.Errorf("outcomes = %v, want the corrected row stored", got)
	}
}

func TestImportFollowsTheFileOrderWithinACategory(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{{"еда", "еда"}, {"café", "cafe\u0301"}, {"\u0130", "I\u0307"}, {"x" + strings.Repeat("\u0301", 29) + "\u0327", "x\u0327" + strings.Repeat("\u0301", 29)}} {
		t.Run(pair[0], func(t *testing.T) {
			rows := []usecase.ImportRow{
				row(2, 600, pair[0], ""),
				row(3, 100, "транспорт", ""),
				row(4, 600, pair[1], ""), // 1200 > 1000: rejected
				row(5, 400, pair[0], ""), // 1000: fits exactly
				row(6, 300, "кафе", ""),
				row(7, 900, "кафе", ""), // 1200 > 1000: rejected
				row(8, 200, "транспорт", ""),
			}
			want := []string{"ok", "ok", "budget", "ok", "ok", "budget", "ok"}
			for range 30 {
				s := importer(t, memory.New(), 8)
				setBudget(t, s, pair[0], 1000, "month")
				setBudget(t, s, "кафе", 1000, "month")
				results, err := s.Import(context.Background(), user, rows)
				if err != nil {
					t.Fatal(err)
				}
				if got := outcomes(results); !reflect.DeepEqual(got, want) {
					t.Fatalf("outcomes = %v, want %v on every run", got, want)
				}
			}
		})
	}
}

// barrierRepo makes the first n transactions wait until n are running at
// once, proving the import runs n groups in parallel.
type barrierRepo struct {
	*memory.Repo
	n       int32
	arrived atomic.Int32
	all     chan struct{}
	once    sync.Once
}

func (r *barrierRepo) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	if r.arrived.Add(1) <= r.n {
		if r.arrived.Load() == r.n {
			r.once.Do(func() { close(r.all) })
		}
		select {
		case <-r.all:
		case <-time.After(5 * time.Second):
			return errors.New("the other workers never came: the import is not parallel")
		}
	}
	return r.Repo.InTx(ctx, fn)
}

func TestImportRunsGroupsInParallel(t *testing.T) {
	t.Parallel()
	repo := &barrierRepo{Repo: memory.New(), n: 3, all: make(chan struct{})}
	results, err := importer(t, repo, 3).Import(context.Background(), user,
		[]usecase.ImportRow{row(2, 1, "еда", ""), row(3, 1, "транспорт", ""), row(4, 1, "кафе", "")})
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(results); !reflect.DeepEqual(got, []string{"ok", "ok", "ok"}) {
		t.Errorf("outcomes = %v", got)
	}
}

// countingRepo records how many transactions run at once.
type countingRepo struct {
	*memory.Repo
	now, peak atomic.Int32
}

func (r *countingRepo) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	n := r.now.Add(1)
	defer r.now.Add(-1)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(time.Millisecond)
	return r.Repo.InTx(ctx, fn)
}

func TestImportsShareTheRowLimit(t *testing.T) {
	t.Parallel()
	repo := &countingRepo{Repo: memory.New()}
	s := importer(t, repo, 2)
	var rows []usecase.ImportRow
	for i, c := range []string{"а", "б", "в", "г", "д", "е"} {
		rows = append(rows, row(i+2, 1, c, ""))
	}
	var wg sync.WaitGroup
	for range 3 { // three imports at once
		wg.Go(func() {
			if _, err := s.Import(context.Background(), user, rows); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if p := repo.peak.Load(); p > 2 {
		t.Errorf("%d import rows were stored at once, want at most 2 across all imports", p)
	}
}

// failingRepo fails every transaction after the first ones.
type failingRepo struct {
	*memory.Repo
	ok    int32
	calls atomic.Int32
}

func (r *failingRepo) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	if r.calls.Add(1) > r.ok {
		return errors.New("connection refused")
	}
	return r.Repo.InTx(ctx, fn)
}

func TestStorageFailureStopsTheImport(t *testing.T) {
	t.Parallel()
	repo := &failingRepo{Repo: memory.New(), ok: 1}
	rows := []usecase.ImportRow{row(2, 1, "еда", ""), row(3, 1, "еда", ""), row(4, 1, "еда", ""), row(5, 1, "еда", "")}
	results, err := importer(t, repo, 1).Import(context.Background(), user, rows)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcomes(results), []string{"ok", "error", "stopped", "stopped"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
	var stopped *usecase.ImportStoppedError
	if errors.As(results[2].Err, &stopped); stopped == nil || stopped.Line != 3 {
		t.Errorf("line 4: %v, want stopped because of line 3", results[2].Err)
	}
	if n := repo.calls.Load(); n != 2 {
		t.Errorf("%d transactions tried, want 2: no more after the failure", n)
	}
}

// cancelRepo cancels the import after the given number of transactions.
type cancelRepo struct {
	*memory.Repo
	after  int32
	done   atomic.Int32
	cancel context.CancelFunc
}

func (r *cancelRepo) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	err := r.Repo.InTx(ctx, fn)
	if r.done.Add(1) == r.after {
		r.cancel()
	}
	return err
}

func TestImportStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	repo := &cancelRepo{Repo: memory.New(), after: 2, cancel: cancel}
	rows := []usecase.ImportRow{row(2, 1, "еда", ""), row(3, 1, "еда", ""), row(4, 1, "еда", ""), row(5, 1, "еда", "")}
	results, err := importer(t, repo, 2).Import(ctx, user, rows)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := outcomes(results), []string{"ok", "ok", "canceled", "canceled"}; !reflect.DeepEqual(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
}

func TestSourceIDOfAnExport(t *testing.T) {
	t.Parallel()
	s := importer(t, memory.New(), 4)
	ctx := context.Background()
	mine, err := s.CreateTransaction(ctx, user, domain.TransactionInput{Amount: 100, Category: "еда"})
	if err != nil {
		t.Fatal(err)
	}
	exported := []usecase.ImportRow{{Line: 2, SourceID: mine.ID, Input: domain.TransactionInput{Amount: 100, Category: "еда"}}}

	// Back into the same account: the transaction is already there.
	results, _ := s.Import(ctx, user, exported)
	var d *domain.DuplicateError
	if errors.As(results[0].Err, &d); d == nil || d.ExistingID != mine.ID {
		t.Errorf("same account: %v, want a duplicate of #%d", results[0].Err, mine.ID)
	}
	// Into another account, twice: stored once.
	first, _ := s.Import(ctx, user+1, exported)
	second, _ := s.Import(ctx, user+1, exported)
	if outcome(first[0]) != "ok" || first[0].Transaction.ExternalID != "export-1" || outcome(second[0]) != "duplicate" {
		t.Errorf("another account: %+v then %v; want stored with external_id export-1, then a duplicate", first[0], second[0].Err)
	}
}

// Hold the first source lookup so an incorrectly independent group can
// commit first. Correctly grouped rows proceed after the bounded wait.
type delayedSourceRepo struct {
	*memory.Repo
	first    atomic.Bool
	inserted chan struct{}
	once     sync.Once
}

func (r *delayedSourceRepo) HasTransaction(ctx context.Context, userID, id int64) (bool, error) {
	if !r.first.Swap(true) {
		select {
		case <-r.inserted:
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return r.Repo.HasTransaction(ctx, userID, id)
}

func (r *delayedSourceRepo) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	err := r.Repo.InTx(ctx, fn)
	r.once.Do(func() { close(r.inserted) })
	return err
}

func TestImportFollowsFileOrderForExportKeys(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "repeated source ID", true: "explicit export key"}[explicit], func(t *testing.T) {
			repo := &delayedSourceRepo{Repo: memory.New(), inserted: make(chan struct{})}
			first, second := row(2, 100, "first", ""), row(3, 200, "second", "")
			first.SourceID = 1000
			if explicit {
				second.Input.ExternalID = "export-1000"
			} else {
				second.SourceID = first.SourceID
			}
			results, err := importer(t, repo, 2).Import(context.Background(), user, []usecase.ImportRow{first, second})
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomes(results); !reflect.DeepEqual(got, []string{"ok", "duplicate"}) {
				t.Fatalf("outcomes = %v, want the first row stored", got)
			}
			if tx := results[0].Transaction; tx.Category != "first" || tx.Amount != 100 || tx.ExternalID != "export-1000" {
				t.Fatalf("stored transaction = %+v", tx)
			}
		})
	}
}

func TestImportLimits(t *testing.T) {
	t.Parallel()
	s := importer(t, memory.New(), 0) // less than one worker still makes progress
	results, err := s.Import(context.Background(), user, nil)
	if err != nil || len(results) != 0 {
		t.Errorf("empty import = %v, %v", results, err)
	}
	if _, err := s.Import(context.Background(), user, make([]usecase.ImportRow, usecase.MaxImportRows+1)); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("too many rows: %v, want a validation error", err)
	}
	if results, _ := s.Import(context.Background(), user, []usecase.ImportRow{row(2, 1, "еда", "")}); results[0].Err != nil {
		t.Errorf("one row: %v", results[0].Err)
	}
}

func TestImportInvalidatesTheCacheOnce(t *testing.T) {
	t.Parallel()
	cache := &flakyCache{}
	s, _ := newCachedService(t, cache)
	rows := []usecase.ImportRow{row(2, 1, "еда", ""), row(3, 1, "транспорт", ""), row(4, 1, "кафе", "")}
	if _, err := s.Import(context.Background(), user, rows); err != nil {
		t.Fatal(err)
	}
	if n := cache.invalidations.Load(); n != 1 {
		t.Errorf("invalidations = %d, want 1 for the whole import", n)
	}
	if _, err := s.Import(context.Background(), user, []usecase.ImportRow{row(2, 0, "", "")}); err != nil {
		t.Fatal(err)
	}
	if n := cache.invalidations.Load(); n != 1 {
		t.Errorf("an import that stored nothing invalidated the cache")
	}
}

type aggregateOverflowOnce struct {
	*memory.Repo
	seen atomic.Bool
}

func (r *aggregateOverflowOnce) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	if !r.seen.Swap(true) {
		return money.ErrOverflow
	}
	return r.Repo.InTx(ctx, fn)
}
func TestAggregateOverflowDoesNotStopOtherImportRows(t *testing.T) {
	s := importer(t, &aggregateOverflowOnce{Repo: memory.New()}, 1)
	results, err := s.Import(context.Background(), user, []usecase.ImportRow{row(2, 100, "a", "one"), row(3, 100, "b", "two")})
	if err != nil || len(results) != 2 || !errors.Is(results[0].Err, money.ErrOverflow) || results[1].Err != nil {
		t.Fatalf("results=%+v err=%v", results, err)
	}
}
