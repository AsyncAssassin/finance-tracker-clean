package rest

import (
	"net/http"
	"strings"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// upsertBudgetRequest is the body of POST /api/budget.
type upsertBudgetRequest struct {
	Category string      `json:"category"`
	Limit    money.Money `json:"limit"`
	Period   string      `json:"period"`
}

func (s *server) upsertBudget(w http.ResponseWriter, r *http.Request) {
	var req upsertBudgetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	errs := fieldErrors{}
	if req.Limit <= 0 {
		errs.add("limit", "must be greater than 0")
	}
	if strings.TrimSpace(req.Category) == "" {
		errs.add("category", "must not be empty")
	}
	if errs.respond(w) {
		return
	}
	b, created, err := s.ledger.UpsertBudget(r.Context(), userIDFrom(r.Context()), ledgerclient.BudgetInput{
		Category: req.Category,
		Limit:    req.Limit,
		Period:   req.Period,
	})
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, b)
}

func (s *server) listBudgets(w http.ResponseWriter, r *http.Request) {
	list, err := s.ledger.ListBudgets(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if list == nil {
		list = []ledgerclient.Budget{}
	}
	writeJSON(w, http.StatusOK, listResponse[ledgerclient.Budget]{Items: list})
}
