package grpcclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/protoadapt"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/csvimport"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	rediscache "github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/cache/redis"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
)

const user int64 = 1

// dial returns a client connected to ln with the given call timeout.
func dial(t *testing.T, ln *bufconn.Listener, timeout time.Duration) *Client {
	t.Helper()
	c, err := New("passthrough:///bufnet", timeout, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return ln.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newLedger starts the real ledger server over an in-memory repository
// and returns a client for it, so the tests cover both ends of the wire.
func newLedger(t *testing.T) *Client {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC) }
	srv := grpcserver.New(usecase.New(memory.New(), usecase.WithClock(clock)), slog.New(slog.DiscardHandler))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return dial(t, ln, 5*time.Second)
}

// stubLedger lets a test decide how the ledger answers.
type stubLedger struct {
	ledgerv1.UnimplementedLedgerServiceServer
	listBudgets  func(context.Context) (*ledgerv1.ListBudgetsResponse, error)
	importRows   func(context.Context) (*ledgerv1.ImportTransactionsResponse, error)
	upsertBudget func(context.Context) (*ledgerv1.UpsertBudgetResponse, error)
	export       func(ledgerv1.LedgerService_ExportTransactionsCSVServer) error
}

func (s *stubLedger) ImportTransactions(ctx context.Context, _ *ledgerv1.ImportTransactionsRequest) (*ledgerv1.ImportTransactionsResponse, error) {
	return s.importRows(ctx)
}

func (s *stubLedger) UpsertBudget(ctx context.Context, _ *ledgerv1.UpsertBudgetRequest) (*ledgerv1.UpsertBudgetResponse, error) {
	return s.upsertBudget(ctx)
}

func (s *stubLedger) ListBudgets(ctx context.Context, _ *ledgerv1.ListBudgetsRequest) (*ledgerv1.ListBudgetsResponse, error) {
	return s.listBudgets(ctx)
}

func (s *stubLedger) ExportTransactionsCSV(_ *ledgerv1.ExportTransactionsCSVRequest, stream ledgerv1.LedgerService_ExportTransactionsCSVServer) error {
	return s.export(stream)
}

