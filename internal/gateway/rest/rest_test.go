package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// fakeLedger implements ledgerclient.Client with per-test functions.
type fakeLedger struct {
	create      func(ctx context.Context, userID int64, in ledgerclient.TransactionInput) (ledgerclient.Transaction, error)
	list        func(ctx context.Context, userID int64, f ledgerclient.TransactionFilter) ([]ledgerclient.Transaction, error)
	export      func(ctx context.Context, userID int64, f ledgerclient.TransactionFilter) ([]byte, error)
	importRows  func(ctx context.Context, userID int64, rows []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error)
	upsert      func(ctx context.Context, userID int64, in ledgerclient.BudgetInput) (ledgerclient.Budget, bool, error)
	listBudgets func(ctx context.Context, userID int64) ([]ledgerclient.Budget, error)
	summary     func(ctx context.Context, userID int64, from, to civil.Date) (ledgerclient.Summary, error)
}

var errNotStubbed = errors.New("not stubbed")

func (f *fakeLedger) CreateTransaction(ctx context.Context, userID int64, in ledgerclient.TransactionInput) (ledgerclient.Transaction, error) {
	if f.create == nil {
		return ledgerclient.Transaction{}, errNotStubbed
	}
	return f.create(ctx, userID, in)
}

func (f *fakeLedger) ListTransactions(ctx context.Context, userID int64, fl ledgerclient.TransactionFilter) ([]ledgerclient.Transaction, error) {
	if f.list == nil {
		return nil, errNotStubbed
	}
	return f.list(ctx, userID, fl)
}

func (f *fakeLedger) ExportTransactionsCSV(ctx context.Context, userID int64, fl ledgerclient.TransactionFilter) ([]byte, error) {
	if f.export == nil {
		return nil, errNotStubbed
	}
	return f.export(ctx, userID, fl)
}

func (f *fakeLedger) ImportTransactions(ctx context.Context, userID int64, rows []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
	if f.importRows == nil {
		return nil, errNotStubbed
	}
	return f.importRows(ctx, userID, rows)
}

func (f *fakeLedger) UpsertBudget(ctx context.Context, userID int64, in ledgerclient.BudgetInput) (ledgerclient.Budget, bool, error) {
	if f.upsert == nil {
		return ledgerclient.Budget{}, false, errNotStubbed
	}
	return f.upsert(ctx, userID, in)
}

func (f *fakeLedger) ListBudgets(ctx context.Context, userID int64) ([]ledgerclient.Budget, error) {
	if f.listBudgets == nil {
		return nil, errNotStubbed
	}
	return f.listBudgets(ctx, userID)
}

func (f *fakeLedger) Summary(ctx context.Context, userID int64, from, to civil.Date) (ledgerclient.Summary, error) {
	if f.summary == nil {
		return ledgerclient.Summary{}, errNotStubbed
	}
	return f.summary(ctx, userID, from, to)
}

// fakeAuth implements authclient.Client with per-test functions.
type fakeAuth struct {
	register func(ctx context.Context, email, password string) (authclient.Session, error)
	login    func(ctx context.Context, email, password string) (authclient.Session, error)
}

func (f *fakeAuth) Register(ctx context.Context, email, password string) (authclient.Session, error) {
	if f.register == nil {
		return authclient.Session{}, errNotStubbed
	}
	return f.register(ctx, email, password)
}

func (f *fakeAuth) Login(ctx context.Context, email, password string) (authclient.Session, error) {
	if f.login == nil {
		return authclient.Session{}, errNotStubbed
	}
	return f.login(ctx, email, password)
}

// fakeTokens accepts the tokens it lists, each issued for a user.
type fakeTokens map[string]int64

func (f fakeTokens) Verify(token string) (int64, error) {
	id, ok := f[token]
	if !ok {
		return 0, errors.New("unknown token")
	}
	return id, nil
}

const (
	testUser  int64 = 42
	testToken       = "token-of-42"
	otherUser int64 = 7
)

func newHandler(ledger *fakeLedger) http.Handler {
	return newHandlerWithAuth(ledger, &fakeAuth{})
}

