package rest

import (
	"net/http"
)

func (s *server) summary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	errs := fieldErrors{}
	from := dateParam(q, "from", errs)
	to := dateParam(q, "to", errs)
	if q.Get("from") == "" {
		errs.add("from", "is required")
	}
	if q.Get("to") == "" {
		errs.add("to", "is required")
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		errs.add("to", "must not be before from")
	}
	if errs.respond(w) {
		return
	}
	report, err := s.ledger.Summary(r.Context(), userIDFrom(r.Context()), from, to)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	if report.Cache != "" {
		w.Header().Set("X-Cache", report.Cache) // HIT or MISS, to see the report cache at work
	}
	writeJSON(w, http.StatusOK, report)
}