func newStub(t *testing.T, stub *stubLedger, timeout time.Duration) *Client {
	t.Helper()
	srv := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(srv, stub)
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return dial(t, ln, timeout)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c := newLedger(t)
	ctx := context.Background()

	b, created, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: " Еда ", Limit: 500000})
	if err != nil || !created || b != (ledgerclient.Budget{Category: "еда", Limit: 500000, Period: "month"}) {
		t.Fatalf("UpsertBudget = %+v, %v, %v", b, created, err)
	}
	if _, created, err = c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: "еда", Limit: 500000, Period: "none"}); err != nil || created {
		t.Fatalf("second UpsertBudget = created %v, %v", created, err)
	}

	tx, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 475050, Category: "еда", Description: "продукты", ExternalID: "row-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := ledgerclient.Transaction{ID: 1, Amount: 475050, Category: "еда", Description: "продукты", Date: civil.New(2026, time.October, 3), ExternalID: "row-1", CreatedAt: tx.CreatedAt}
	if tx != want || tx.CreatedAt.IsZero() {
		t.Errorf("CreateTransaction = %+v, want %+v", tx, want)
	}
	dated, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 100, Category: "здоровье", Date: civil.New(2026, time.September, 30)})
	if err != nil || !dated.Date.Equal(civil.New(2026, time.September, 30)) {
		t.Fatalf("CreateTransaction with a date = %+v, %v", dated, err)
	}

	list, err := c.ListTransactions(ctx, user, ledgerclient.TransactionFilter{Category: "ЕДА"})
	if err != nil || len(list) != 1 || list[0] != tx {
		t.Errorf("ListTransactions = %+v, %v", list, err)
	}
	list, err = c.ListTransactions(ctx, user, ledgerclient.TransactionFilter{From: civil.New(2026, time.September, 1), To: civil.New(2026, time.September, 30), Limit: 10})
	if err != nil || len(list) != 1 || list[0].ID != dated.ID {
		t.Errorf("ListTransactions by dates = %+v, %v", list, err)
	}
	csv, err := c.ExportTransactionsCSV(ctx, user, ledgerclient.TransactionFilter{})
	if err != nil || !strings.HasPrefix(string(csv), "id,date,category,amount,description,external_id\n1,2026-10-03,еда,4750.50,продукты,row-1") {
		t.Errorf("ExportTransactionsCSV = %q, %v", csv, err)
	}
	budgets, err := c.ListBudgets(ctx, user)
	if err != nil || len(budgets) != 1 || budgets[0].Period != "none" {
		t.Errorf("ListBudgets = %+v, %v", budgets, err)
	}
	if other, err := c.ListBudgets(ctx, user+1); err != nil || len(other) != 0 {
		t.Errorf("budgets of another user = %+v, %v", other, err)
	}

	sum, err := c.Summary(ctx, user, civil.New(2026, time.October, 1), civil.New(2026, time.October, 31))
	if err != nil {
		t.Fatal(err)
	}
	if !sum.From.Equal(civil.New(2026, time.October, 1)) || !sum.To.Equal(civil.New(2026, time.October, 31)) {
		t.Errorf("summary range = %s..%s", sum.From, sum.To)
	}
	if sum.Total != 475050 || len(sum.Categories) != 1 || sum.Categories[0].Budget == nil {
		t.Fatalf("Summary = %+v", sum)
	}
	if u := sum.Categories[0].Budget; *u != (ledgerclient.BudgetUsage{Period: "none", Limit: 500000, Remaining: 24950, Percent: 95.01}) {
		t.Errorf("budget usage = %+v", u)
	}
	sum, err = c.Summary(ctx, user, civil.New(2026, time.September, 1), civil.New(2026, time.October, 31))
	if err != nil || len(sum.Categories) != 2 || sum.Categories[1].Category != "здоровье" || sum.Categories[1].Budget != nil {
		t.Errorf("summary without budget = %+v, %v", sum.Categories, err)
	}
}

func TestErrorTranslationEndToEnd(t *testing.T) {
	t.Parallel()
	c := newLedger(t)
	ctx := context.Background()
	if _, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: "еда", Limit: 100}); err != nil {
		t.Fatal(err)
	}

	_, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 0, Category: " "})
	var validation *upstream.ValidationError
	if !errors.As(err, &validation) || validation.Fields["amount"] == "" || validation.Fields["category"] == "" {
		t.Errorf("validation error = %v", err)
	}

	if _, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 60, Category: "еда", ExternalID: "row-1"}); err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 41, Category: "еда"})
	var exceeded *ledgerclient.BudgetExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("budget error = %v", err)
	}
	want := ledgerclient.BudgetExceededError{Category: "еда", Period: "month", PeriodLabel: "2026-10", Limit: 100, Spent: 60, Amount: 41, Remaining: 40}
	if *exceeded != want {
		t.Errorf("budget error = %+v, want %+v", *exceeded, want)
	}

	_, err = c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 1, Category: "еда", ExternalID: "row-1"})
	var duplicate *ledgerclient.DuplicateError
	if !errors.As(err, &duplicate) || duplicate.ExistingID != 1 || duplicate.ExternalID != "row-1" {
		t.Errorf("duplicate error = %v", err)
	}

	_, err = c.ListTransactions(ctx, user, ledgerclient.TransactionFilter{Limit: -1})
	if !errors.As(err, &validation) || validation.Fields["limit"] == "" {
		t.Errorf("filter error = %v", err)
	}
	_, err = c.ExportTransactionsCSV(ctx, user, ledgerclient.TransactionFilter{From: civil.New(2026, time.October, 2), To: civil.New(2026, time.October, 1)})
	if !errors.As(err, &validation) || validation.Fields["to"] == "" {
		t.Errorf("export filter error = %v", err)
	}
	_, err = c.Summary(ctx, user, civil.Date{}, civil.Date{})
	if !errors.As(err, &validation) || validation.Fields["from"] == "" || validation.Fields["to"] == "" {
		t.Errorf("summary error = %v", err)
	}
	_, err = c.ListBudgets(ctx, 0)
	if !errors.As(err, &validation) || validation.Fields["user_id"] == "" {
		t.Errorf("missing user error = %v", err)
	}
}

