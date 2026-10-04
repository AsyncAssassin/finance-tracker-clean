package grpcserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	rediscache "github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/cache/redis"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
)

const user int64 = 1

// hookRepo is the in-memory repository with ListBudgets and
// ListTransactions replaceable, to make the ledger fail on demand.
type hookRepo struct {
	*memory.Repo
	listBudgets      func(ctx context.Context) ([]domain.Budget, error)
	listTransactions func(ctx context.Context) ([]domain.Transaction, error)
}

func (r *hookRepo) ListBudgets(ctx context.Context, userID int64) ([]domain.Budget, error) {
	if r.listBudgets != nil {
		return r.listBudgets(ctx)
	}
	return r.Repo.ListBudgets(ctx, userID)
}

func (r *hookRepo) ListTransactions(ctx context.Context, userID int64, f usecase.TransactionFilter) ([]domain.Transaction, error) {
	if r.listTransactions != nil {
		return r.listTransactions(ctx)
	}
	return r.Repo.ListTransactions(ctx, userID, f)
}

// syncBuffer collects log output written from the server goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	srv    *grpckit.Server
	client ledgerv1.LedgerServiceClient
	health healthpb.HealthClient
	logs   *syncBuffer
}

func start(t *testing.T, repo usecase.Repository) env {
	t.Helper()
	clock := func() time.Time { return time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC) }
	logs := &syncBuffer{}
	srv := grpcserver.New(usecase.New(repo, usecase.WithClock(clock)), slog.New(slog.NewJSONHandler(logs, nil)))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return env{srv: srv, client: ledgerv1.NewLedgerServiceClient(conn), health: healthpb.NewHealthClient(conn), logs: logs}
}

// fieldViolations returns the BadRequest fields of an INVALID_ARGUMENT
// status, failing the test for any other error.
func fieldViolations(t *testing.T, err error) map[string]string {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != codes.InvalidArgument {
		t.Fatalf("code = %s (%v), want InvalidArgument", st.Code(), err)
	}
	fields := map[string]string{}
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				fields[v.GetField()] = v.GetDescription()
			}
		}
	}
	return fields
}

// export reads a whole CSV export and the number of chunks it came in.
func export(t *testing.T, c ledgerv1.LedgerServiceClient, req *ledgerv1.ExportTransactionsCSVRequest) ([]byte, int, error) {
	t.Helper()
	stream, err := c.ExportTransactionsCSV(context.Background(), req)
	if err != nil {
		return nil, 0, err
	}
	var data []byte
	chunks := 0
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return data, chunks, nil
		}
		if err != nil {
			return nil, chunks, err
		}
		data = append(data, msg.GetChunk()...)
		chunks++
	}
}

func TestUserIDIsRequired(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	ctx := context.Background()
	calls := map[string]func() error{
		"CreateTransaction": func() error {
			_, err := e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{AmountKopecks: 100, Category: "еда"})
			return err
		},
		"ListTransactions": func() error {
			_, err := e.client.ListTransactions(ctx, &ledgerv1.ListTransactionsRequest{UserId: -1})
			return err
		},
		"ExportTransactionsCSV": func() error {
			_, _, err := export(t, e.client, &ledgerv1.ExportTransactionsCSVRequest{})
			return err
		},
		"UpsertBudget": func() error {
			_, err := e.client.UpsertBudget(ctx, &ledgerv1.UpsertBudgetRequest{Category: "еда", LimitKopecks: 100})
			return err
		},
		"ListBudgets": func() error {
			_, err := e.client.ListBudgets(ctx, &ledgerv1.ListBudgetsRequest{})
			return err
		},
		"GetSummary": func() error {
			_, err := e.client.GetSummary(ctx, &ledgerv1.GetSummaryRequest{From: "2026-10-01", To: "2026-10-31"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if fields := fieldViolations(t, call()); fields["user_id"] == "" {
				t.Errorf("violations = %v, want user_id", fields)
			}
		})
	}
}

