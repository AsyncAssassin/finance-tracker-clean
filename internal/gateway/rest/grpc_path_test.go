package rest

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	ledgergrpc "github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient/grpcclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
)

// newHandlerOverGRPC builds the HTTP handler over the real gRPC client and
// the real ledger server with an in-memory repository, so a request
// crosses the wire the way it does in the running stack. A fake client
// cannot show what the gRPC marshaling rejects.
func newHandlerOverGRPC(t *testing.T) http.Handler {
	t.Helper()
	srv := grpcserver.New(usecase.New(memory.New()), slog.New(slog.DiscardHandler))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	ledger, err := ledgergrpc.New("passthrough:///bufnet", 5*time.Second,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return NewHandler(Deps{
		Ledger: ledger,
		Auth:   &fakeAuth{},
		Tokens: fakeTokens{testToken: testUser},
		Log:    slog.New(slog.DiscardHandler),
	})
}

// A category filter with invalid UTF-8 used to reach the gRPC client,
// whose marshaling failed, and the failure came back as 500 instead of
// the 400 every other bad input gets. Both routes
// must reject it before the call and keep filtering valid text.
func TestTransactionFilterOverGRPC(t *testing.T) {
	t.Parallel()
	h := newHandlerOverGRPC(t)
	res, body := do(t, h, http.MethodPost, "/api/transactions",
		`{"amount":"1250,50","category":"Еда","description":"продукты","date":"2026-10-03"}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	for _, path := range []string{"/api/transactions", "/api/transactions/export.csv"} {
		for _, raw := range []string{"%FF", "%C3%28", "%00"} {
			res, body := do(t, h, http.MethodGet, path+"?category="+raw, "")
			e := decodeError(t, res, body, http.StatusBadRequest, codeValidation)
			if e.Error.Details["category"] != "must be valid UTF-8 without NUL characters" {
				t.Errorf("GET %s?category=%s: details = %v", path, raw, e.Error.Details)
			}
		}
		// The ledger normalizes the filter like a stored category, so
		// "еда" finds "Еда"; another category finds nothing; no filter
		// finds everything.
		for raw, found := range map[string]bool{"%D0%B5%D0%B4%D0%B0": true, "%D1%82%D1%80%D0%B0%D0%BD%D1%81%D0%BF%D0%BE%D1%80%D1%82": false, "": true} {
			res, body := do(t, h, http.MethodGet, path+"?category="+raw, "")
			if res.StatusCode != http.StatusOK || strings.Contains(body, "продукты") != found {
				t.Errorf("GET %s?category=%s = %d %s, want 200 with the transaction found=%v", path, raw, res.StatusCode, body, found)
			}
		}
	}
}