func TestTranslate(t *testing.T) {
	t.Parallel()
	with := func(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
		st, err := status.New(code, msg).WithDetails(details...)
		if err != nil {
			t.Fatal(err)
		}
		return st.Err()
	}
	budget := &ledgerv1.BudgetExceeded{Category: "еда", Period: "year", PeriodLabel: "2026", LimitKopecks: 100, SpentKopecks: 90, AmountKopecks: 20, RemainingKopecks: 10}
	tests := []struct {
		name  string
		in    error
		check func(error) bool
	}{
		{"budget exceeded", with(codes.FailedPrecondition, "over", budget),
			func(err error) bool {
				var e *ledgerclient.BudgetExceededError
				return errors.As(err, &e) && *e == ledgerclient.BudgetExceededError{Category: "еда", Period: "year", PeriodLabel: "2026", Limit: 100, Spent: 90, Amount: 20, Remaining: 10}
			}},
		{"duplicate", with(codes.AlreadyExists, "dup", &ledgerv1.DuplicateTransaction{ExternalId: "row-1", ExistingId: 7}),
			func(err error) bool {
				var e *ledgerclient.DuplicateError
				return errors.As(err, &e) && *e == ledgerclient.DuplicateError{ExternalID: "row-1", ExistingID: 7}
			}},
		{"budget detail with another code is not trusted", with(codes.AlreadyExists, "odd", budget),
			func(err error) bool {
				var e *ledgerclient.BudgetExceededError
				return !errors.As(err, &e) && errors.Is(err, upstream.ErrConflict)
			}},
		{"failed precondition without detail", with(codes.FailedPrecondition, "no"),
			func(err error) bool { return errors.Is(err, upstream.ErrConflict) }},
		{"generic codes go through upstream", with(codes.Unavailable, "down"),
			func(err error) bool {
				var e *upstream.UnavailableError
				return errors.As(err, &e) && e.Service == "ledger"
			}},
		{"not a status", io.ErrUnexpectedEOF,
			func(err error) bool { return errors.Is(err, io.ErrUnexpectedEOF) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := translate(tc.in); !tc.check(got) {
				t.Errorf("translate(%v) = %#v", tc.in, got)
			}
		})
	}
}

func TestLedgerDown(t *testing.T) {
	t.Parallel()
	ln := bufconn.Listen(1 << 10)
	_ = ln.Close() // nothing listens: every dial fails
	c := dial(t, ln, 5*time.Second)
	start := time.Now()
	_, err := c.ListBudgets(context.Background(), user)
	if !errors.Is(err, upstream.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the call waited %s instead of failing fast", d)
	}
}

