package rest

import (
	"net/http"
	"strings"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// createTransactionRequest is the body of POST /api/transactions.
type createTransactionRequest struct {
	Amount      money.Money `json:"amount"`
	Category    string      `json:"category"`
	Description string      `json:"description"`
	Date        civil.Date  `json:"date"`
	ExternalID  string      `json:"external_id"`
}

// listResponse wraps a list, so the shape can grow (paging) without
// breaking clients.
type listResponse[T any] struct {
	Items []T `json:"items"`
}

func (s *server) createTransaction(w http.ResponseWriter, r *http.Request) {
	var req createTransactionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// The gateway checks the shape of a request (required fields, formats)
	// and fails fast before the call to the ledger; the ledger stays the
	// source of truth for the business rules and their limits.
	errs := fieldErrors{}
	if req.Amount <= 0 {
		errs.add("amount", "must be greater than 0")
	}
	if strings.TrimSpace(req.Category) == "" {
		errs.add("category", "must not be empty")
	}
	if errs.respond(w) {
		return
	}
	tx, err := s.ledger.CreateTransaction(r.Context(), userIDFrom(r.Context()), ledgerclient.TransactionInput{
		Amount:      req.Amount,
		Category:    req.Category,
		Description: req.Description,
		Date:        req.Date,
		ExternalID:  req.ExternalID,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, tx)
}

func (s *server) listTransactions(w http.ResponseWriter, r *http.Request) {
	errs := fieldErrors{}
	f := transactionFilter(r.URL.Query(), errs)
	if errs.respond(w) {
		return
	}
	list, err := s.ledger.ListTransactions(r.Context(), userIDFrom(r.Context()), f)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if list == nil {
		list = []ledgerclient.Transaction{}
	}
	writeJSON(w, http.StatusOK, listResponse[ledgerclient.Transaction]{Items: list})
}

func (s *server) exportTransactions(w http.ResponseWriter, r *http.Request) {
	errs := fieldErrors{}
	f := transactionFilter(r.URL.Query(), errs)
	if errs.respond(w) {
		return
	}
	data, err := s.ledger.ExportTransactionsCSV(r.Context(), userIDFrom(r.Context()), f)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="transactions.csv"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
