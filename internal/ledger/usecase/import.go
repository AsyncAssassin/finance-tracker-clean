package usecase

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// MaxImportRows bounds one import.
const MaxImportRows = 10_000

// DefaultImportWorkers is how many import rows are stored at once when
// WithImportWorkers is not used.
const DefaultImportWorkers = 4

// WithImportWorkers sets how many import rows are stored at once, across
// all imports running together: each row being stored holds a database
// connection, and imports must leave connections to the other requests.
func WithImportWorkers(n int) Option {
	return func(s *Service) { s.importSlots = make(chan struct{}, max(n, 1)) }
}

// ImportRow is one transaction to import and the line it came from.
type ImportRow struct {
	Line  int
	Input domain.TransactionInput
	// SourceID is the id column of a CSV export, 0 when absent: a row whose
	// transaction the user has is not stored again, and in another account
	// it becomes the external ID "export-{id}" when the row has none.
	SourceID int64
}

// ImportResult is the outcome of one row: the stored transaction, or the
// error that kept it out. Errors of the row itself are those of
// CreateTransaction; a row the import did not reach has the cause of the
// stop: the context's error or an *ImportStoppedError.
type ImportResult struct {
	Line        int
	Transaction domain.Transaction
	Err         error
}

// ImportStoppedError is the error of the rows not stored because storage
// failed on an earlier row.
type ImportStoppedError struct {
	Line int // the row that failed
	Err  error
}

func (e *ImportStoppedError) Error() string {
	return fmt.Sprintf("not stored: the import stopped after an internal error on line %d", e.Line)
}

func (e *ImportStoppedError) Unwrap() error { return e.Err }

// Import stores rows the way CreateTransaction stores one transaction and
// returns a result for every row, in the order of rows. A row with bad
// data does not stop the others.
//
// The rows are split into groups: rows of one category, and rows with the
// same external ID, end up in the same group. A group is stored by one
// goroutine in the order of rows, so budget checks and duplicates follow
// the order of the file whatever the scheduling, while groups run in
// parallel. At most the WithImportWorkers number of rows are stored at
// once, across all imports.
//
// The import stops early when ctx ends or when storage fails, not the
// data: the remaining rows then fail with the cause instead of all failing
// the same way. The report cache is invalidated once at the end.
func (s *Service) Import(ctx context.Context, userID int64, rows []ImportRow) ([]ImportResult, error) {
	if len(rows) > MaxImportRows {
		v := new(domain.ValidationError)
		v.Add("rows", "at most "+strconv.Itoa(MaxImportRows)+" rows per import")
		return nil, v
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	results := make([]ImportResult, len(rows))
	groups := groupRows(rows)
	jobs := make(chan []int)
	var wg sync.WaitGroup
	for range min(cap(s.importSlots), max(len(groups), 1)) {
		wg.Go(func() {
			for group := range jobs {
				for _, i := range group {
					// Each index belongs to one group, so the goroutines write
					// disjoint elements of results and need no lock.
					results[i] = s.importRow(ctx, stop, userID, rows[i])
				}
			}
		})
	}
	for _, g := range groups {
		jobs <- g // the goroutines always drain, even after a stop
	}
	close(jobs)
	wg.Wait()

	for _, r := range results {
		if r.Err == nil || errors.Is(r.Err, ErrCommitUncertain) {
			s.invalidateReports(ctx, userID)
			break
		}
	}
	return results, nil
}

// importRow stores one row. stop ends the import when storage fails.
func (s *Service) importRow(ctx context.Context, stop context.CancelCauseFunc, userID int64, row ImportRow) ImportResult {
	res := ImportResult{Line: row.Line}
	select {
	case s.importSlots <- struct{}{}:
		defer func() { <-s.importSlots }()
	case <-ctx.Done():
		res.Err = context.Cause(ctx)
		return res
	}
	if ctx.Err() != nil {
		res.Err = context.Cause(ctx)
		return res
	}
	in := row.Input
	if row.SourceID > 0 {
		owned, err := s.repo.HasTransaction(ctx, userID, row.SourceID)
		switch {
		case err != nil:
			res.Err = s.stopOnStorageError(ctx, stop, row.Line, fmt.Errorf("check source id: %w", err))
			return res
		case owned:
			// Ownership is known; the request's key need not be the stored key.
			res.Err = &domain.DuplicateError{ExistingID: row.SourceID}
			return res
		case strings.TrimSpace(in.ExternalID) == "":
			in.ExternalID = "export-" + strconv.FormatInt(row.SourceID, 10)
		}
	}
	tx, err := s.createTransaction(ctx, userID, in)
	if err != nil {
		res.Err = s.stopOnStorageError(ctx, stop, row.Line, err)
		return res
	}
	res.Transaction = tx
	return res
}

// stopOnStorageError returns the error of a row and stops the import when
// it is a storage failure rather than a problem of the row's data.
func (s *Service) stopOnStorageError(ctx context.Context, stop context.CancelCauseFunc, line int, err error) error {
	var (
		v *domain.ValidationError
		b *domain.BudgetExceededError
		d *domain.DuplicateError
	)
	switch {
	case errors.As(err, &v), errors.As(err, &b), errors.As(err, &d), errors.Is(err, money.ErrOverflow), errors.Is(err, domain.ErrQuotaExceeded):
		return err
	case ctx.Err() != nil:
		if errors.Is(err, ErrCommitUncertain) {
			// Keep the cancellation cause and the need to invalidate reports.
			return errors.Join(context.Cause(ctx), err)
		}
		return context.Cause(ctx) // stopped meanwhile: report why
	default:
		stop(&ImportStoppedError{Line: line, Err: err})
		return err
	}
}

// groupRows returns the indices of rows grouped so that rows of one
// category, and rows with one external ID, share a group; groups and the
// rows within them are in the order of rows.
func groupRows(rows []ImportRow) [][]int {
	parent := make([]int, len(rows))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra > rb {
			ra, rb = rb, ra
		}
		parent[rb] = ra // the smallest index is the root
	}
	firstOf := map[string]int{}
	link := func(key string, i int) {
		if j, ok := firstOf[key]; ok {
			union(i, j)
		} else {
			firstOf[key] = i
		}
	}
	for i, r := range rows {
		// Invalid categories are rejected by importRow before any write.
		category, _ := domain.NormalizeCategory(r.Input.Category)
		link("category:"+category, i)
		id := strings.TrimSpace(r.Input.ExternalID)
		if id == "" && r.SourceID > 0 {
			id = "export-" + strconv.FormatInt(r.SourceID, 10)
		}
		if id != "" {
			link("external:"+id, i)
		}
	}
	groupOf := map[int]int{}
	var groups [][]int
	for i := range rows {
		root := find(i)
		g, ok := groupOf[root]
		if !ok {
			g = len(groups)
			groupOf[root] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups
}