func TestTimeout(t *testing.T) {
	t.Parallel()
	c := newStub(t, &stubLedger{listBudgets: func(ctx context.Context) (*ledgerv1.ListBudgetsResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}, 50*time.Millisecond)
	_, err := c.ListBudgets(context.Background(), user)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestExportTimeout(t *testing.T) {
	t.Parallel()
	c := newStub(t, &stubLedger{export: func(stream ledgerv1.LedgerService_ExportTransactionsCSVServer) error {
		if err := stream.Send(&ledgerv1.ExportTransactionsCSVResponse{Chunk: []byte("id\n")}); err != nil {
			return err
		}
		<-stream.Context().Done() // the rest never comes
		return stream.Context().Err()
	}}, 50*time.Millisecond)
	if _, err := c.ExportTransactionsCSV(context.Background(), user, ledgerclient.TransactionFilter{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestEmptyUpsertResponseIsInternal(t *testing.T) {
	t.Parallel()
	c := newStub(t, &stubLedger{upsertBudget: func(context.Context) (*ledgerv1.UpsertBudgetResponse, error) {
		return &ledgerv1.UpsertBudgetResponse{Created: true}, nil
	}}, 5*time.Second)
	_, _, err := c.UpsertBudget(context.Background(), user, ledgerclient.BudgetInput{Category: "еда", Limit: 100})
	var validation *upstream.ValidationError
	if err == nil || errors.As(err, &validation) {
		t.Errorf("err = %v, want a plain internal error", err)
	}
}

func TestCallerCancel(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	c := newStub(t, &stubLedger{listBudgets: func(ctx context.Context) (*ledgerv1.ListBudgetsResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	_, err := c.ListBudgets(ctx, user)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want Canceled", err)
	}
}

func TestRequestIDForwarded(t *testing.T) {
	t.Parallel()
	got := make(chan []string, 2)
	c := newStub(t, &stubLedger{listBudgets: func(ctx context.Context) (*ledgerv1.ListBudgetsResponse, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		got <- md.Get(requestid.MetadataKey)
		return &ledgerv1.ListBudgetsResponse{}, nil
	}}, 5*time.Second)

	if _, err := c.ListBudgets(requestid.With(context.Background(), "req-42"), user); err != nil {
		t.Fatal(err)
	}
	if ids := <-got; len(ids) != 1 || ids[0] != "req-42" {
		t.Errorf("request id in metadata = %q, want [req-42]", ids)
	}
	if _, err := c.ListBudgets(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	if ids := <-got; len(ids) != 0 {
		t.Errorf("request id without one in the context = %q, want none", ids)
	}
}

func TestExportCollectsChunksBeyondMessageLimit(t *testing.T) {
	t.Parallel()
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	const chunks = 80 // 5 MB in all, more than the 4 MB limit of one message
	c := newStub(t, &stubLedger{export: func(stream ledgerv1.LedgerService_ExportTransactionsCSVServer) error {
		for range chunks {
			if err := stream.Send(&ledgerv1.ExportTransactionsCSVResponse{Chunk: chunk}); err != nil {
				return err
			}
		}
		return nil
	}}, 5*time.Second)
	data, err := c.ExportTransactionsCSV(context.Background(), user, ledgerclient.TransactionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != chunks*len(chunk) {
		t.Errorf("got %d bytes, want %d", len(data), chunks*len(chunk))
	}
}

func TestExportErrorMidStream(t *testing.T) {
	t.Parallel()
	c := newStub(t, &stubLedger{export: func(stream ledgerv1.LedgerService_ExportTransactionsCSVServer) error {
		if err := stream.Send(&ledgerv1.ExportTransactionsCSVResponse{Chunk: []byte("id,date\n")}); err != nil {
			return err
		}
		return status.Error(codes.Internal, "internal error")
	}}, 5*time.Second)
	data, err := c.ExportTransactionsCSV(context.Background(), user, ledgerclient.TransactionFilter{})
	if err == nil || data != nil {
		t.Errorf("ExportTransactionsCSV = %q, %v; want no data and an error", data, err)
	}
}

func TestBadResponseFromLedgerIsInternal(t *testing.T) {
	t.Parallel()
	for name, tx := range map[string]*ledgerv1.Transaction{
		"bad date":       {Date: "03.10.2026"},
		"no transaction": nil,
	} {
		_, err := fromTransaction(tx)
		var validation *upstream.ValidationError
		if err == nil || errors.As(err, &validation) || errors.Is(err, upstream.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want a plain internal error", name, err)
		}
	}
}

func TestClampInt32(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   int
		want int32
	}{
		{0, 0}, {50, 50}, {-1, -1},
		{math.MaxInt32, math.MaxInt32}, {math.MaxInt32 + 1, math.MaxInt32}, {math.MaxInt64, math.MaxInt32},
		{math.MinInt32 - 1, math.MinInt32}, {math.MinInt64, math.MinInt32},
	} {
		if got := clampInt32(tc.in); got != tc.want {
			t.Errorf("clampInt32(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSummaryCacheStatus(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := usecase.New(memory.New(), usecase.WithReportCache(rediscache.New(rdb, time.Minute), slog.New(slog.DiscardHandler)))
	srv := grpcserver.New(svc, slog.New(slog.DiscardHandler))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	c := dial(t, ln, 5*time.Second)
	for _, want := range []string{"MISS", "HIT"} {
		sum, err := c.Summary(context.Background(), user, civil.New(2026, time.October, 1), civil.New(2026, time.October, 31))
		if err != nil || sum.Cache != want {
			t.Errorf("Summary cache = %q, %v; want %s", sum.Cache, err, want)
		}
	}
	// Without a cache the status is empty.
	if sum, err := newLedger(t).Summary(context.Background(), user, civil.New(2026, time.October, 1), civil.New(2026, time.October, 31)); err != nil || sum.Cache != "" {
		t.Errorf("without a cache: %q, %v", sum.Cache, err)
	}
}

func TestImportEndToEnd(t *testing.T) {
	t.Parallel()
	c := newLedger(t)
	ctx := context.Background()
	if _, _, err := c.UpsertBudget(ctx, user, ledgerclient.BudgetInput{Category: "еда", Limit: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTransaction(ctx, user, ledgerclient.TransactionInput{Amount: 1, Category: "прочее", ExternalID: "old"}); err != nil {
		t.Fatal(err)
	}
	in := func(line int, amount money.Money, category, ext string) ledgerclient.ImportRow {
		return ledgerclient.ImportRow{Line: line, Input: ledgerclient.TransactionInput{Amount: amount, Category: category, ExternalID: ext, Description: "импорт"}}
	}
	rows := []ledgerclient.ImportRow{
		in(2, 600, "еда", "a"),
		in(3, 600, "еда", "b"),  // over the budget
		in(4, 5, "кафе", "old"), // stored before
		in(5, 0, "кафе", "c"),   // invalid
		in(6, 400, "еда", "d"),
		in(7, 5, "кафе", "a"), // repeats line 2
	}
	results, err := c.ImportTransactions(ctx, user, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(rows) {
		t.Fatalf("%d results for %d rows", len(results), len(rows))
	}
	var (
		exceeded  *ledgerclient.BudgetExceededError
		duplicate *ledgerclient.DuplicateError
		invalid   *upstream.ValidationError
	)
	if results[0].ID == 0 || results[0].Err != nil || results[4].ID == 0 {
		t.Errorf("stored rows: %+v, %+v", results[0], results[4])
	}
	if !errors.As(results[1].Err, &exceeded) || exceeded.Remaining != 400 {
		t.Errorf("line 3: %v, want the budget error with 400 remaining", results[1].Err)
	}
	if !errors.As(results[2].Err, &duplicate) || duplicate.ExternalID != "old" || duplicate.ExistingID != 1 {
		t.Errorf("line 4: %v, want a duplicate of #1", results[2].Err)
	}
	if !errors.As(results[3].Err, &invalid) || invalid.Fields["amount"] == "" {
		t.Errorf("line 5: %v", results[3].Err)
	}
	if !errors.As(results[5].Err, &duplicate) || duplicate.ExistingID != results[0].ID {
		t.Errorf("line 7: %v, want a duplicate of line 2 (#%d)", results[5].Err, results[0].ID)
	}
	for i, r := range results {
		if r.Line != rows[i].Line {
			t.Errorf("result %d has line %d", i, r.Line)
		}
	}
}

func TestExportImportsBack(t *testing.T) {
	t.Parallel()
	c := newLedger(t)
	ctx := context.Background()
	for _, tx := range []ledgerclient.TransactionInput{
		{Amount: 475050, Category: "еда", Description: "продукты, неделя", Date: civil.New(2026, time.October, 3), ExternalID: "bank-1"},
		{Amount: 70000, Category: "здоровье", Description: "=не формула", Date: civil.New(2026, time.October, 4)},
	} {
		if _, err := c.CreateTransaction(ctx, user, tx); err != nil {
			t.Fatal(err)
		}
	}
	exported, err := c.ExportTransactionsCSV(ctx, user, ledgerclient.TransactionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := csvimport.Parse(exported)
	if err != nil || len(parsed.Problems) != 0 {
		t.Fatalf("parse the export: %v, %+v", err, parsed.Problems)
	}
	importInto := func(u int64) []ledgerclient.ImportResult {
		t.Helper()
		results, err := c.ImportTransactions(ctx, u, parsed.Rows)
		if err != nil {
			t.Fatal(err)
		}
		return results
	}

	// Into the same account: every row is already there.
	for _, r := range importInto(user) {
		var d *ledgerclient.DuplicateError
		if !errors.As(r.Err, &d) {
			t.Errorf("same account, line %d: %v, want a duplicate", r.Line, r.Err)
		}
	}
	// Into another account: the same data, with external IDs that make a
	// second import store nothing.
	for _, r := range importInto(user + 1) {
		if r.Err != nil {
			t.Fatalf("another account, line %d: %v", r.Line, r.Err)
		}
	}
	for _, r := range importInto(user + 1) {
		if r.Err == nil {
			t.Errorf("second import into another account stored line %d again", r.Line)
		}
	}
	again, err := c.ExportTransactionsCSV(ctx, user+1, ledgerclient.TransactionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Only the ids differ, and the external ID of the row that had none.
	rows := func(b []byte) [][]string {
		var out [][]string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n")[1:] {
			out = append(out, strings.Split(line, ","))
		}
		return out
	}
	a, b := rows(exported), rows(again)
	if len(a) != len(b) {
		t.Fatalf("%d rows exported, %d after the round trip", len(a), len(b))
	}
	for i := range a {
		last := len(a[i]) - 1
		if !reflect.DeepEqual(a[i][1:last], b[i][1:last]) {
			t.Errorf("row %d changed: %v → %v", i, a[i], b[i])
		}
		wantExt := a[i][last]
		if wantExt == "" {
			wantExt = "export-" + a[i][0]
		}
		if b[i][last] != wantExt {
			t.Errorf("row %d external_id = %q, want %q", i, b[i][last], wantExt)
		}
	}
}

func TestImportLargerThanTheDefaultMessageLimit(t *testing.T) {
	t.Parallel()
	c := newLedger(t)
	rows := make([]ledgerclient.ImportRow, 10_000)
	description := strings.Repeat("д", 200) // 400 bytes; about 4.5 MB in all
	for i := range rows {
		rows[i] = ledgerclient.ImportRow{Line: i + 2, Input: ledgerclient.TransactionInput{Amount: 1, Category: fmt.Sprintf("к%d", i%7), Description: description}}
	}
	results, err := c.ImportTransactions(context.Background(), user, rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("line %d: %v", r.Line, r.Err)
		}
	}
}

func TestImportUsesItsOwnTimeout(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	stub := &stubLedger{importRows: func(ctx context.Context) (*ledgerv1.ImportTransactionsResponse, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	c := newStub(t, stub, 5*time.Second).WithImportTimeout(50 * time.Millisecond)
	start := time.Now()
	_, err := c.ImportTransactions(context.Background(), user, []ledgerclient.ImportRow{{Line: 2}})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("err = %v after %s, want a timeout after the import timeout", err, time.Since(start))
	}
}