func newHandlerWithAuth(ledger *fakeLedger, auth *fakeAuth) http.Handler {
	return NewHandler(Deps{
		Ledger:  ledger,
		Auth:    auth,
		Tokens:  fakeTokens{testToken: testUser, "token-of-7": otherUser},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenAPI: []byte("openapi: 3.0.3\n"),
		Docs:    []byte("<html></html>"),
	})
}

// do sends a request and returns the response with its body. It sends the
// token of testUser unless headers set Authorization themselves.
func do(t *testing.T, h http.Handler, method, target, body string, headers ...string) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Header.Set("Authorization", "Bearer "+testToken)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// apiError is the shape of every error response.
type apiError struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeError(t *testing.T, res *http.Response, body string, wantStatus int, wantCode string) apiError {
	t.Helper()
	if res.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d; body: %s", res.StatusCode, wantStatus, body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var e apiError
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, body)
	}
	if e.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q; body: %s", e.Error.Code, wantCode, body)
	}
	if e.Error.Message == "" {
		t.Fatal("error message is empty")
	}
	return e
}

func TestPingAndRouting(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeLedger{})

	res, body := do(t, h, http.MethodGet, "/ping", "")
	if res.StatusCode != http.StatusOK || body != "pong" || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("GET /ping = %d %q %q", res.StatusCode, body, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID is not set on the response")
	}

	res, body = do(t, h, http.MethodPost, "/ping", "")
	decodeError(t, res, body, http.StatusMethodNotAllowed, codeMethodNotAllowed)
	if res.Header.Get("Allow") != "GET" {
		t.Errorf("Allow = %q, want GET", res.Header.Get("Allow"))
	}

	res, body = do(t, h, http.MethodDelete, "/api/transactions", "")
	decodeError(t, res, body, http.StatusMethodNotAllowed, codeMethodNotAllowed)
	if res.Header.Get("Allow") != "GET, POST" {
		t.Errorf("Allow = %q, want \"GET, POST\"", res.Header.Get("Allow"))
	}

	res, body = do(t, h, http.MethodGet, "/nope", "")
	decodeError(t, res, body, http.StatusNotFound, codeNotFound)

	res, body = do(t, h, http.MethodGet, "/openapi.yaml", "")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, "openapi:") {
		t.Errorf("GET /openapi.yaml = %d %q", res.StatusCode, body)
	}
	res, _ = do(t, h, http.MethodGet, "/docs", "")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
		t.Errorf("GET /docs = %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}

	res, _ = do(t, h, http.MethodGet, "/ping", "", "X-Request-ID", "client-123")
	if res.Header.Get("X-Request-ID") != "client-123" {
		t.Errorf("X-Request-ID = %q, want the client's id echoed", res.Header.Get("X-Request-ID"))
	}
}

