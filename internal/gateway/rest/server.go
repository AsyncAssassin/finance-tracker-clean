// Package rest is the HTTP API of the gateway. Handlers validate requests,
// call the services through their clients and translate their errors into
// HTTP status codes with one JSON error format. Every /api route except
// /api/auth/* needs an access token, and the ledger is called on behalf of
// the user the token was issued for.
package rest

import (
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
)

// TokenVerifier checks an access token and returns the user it was issued
// for; jwtauth.Verifier implements it.
type TokenVerifier interface {
	Verify(token string) (userID int64, err error)
}

// Deps is what the HTTP API needs to run.
type Deps struct {
	Ledger  ledgerclient.Client
	Auth    authclient.Client
	Tokens  TokenVerifier
	Log     *slog.Logger
	OpenAPI []byte // the OpenAPI document served at /openapi.yaml
	Docs    []byte // the HTML page served at /docs
	// ImportTimeout bounds each phase of an import on its own: reading the
	// body, then the ledger call, which the ledger client limits to the
	// same value; the write deadline is extended past each phase. Zero
	// means ledgerclient.DefaultImportTimeout.
	ImportTimeout time.Duration
}

type server struct {
	ledger        ledgerclient.Client
	auth          authclient.Client
	tokens        TokenVerifier
	log           *slog.Logger
	openapi       []byte
	docs          []byte
	importTimeout time.Duration
	// responseGrace is how long a client has to receive an answer after
	// the work it waited for; the import extends its write deadline by it.
	responseGrace time.Duration
}

// defaultResponseGrace is the margin for writing an answer after the work
// it waited for, the same one cmd/gateway adds to the server's WriteTimeout.
const defaultResponseGrace = 20 * time.Second

// NewHandler builds the HTTP handler of the gateway with its middleware.
func NewHandler(d Deps) http.Handler {
	return newServer(d).handler()
}

func newServer(d Deps) *server {
	s := &server{ledger: d.Ledger, auth: d.Auth, tokens: d.Tokens, log: d.Log, openapi: d.OpenAPI, docs: d.Docs,
		importTimeout: d.ImportTimeout, responseGrace: defaultResponseGrace}
	if s.importTimeout <= 0 {
		s.importTimeout = ledgerclient.DefaultImportTimeout
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// handler builds the routes of s with their middleware.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	route(mux, "/ping", map[string]http.HandlerFunc{"GET": s.ping})
	route(mux, "/openapi.yaml", map[string]http.HandlerFunc{"GET": s.openAPI})
	route(mux, "/docs", map[string]http.HandlerFunc{"GET": s.docsPage})
	route(mux, "/api/auth/register", map[string]http.HandlerFunc{"POST": s.register})
	route(mux, "/api/auth/login", map[string]http.HandlerFunc{"POST": s.login})
	// The ledger routes act on behalf of the token's user.
	auth := s.authenticated
	route(mux, "/api/transactions", map[string]http.HandlerFunc{"POST": auth(s.createTransaction), "GET": auth(s.listTransactions)})
	route(mux, "/api/transactions/export.csv", map[string]http.HandlerFunc{"GET": auth(s.exportTransactions)})
	route(mux, "/api/transactions/import", map[string]http.HandlerFunc{"POST": auth(oneImportPerUser(s.importTransactions))})
	route(mux, "/api/budget", map[string]http.HandlerFunc{"POST": auth(s.upsertBudget), "GET": auth(s.listBudgets)})
	route(mux, "/api/budgets", map[string]http.HandlerFunc{"POST": auth(s.upsertBudget), "GET": auth(s.listBudgets)})
	route(mux, "/api/reports/summary", map[string]http.HandlerFunc{"GET": auth(s.summary)})
	mux.HandleFunc("/", s.notFound)

	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeMux redirects unclean paths with a body that can hold admission
		// while an unfinished request body is drained. Keep rejection bounded.
		escapedPath := r.URL.EscapedPath()
		if strings.TrimSuffix(escapedPath, "/") != strings.TrimSuffix(path.Clean(escapedPath), "/") {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, "path must not contain repeated slashes or dot segments", nil)
			return
		}
		mux.ServeHTTP(w, r)
	})
	h = admission(h)
	h = recoverer(s.log)(h)
	h = logRequests(s.log)(h)
	h = requestID(h)
	h = responseHeaders(h)
	return h
}

// route registers one handler per method for path and a JSON 405 for the
// other methods. ServeMux prefers the method-specific patterns.
func route(mux *http.ServeMux, path string, handlers map[string]http.HandlerFunc) {
	methods := make([]string, 0, len(handlers))
	for method, h := range handlers {
		methods = append(methods, method)
		mux.HandleFunc(method+" "+path, h)
	}
	slices.Sort(methods)
	allow := strings.Join(methods, ", ")
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			"method not allowed; use "+allow, nil)
	})
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, codeNotFound, "route not found", nil)
}