func TestBadDates(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	ctx := context.Background()

	_, err := e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{UserId: user, AmountKopecks: 100, Category: "еда", Date: "03.10.2026"})
	if fields := fieldViolations(t, err); fields["date"] == "" {
		t.Errorf("create: violations = %v, want date", fields)
	}
	_, err = e.client.ListTransactions(ctx, &ledgerv1.ListTransactionsRequest{UserId: user, Filter: &ledgerv1.TransactionFilter{From: "2026-13-01", To: "tomorrow"}})
	if fields := fieldViolations(t, err); fields["from"] == "" || fields["to"] == "" {
		t.Errorf("list: violations = %v, want from and to", fields)
	}
	_, err = e.client.GetSummary(ctx, &ledgerv1.GetSummaryRequest{UserId: user, From: "2026-10-01", To: "31.10.2026"})
	if fields := fieldViolations(t, err); fields["to"] == "" {
		t.Errorf("summary: violations = %v, want to", fields)
	}
}

func TestDomainErrorsBecomeStatusesWithDetails(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	ctx := context.Background()
	if _, err := e.client.UpsertBudget(ctx, &ledgerv1.UpsertBudgetRequest{UserId: user, Category: "еда", LimitKopecks: 100}); err != nil {
		t.Fatal(err)
	}

	_, err := e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{UserId: user, Category: " "})
	if fields := fieldViolations(t, err); fields["amount"] == "" || fields["category"] == "" {
		t.Errorf("violations = %v, want amount and category", fields)
	}
	_, err = e.client.UpsertBudget(ctx, &ledgerv1.UpsertBudgetRequest{UserId: user, Category: "еда", LimitKopecks: 100, Period: "week"})
	if fields := fieldViolations(t, err); fields["period"] == "" {
		t.Errorf("violations = %v, want period", fields)
	}

	if _, err := e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{UserId: user, AmountKopecks: 60, Category: "еда", ExternalId: "row-1"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{UserId: user, AmountKopecks: 41, Category: "еда"})
	st := status.Convert(err)
	if st.Code() != codes.FailedPrecondition || len(st.Details()) != 1 {
		t.Fatalf("budget: status = %v, details %v", st, st.Details())
	}
	got, ok := st.Details()[0].(*ledgerv1.BudgetExceeded)
	if !ok || got.GetCategory() != "еда" || got.GetPeriod() != "month" || got.GetPeriodLabel() != "2026-10" ||
		got.GetLimitKopecks() != 100 || got.GetSpentKopecks() != 60 || got.GetAmountKopecks() != 41 || got.GetRemainingKopecks() != 40 {
		t.Errorf("budget detail = %v", st.Details()[0])
	}

	_, err = e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{UserId: user, AmountKopecks: 1, Category: "еда", ExternalId: "row-1"})
	st = status.Convert(err)
	if st.Code() != codes.AlreadyExists || len(st.Details()) != 1 {
		t.Fatalf("duplicate: status = %v, details %v", st, st.Details())
	}
	if dup, ok := st.Details()[0].(*ledgerv1.DuplicateTransaction); !ok || dup.GetExternalId() != "row-1" || dup.GetExistingId() != 1 {
		t.Errorf("duplicate detail = %v", st.Details()[0])
	}
}

func TestInternalErrorIsHidden(t *testing.T) {
	t.Parallel()
	repo := &hookRepo{Repo: memory.New(), listBudgets: func(context.Context) ([]domain.Budget, error) {
		return nil, errors.New("password authentication failed for user finance")
	}}
	e := start(t, repo)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-request-id", "req-500")
	_, err := e.client.ListBudgets(ctx, &ledgerv1.ListBudgetsRequest{UserId: user})
	st := status.Convert(err)
	if st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Errorf("status = %v, want INTERNAL with a generic message", st)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "password authentication failed") || !strings.Contains(logs, "req-500") {
		t.Errorf("the cause and the request id are not in the log:\n%s", logs)
	}
}