func TestCreateTransaction(t *testing.T) {
	t.Parallel()
	var got ledgerclient.TransactionInput
	var gotUser int64
	ledger := &fakeLedger{create: func(_ context.Context, userID int64, in ledgerclient.TransactionInput) (ledgerclient.Transaction, error) {
		got, gotUser = in, userID
		return ledgerclient.Transaction{ID: 1, Amount: in.Amount, Category: "еда", Description: in.Description, Date: civil.New(2026, time.October, 3), CreatedAt: time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)}, nil
	}}
	h := newHandler(ledger)

	res, body := do(t, h, http.MethodPost, "/api/transactions",
		`{"amount":"1250,50","category":"Еда","description":"продукты","date":"2026-10-03","external_id":"row-1"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if gotUser != testUser {
		t.Errorf("user id passed to the ledger = %d, want %d", gotUser, testUser)
	}
	want := ledgerclient.TransactionInput{Amount: 125050, Category: "Еда", Description: "продукты", Date: civil.New(2026, time.October, 3), ExternalID: "row-1"}
	if got != want {
		t.Errorf("input passed to the ledger = %+v, want %+v", got, want)
	}
	if !strings.Contains(body, `"amount":1250.50`) || !strings.Contains(body, `"date":"2026-10-03"`) || !strings.Contains(body, `"id":1`) {
		t.Errorf("body = %s", body)
	}
}

func TestCreateTransactionErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		ledgerErr  error
		wantStatus int
		wantCode   string
		wantDetail string // a key that must be present in details
	}{
		{name: "broken json", body: `{"amount":`, wantStatus: 400, wantCode: codeInvalidJSON},
		{name: "unknown field", body: `{"amount":1,"category":"еда","ammount":2}`, wantStatus: 400, wantCode: codeInvalidJSON},
		{name: "bad amount text", body: `{"amount":"abc","category":"еда"}`, wantStatus: 400, wantCode: codeInvalidJSON},
		{name: "bad date", body: `{"amount":1,"category":"еда","date":"03.10.2026"}`, wantStatus: 400, wantCode: codeInvalidJSON},
		{name: "trailing data", body: `{"amount":1,"category":"еда"} {}`, wantStatus: 400, wantCode: codeInvalidJSON},
		{name: "empty body", body: ``, wantStatus: 400, wantCode: codeInvalidJSON},
		{name: "zero amount and empty category", body: `{"amount":0,"category":" "}`, wantStatus: 400, wantCode: codeValidation, wantDetail: "amount"},
		{name: "ledger validation", body: `{"amount":1,"category":"еда"}`, ledgerErr: &upstream.ValidationError{Fields: map[string]string{"description": "too long"}}, wantStatus: 400, wantCode: codeValidation, wantDetail: "description"},
		{name: "budget exceeded", body: `{"amount":500,"category":"еда"}`, ledgerErr: &ledgerclient.BudgetExceededError{Category: "еда", Period: "month", PeriodLabel: "2026-10", Limit: 500000, Spent: 475050, Amount: 50000, Remaining: 24950}, wantStatus: 409, wantCode: codeBudgetExceeded, wantDetail: "remaining"},
		{name: "duplicate", body: `{"amount":1,"category":"еда","external_id":"row-1"}`, ledgerErr: &ledgerclient.DuplicateError{ExternalID: "row-1", ExistingID: 7}, wantStatus: 409, wantCode: codeAlreadyExists, wantDetail: "id"},
		{name: "invalid argument without fields", body: `{"amount":1,"category":"еда"}`, ledgerErr: fmt.Errorf("%w: bad", upstream.ErrInvalidArgument), wantStatus: 400, wantCode: codeInvalidRequest},
		{name: "conflict without details", body: `{"amount":1,"category":"еда"}`, ledgerErr: fmt.Errorf("%w: busy", upstream.ErrConflict), wantStatus: 409, wantCode: codeConflict},
		{name: "not found", body: `{"amount":1,"category":"еда"}`, ledgerErr: fmt.Errorf("%w: no such thing", upstream.ErrNotFound), wantStatus: 404, wantCode: codeNotFound},
		{name: "ledger down", body: `{"amount":1,"category":"еда"}`, ledgerErr: &upstream.UnavailableError{Service: "ledger", Reason: "connection refused"}, wantStatus: 502, wantCode: codeLedgerDown},
		{name: "ledger timeout", body: `{"amount":1,"category":"еда"}`, ledgerErr: context.DeadlineExceeded, wantStatus: 504, wantCode: codeTimeout},
		{name: "client went away", body: `{"amount":1,"category":"еда"}`, ledgerErr: context.Canceled, wantStatus: statusClientClosedRequest, wantCode: codeClientClosed},
		{name: "unknown error", body: `{"amount":1,"category":"еда"}`, ledgerErr: errors.New("disk on fire"), wantStatus: 500, wantCode: codeInternal, wantDetail: "request_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ledger := &fakeLedger{create: func(context.Context, int64, ledgerclient.TransactionInput) (ledgerclient.Transaction, error) {
				return ledgerclient.Transaction{}, tc.ledgerErr
			}}
			res, body := do(t, newHandler(ledger), http.MethodPost, "/api/transactions", tc.body)
			e := decodeError(t, res, body, tc.wantStatus, tc.wantCode)
			if tc.wantDetail != "" {
				if _, ok := e.Error.Details[tc.wantDetail]; !ok {
					t.Errorf("details = %v, want key %q", e.Error.Details, tc.wantDetail)
				}
			}
			if tc.wantCode == codeInternal && strings.Contains(body, "disk on fire") {
				t.Error("internal error details leaked into the response")
			}
		})
	}
}

func TestPayloadTooLarge(t *testing.T) {
	t.Parallel()
	body := `{"amount":1,"category":"еда","description":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	res, got := do(t, newHandler(&fakeLedger{}), http.MethodPost, "/api/transactions", body)
	decodeError(t, res, got, http.StatusRequestEntityTooLarge, codePayloadTooLarge)
}

func TestListAndExportTransactions(t *testing.T) {
	t.Parallel()
	var gotFilter ledgerclient.TransactionFilter
	ledger := &fakeLedger{
		list: func(_ context.Context, _ int64, f ledgerclient.TransactionFilter) ([]ledgerclient.Transaction, error) {
			gotFilter = f
			if f.Category == "none" {
				return nil, nil
			}
			return []ledgerclient.Transaction{{ID: 1, Amount: 100, Category: "еда", Date: civil.New(2026, time.October, 3)}}, nil
		},
		export: func(context.Context, int64, ledgerclient.TransactionFilter) ([]byte, error) {
			return []byte("id,date,category,amount,description\n1,2026-10-03,еда,1.00,\n"), nil
		},
	}
	h := newHandler(ledger)

	res, body := do(t, h, http.MethodGet, "/api/transactions?from=2026-10-01&to=2026-10-31&category=еда&limit=5&offset=10", "")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(body, `{"items":[{"id":1`) {
		t.Errorf("list = %d %s", res.StatusCode, body)
	}
	want := ledgerclient.TransactionFilter{From: civil.New(2026, time.October, 1), To: civil.New(2026, time.October, 31), Category: "еда", Limit: 5, Offset: 10}
	if gotFilter != want {
		t.Errorf("filter = %+v, want %+v", gotFilter, want)
	}

	res, body = do(t, h, http.MethodGet, "/api/transactions?category=none", "")
	if res.StatusCode != http.StatusOK || strings.TrimSpace(body) != `{"items":[]}` {
		t.Errorf("empty list = %d %s, want {\"items\":[]}", res.StatusCode, body)
	}

	for _, bad := range []string{"from=03.10.2026", "from=2026-10-31&to=2026-10-01", "limit=-1", "offset=x"} {
		res, body = do(t, h, http.MethodGet, "/api/transactions?"+bad, "")
		decodeError(t, res, body, http.StatusBadRequest, codeValidation)
	}

	res, body = do(t, h, http.MethodGet, "/api/transactions/export.csv?from=2026-10-01", "")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(res.Header.Get("Content-Disposition"), "transactions.csv") || !strings.HasPrefix(body, "id,date,category") {
		t.Errorf("export = %d %q %q %s", res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Content-Disposition"), body)
	}
}

func TestBudgets(t *testing.T) {
	t.Parallel()
	ledger := &fakeLedger{
		upsert: func(_ context.Context, _ int64, in ledgerclient.BudgetInput) (ledgerclient.Budget, bool, error) {
			period := in.Period
			if period == "" {
				period = "month"
			}
			return ledgerclient.Budget{Category: strings.ToLower(in.Category), Limit: in.Limit, Period: period}, in.Period == "", nil
		},
		listBudgets: func(context.Context, int64) ([]ledgerclient.Budget, error) {
			return []ledgerclient.Budget{{Category: "еда", Limit: 500000, Period: "month"}}, nil
		},
	}
	h := newHandler(ledger)

	res, body := do(t, h, http.MethodPost, "/api/budget", `{"category":"Еда","limit":5000}`)
	if res.StatusCode != http.StatusCreated || strings.TrimSpace(body) != `{"category":"еда","limit":5000.00,"period":"month"}` {
		t.Errorf("create = %d %s", res.StatusCode, body)
	}
	res, body = do(t, h, http.MethodPost, "/api/budget", `{"category":"еда","limit":"100,50","period":"none"}`)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"limit":100.50`) {
		t.Errorf("update = %d %s", res.StatusCode, body)
	}
	res, body = do(t, h, http.MethodPost, "/api/budget", `{"category":"","limit":0}`)
	e := decodeError(t, res, body, http.StatusBadRequest, codeValidation)
	if _, ok := e.Error.Details["limit"]; !ok {
		t.Errorf("details = %v, want limit", e.Error.Details)
	}
	if _, ok := e.Error.Details["category"]; !ok {
		t.Errorf("details = %v, want category", e.Error.Details)
	}
	res, body = do(t, h, http.MethodGet, "/api/budget", "")
	if res.StatusCode != http.StatusOK || strings.TrimSpace(body) != `{"items":[{"category":"еда","limit":5000.00,"period":"month"}]}` {
		t.Errorf("list = %d %s", res.StatusCode, body)
	}
}

func TestBudgetRouting(t *testing.T) {
	t.Parallel()
	h := newHandler(&fakeLedger{})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, token := range []string{"", "Bearer " + testToken} {
			res, body := do(t, h, method, "/api/budgets", `{"category":"food","limit":100}`, "Authorization", token)
			decodeError(t, res, body, http.StatusNotFound, codeNotFound)
		}
	}
	res, body := do(t, h, http.MethodDelete, "/api/budget", "")
	decodeError(t, res, body, http.StatusMethodNotAllowed, codeMethodNotAllowed)
	if res.Header.Get("Allow") != "GET, POST" {
		t.Errorf("Allow = %q, want GET, POST", res.Header.Get("Allow"))
	}
}

func TestSummary(t *testing.T) {
	t.Parallel()
	var gotFrom, gotTo civil.Date
	ledger := &fakeLedger{summary: func(_ context.Context, _ int64, from, to civil.Date) (ledgerclient.Summary, error) {
		gotFrom, gotTo = from, to
		return ledgerclient.Summary{From: from, To: to, Total: 545050, Categories: []ledgerclient.CategorySummary{
			{Category: "еда", Spent: 475050, Budget: &ledgerclient.BudgetUsage{Period: "month", Limit: 500000, Remaining: 24950, Percent: 95.01}},
			{Category: "здоровье", Spent: 70000},
		}}, nil
	}}
	h := newHandler(ledger)

	res, body := do(t, h, http.MethodGet, "/api/reports/summary?from=2026-10-01&to=2026-10-31", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.StatusCode, body)
	}
	if !gotFrom.Equal(civil.New(2026, time.October, 1)) || !gotTo.Equal(civil.New(2026, time.October, 31)) {
		t.Errorf("dates passed = %s..%s", gotFrom, gotTo)
	}
	for _, want := range []string{`"total":5450.50`, `"percent":95.01`, `"budget":null`, `"remaining":249.50`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}

	res, body = do(t, h, http.MethodGet, "/api/reports/summary?from=2026-10-01", "")
	e := decodeError(t, res, body, http.StatusBadRequest, codeValidation)
	if e.Error.Details["to"] != "is required" {
		t.Errorf("details = %v", e.Error.Details)
	}
	res, body = do(t, h, http.MethodGet, "/api/reports/summary?from=2026-10-31&to=2026-10-01", "")
	decodeError(t, res, body, http.StatusBadRequest, codeValidation)
	res, body = do(t, h, http.MethodGet, "/api/reports/summary?from=x&to=2026-10-01", "")
	decodeError(t, res, body, http.StatusBadRequest, codeValidation)
}

func TestPanicBecomes500(t *testing.T) {
	t.Parallel()
	ledger := &fakeLedger{listBudgets: func(context.Context, int64) ([]ledgerclient.Budget, error) {
		panic("boom")
	}}
	res, body := do(t, newHandler(ledger), http.MethodGet, "/api/budget", "")
	decodeError(t, res, body, http.StatusInternalServerError, codeInternal)
}

// TestRecovererStandsAlone checks the recoverer without the logging
// middleware around it: it must still answer 500 once and leave a response
// that was already started alone.
func TestRecovererStandsAlone(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	res, body := do(t, h, http.MethodGet, "/x", "")
	decodeError(t, res, body, http.StatusInternalServerError, codeInternal)

	started := recoverer(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "partial")
		panic("boom after writing")
	}))
	res, body = do(t, started, http.MethodGet, "/x", "")
	if res.StatusCode != http.StatusAccepted || body != "partial" {
		t.Errorf("response after a late panic = %d %q, want the partial 202 untouched", res.StatusCode, body)
	}
}

func TestMoneyInResponsesIsExact(t *testing.T) {
	t.Parallel()
	ledger := &fakeLedger{listBudgets: func(context.Context, int64) ([]ledgerclient.Budget, error) {
		return []ledgerclient.Budget{{Category: "x", Limit: money.MustParse("0.1") + money.MustParse("0.2"), Period: "month"}}, nil
	}}
	_, body := do(t, newHandler(ledger), http.MethodGet, "/api/budget", "")
	if !strings.Contains(body, `"limit":0.30`) {
		t.Errorf("0.1 + 0.2 rendered as %s", body)
	}
}

func TestSummaryCacheHeader(t *testing.T) {
	t.Parallel()
	for cache, want := range map[string]string{"HIT": "HIT", "MISS": "MISS", "": ""} {
		ledger := &fakeLedger{summary: func(context.Context, int64, civil.Date, civil.Date) (ledgerclient.Summary, error) {
			return ledgerclient.Summary{Cache: cache}, nil
		}}
		res, body := do(t, newHandler(ledger), http.MethodGet, "/api/reports/summary?from=2026-10-01&to=2026-10-31", "")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", res.StatusCode, body)
		}
		if got := res.Header.Get("X-Cache"); got != want {
			t.Errorf("cache %q: X-Cache = %q, want %q", cache, got, want)
		}
		if strings.Contains(body, "cache") {
			t.Errorf("the cache status leaked into the body: %s", body)
		}
	}
}

// The category filter is free text, so it gets the checks of every text
// field before the call: a string field with invalid UTF-8 cannot even be
// marshaled for gRPC, and the ledger rejects NUL. Both routes share the
// filter, valid text reaches the ledger unchanged and no filter means all.
func TestTransactionFilterRejectsBadText(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var gotFilter ledgerclient.TransactionFilter
	ledger := &fakeLedger{
		list: func(_ context.Context, _ int64, f ledgerclient.TransactionFilter) ([]ledgerclient.Transaction, error) {
			calls.Add(1)
			gotFilter = f
			return []ledgerclient.Transaction{}, nil
		},
		export: func(_ context.Context, _ int64, f ledgerclient.TransactionFilter) ([]byte, error) {
			calls.Add(1)
			gotFilter = f
			return []byte("id,date,category,amount,description,external_id\n"), nil
		},
	}
	h := newHandler(ledger)
	for _, path := range []string{"/api/transactions", "/api/transactions/export.csv"} {
		for _, raw := range []string{"%FF", "%C3%28", "%00", "%D0%B5%D0%B4%D0%B0%00"} {
			before := calls.Load()
			res, body := do(t, h, http.MethodGet, path+"?category="+raw, "")
			e := decodeError(t, res, body, http.StatusBadRequest, codeValidation)
			if e.Error.Details["category"] != "must be valid UTF-8 without NUL characters" {
				t.Errorf("GET %s?category=%s: details = %v", path, raw, e.Error.Details)
			}
			if calls.Load() != before {
				t.Errorf("GET %s?category=%s reached the ledger", path, raw)
			}
		}
		for raw, want := range map[string]string{"": "", "%D0%95%D0%B4%D0%B0": "Еда", "%D0%B5%D0%B4%D0%B0+%D0%B8+%D0%BA%D0%BE%D1%84%D0%B5": "еда и кофе"} {
			before := calls.Load()
			res, body := do(t, h, http.MethodGet, path+"?category="+raw, "")
			if res.StatusCode != http.StatusOK || calls.Load() != before+1 || gotFilter.Category != want {
				t.Errorf("GET %s?category=%s = %d %s with filter category %q, want 200 and %q", path, raw, res.StatusCode, body, gotFilter.Category, want)
			}
		}
	}
}
