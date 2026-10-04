package rest

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

// dateParam parses an optional YYYY-MM-DD query parameter.
func dateParam(q url.Values, name string, errs fieldErrors) civil.Date {
	raw := q.Get(name)
	if raw == "" {
		return civil.Date{}
	}
	d, err := civil.Parse(raw)
	if err != nil {
		errs.add(name, "must be a date in the form YYYY-MM-DD")
	}
	return d
}

// intParam parses an optional non-negative integer query parameter.
func intParam(q url.Values, name string, errs fieldErrors) int {
	raw := q.Get(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		errs.add(name, "must be a non-negative integer")
		return 0
	}
	return n
}

// textParam reads an optional free-text query parameter. Text the ledger
// cannot store or compare is rejected here, like in a JSON body or a CSV
// row: a string with invalid UTF-8 cannot even be marshaled for gRPC, and
// without this check that failure came back as an internal error.
func textParam(q url.Values, name string, errs fieldErrors) string {
	raw := q.Get(name)
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		errs.add(name, "must be valid UTF-8 without NUL characters")
	}
	return raw
}

// transactionFilter reads the common from, to, category, limit and offset
// parameters and validates their form.
func transactionFilter(q url.Values, errs fieldErrors) ledgerclient.TransactionFilter {
	f := ledgerclient.TransactionFilter{
		From:     dateParam(q, "from", errs),
		To:       dateParam(q, "to", errs),
		Category: textParam(q, "category", errs),
		Limit:    intParam(q, "limit", errs),
		Offset:   intParam(q, "offset", errs),
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.To.Before(f.From) {
		errs.add("to", "must not be before from")
	}
	return f
}