func TestPanicBecomesInternal(t *testing.T) {
	t.Parallel()
	repo := &hookRepo{
		Repo:             memory.New(),
		listBudgets:      func(context.Context) ([]domain.Budget, error) { panic("boom") },
		listTransactions: func(context.Context) ([]domain.Transaction, error) { panic("boom in a stream") },
	}
	e := start(t, repo)
	ctx := context.Background()

	_, err := e.client.ListBudgets(ctx, &ledgerv1.ListBudgetsRequest{UserId: user})
	if st := status.Convert(err); st.Code() != codes.Internal || st.Message() != "internal error" {
		t.Errorf("unary: status = %v, want INTERNAL", st)
	}
	_, _, err = export(t, e.client, &ledgerv1.ExportTransactionsCSVRequest{UserId: user})
	if st := status.Convert(err); st.Code() != codes.Internal {
		t.Errorf("stream: status = %v, want INTERNAL", st)
	}
	// The server survives and keeps serving.
	if _, err := e.client.UpsertBudget(ctx, &ledgerv1.UpsertBudgetRequest{UserId: user, Category: "еда", LimitKopecks: 100}); err != nil {
		t.Errorf("call after the panics: %v", err)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "panic in rpc handler") || !strings.Contains(logs, "boom in a stream") {
		t.Errorf("panics are not logged:\n%s", logs)
	}
}

func TestExportStreamsInChunks(t *testing.T) {
	t.Parallel()
	repo := memory.New()
	e := start(t, repo)
	ctx := context.Background()
	description := strings.Repeat("д", 100) // 200 bytes in UTF-8
	for range 400 {                         // about 90 KB of CSV, two chunks
		if _, err := e.client.CreateTransaction(ctx, &ledgerv1.CreateTransactionRequest{UserId: user, AmountKopecks: 12345, Category: "еда", Description: description}); err != nil {
			t.Fatal(err)
		}
	}
	data, chunks, err := export(t, e.client, &ledgerv1.ExportTransactionsCSVRequest{UserId: user, Filter: &ledgerv1.TransactionFilter{Category: "ЕДА", Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := usecase.New(repo).ExportCSV(ctx, user, usecase.TransactionFilter{Category: "еда"})
	if err != nil {
		t.Fatal(err)
	}
	if chunks < 2 || !bytes.Equal(data, want) {
		t.Errorf("got %d bytes in %d chunks, want %d bytes in more than one chunk", len(data), chunks, len(want))
	}
}

func TestRequestIDIsLogged(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-request-id", "req-7")
	if _, err := e.client.ListBudgets(ctx, &ledgerv1.ListBudgetsRequest{UserId: user}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.ListBudgets(context.Background(), &ledgerv1.ListBudgetsRequest{UserId: user}); err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for line := range strings.Lines(e.logs.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		if m["msg"] == "rpc" {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("got %d rpc log lines, want 2", len(lines))
	}
	if lines[0]["request_id"] != "req-7" || lines[0]["method"] != "/ledger.v1.LedgerService/ListBudgets" || lines[0]["code"] != "OK" {
		t.Errorf("first line = %v", lines[0])
	}
	if id, _ := lines[1]["request_id"].(string); id == "" || id == "req-7" {
		t.Errorf("a call without an id got request_id %q, want a generated one", id)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	resp, err := e.health.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "ledger.v1.LedgerService"})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("Check = %v, %v; want SERVING", resp, err)
	}
}

func TestShutdownCancelsHangingCalls(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	repo := &hookRepo{Repo: memory.New(), listBudgets: func(ctx context.Context) ([]domain.Budget, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	e := start(t, repo)
	errc := make(chan error, 1)
	go func() {
		_, err := e.client.ListBudgets(context.Background(), &ledgerv1.ListBudgetsRequest{UserId: user})
		errc <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.srv.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after its context ended")
	}
	if err := <-errc; status.Code(err) == codes.OK {
		t.Error("the hanging call succeeded, want it canceled")
	}
}

func TestSummaryCacheHeader(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := usecase.New(memory.New(), usecase.WithReportCache(rediscache.New(rdb, time.Minute), slog.New(slog.DiscardHandler)))
	srv := grpcserver.New(svc, slog.New(slog.DiscardHandler))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := ledgerv1.NewLedgerServiceClient(conn)

	for _, want := range []string{"MISS", "HIT"} {
		var header metadata.MD
		_, err := client.GetSummary(context.Background(), &ledgerv1.GetSummaryRequest{UserId: user, From: "2026-10-01", To: "2026-10-31"}, grpc.Header(&header))
		if err != nil {
			t.Fatal(err)
		}
		if got := header.Get(ledgerv1.CacheHeader); len(got) != 1 || got[0] != want {
			t.Errorf("x-cache = %q, want %s", got, want)
		}
	}
}

func TestNoCacheHeaderWithoutCache(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	var header metadata.MD
	if _, err := e.client.GetSummary(context.Background(), &ledgerv1.GetSummaryRequest{UserId: user, From: "2026-10-01", To: "2026-10-31"}, grpc.Header(&header)); err != nil {
		t.Fatal(err)
	}
	if got := header.Get(ledgerv1.CacheHeader); len(got) != 0 {
		t.Errorf("x-cache = %q without a cache, want none", got)
	}
}

// slowRepo makes every transaction take a while.
type slowRepo struct {
	*memory.Repo
}

func (r slowRepo) InTx(ctx context.Context, fn func(usecase.Tx) error) error {
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.Repo.InTx(ctx, fn)
}

func TestImportStopsBeforeTheDeadlineWithResults(t *testing.T) {
	t.Parallel()
	e := start(t, slowRepo{memory.New()})
	rows := make([]*ledgerv1.ImportRow, 100) // one category: one after another, 2 s in all
	for i := range rows {
		rows[i] = &ledgerv1.ImportRow{Line: int32(i + 2), AmountKopecks: 100, Category: "еда"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	resp, err := e.client.ImportTransactions(ctx, &ledgerv1.ImportTransactionsRequest{UserId: user, Rows: rows})
	if err != nil {
		t.Fatalf("ImportTransactions = %v, want the results of the stored rows rather than a deadline error", err)
	}
	stored, canceled := 0, 0
	for _, r := range resp.GetResults() {
		switch {
		case r.GetId() != 0:
			stored++
		case r.GetError().GetCode() == "canceled":
			canceled++
		default:
			t.Errorf("line %d: %v", r.GetLine(), r.GetError())
		}
	}
	if stored == 0 || canceled == 0 || stored+canceled != len(rows) {
		t.Errorf("%d stored, %d canceled of %d; want both", stored, canceled, len(rows))
	}
}

func TestImportStatusesOfRows(t *testing.T) {
	t.Parallel()
	e := start(t, memory.New())
	ctx := context.Background()
	if _, err := e.client.UpsertBudget(ctx, &ledgerv1.UpsertBudgetRequest{UserId: user, Category: "еда", LimitKopecks: 100}); err != nil {
		t.Fatal(err)
	}
	resp, err := e.client.ImportTransactions(ctx, &ledgerv1.ImportTransactionsRequest{UserId: user, Rows: []*ledgerv1.ImportRow{
		{Line: 2, AmountKopecks: 60, Category: "еда", ExternalId: "a"},
		{Line: 3, AmountKopecks: 60, Category: "еда"},                     // over the budget
		{Line: 4, AmountKopecks: 1, Category: "кафе", ExternalId: "a"},    // repeats line 2
		{Line: 5, AmountKopecks: 1, Category: "кафе", Date: "31.10.2026"}, // bad date
		{Line: 6, AmountKopecks: 1, Category: "кафе", SourceId: 999},      // another user's id
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.GetResults()
	if len(r) != 5 || r[0].GetId() == 0 || r[4].GetId() == 0 {
		t.Fatalf("results = %v", r)
	}
	if e := r[1].GetError(); e.GetCode() != "budget_exceeded" || e.GetBudget().GetRemainingKopecks() != 40 {
		t.Errorf("line 3 = %v", e)
	}
	if e := r[2].GetError(); e.GetCode() != "already_exists" || e.GetExistingId() != r[0].GetId() {
		t.Errorf("line 4 = %v", e)
	}
	if e := r[3].GetError(); e.GetCode() != "validation_error" || e.GetFields()["date"] == "" {
		t.Errorf("line 5 = %v", e)
	}
	if _, err := e.client.ImportTransactions(ctx, &ledgerv1.ImportTransactionsRequest{Rows: nil}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("import without a user: %v", err)
	}
}
