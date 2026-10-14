package rest

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/csvimport"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
)

// importResponse is the answer to POST /api/transactions/import: counts and
// the outcome of every data row of the file, by line.
type importResponse struct {
	Total   int               `json:"total"`
	Created int               `json:"created"`
	Failed  int               `json:"failed"`
	Rows    []importRowResult `json:"rows"`
}

type importRowResult struct {
	Line   int        `json:"line"`
	Status string     `json:"status"` // "created" or "failed"
	ID     int64      `json:"id,omitempty"`
	Error  *errorInfo `json:"error,omitempty"`
}

// csvTypes are the media types accepted as a CSV body.
var csvTypes = []string{"text/csv", "text/plain", "application/csv"}

// importTransactions imports a CSV file sent as the request body. Problems
// with the file as a whole answer 400 and import nothing; rows with bad
// values fail one by one and the others are stored.
func (s *server) importTransactions(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !slices.Contains(csvTypes, mediaType) {
		writeError(w, http.StatusUnsupportedMediaType, codeUnsupportedType,
			"send the file as the request body with Content-Type: text/csv", nil)
		return
	}
	// An import may take longer than the server-wide deadlines allow, so
	// this request gets its own: the body may take importTimeout to
	// arrive, and the answer to a bad or slow body must still get out
	// after that. A writer that cannot (a test recorder) keeps the
	// server's deadlines.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.importTimeout))
	_ = rc.SetWriteDeadline(time.Now().Add(s.importTimeout + s.responseGrace))

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, csvimport.MaxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var timedOut net.Error
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
				fmt.Sprintf("the file must be at most %d MB", csvimport.MaxBytes>>20), nil)
			return
		}
		if errors.As(err, &timedOut) && timedOut.Timeout() {
			writeError(w, http.StatusRequestTimeout, codeTimeout, "request body was not received in time", nil)
			return
		}
		writeError(w, http.StatusBadRequest, codeInvalidCSV, "could not read the file", nil)
		return
	}
	parsed, err := csvimport.Parse(data)
	if err != nil {
		var fileErr *csvimport.Error
		if errors.As(err, &fileErr) {
			var details any
			if fileErr.Line > 0 {
				details = map[string]any{"line": fileErr.Line}
			}
			writeError(w, http.StatusBadRequest, codeInvalidCSV, fileErr.Message, details)
			return
		}
		s.writeServiceError(w, r, err)
		return
	}

	var results []ledgerclient.ImportResult
	if len(parsed.Rows) > 0 {
		// The ledger call has an importTimeout of its own and starts only
		// now, after the body, so the write deadline moves past it too:
		// a slow upload followed by a long import must still get its
		// answer.
		_ = rc.SetWriteDeadline(time.Now().Add(s.importTimeout + s.responseGrace))
		results, err = s.ledger.ImportTransactions(r.Context(), userIDFrom(r.Context()), parsed.Rows)
		if err != nil {
			s.writeServiceError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, buildImportResponse(parsed, results))
}

func buildImportResponse(parsed csvimport.Result, ledgerResults []ledgerclient.ImportResult) importResponse {
	resp := importResponse{Total: parsed.Total(), Rows: make([]importRowResult, 0, parsed.Total())}
	for _, p := range parsed.Problems {
		resp.Rows = append(resp.Rows, importRowResult{Line: p.Line, Status: "failed", Error: &errorInfo{
			Code: codeValidation, Message: "row validation failed", Details: p.Fields,
		}})
	}
	for _, res := range ledgerResults {
		if res.Err != nil {
			resp.Rows = append(resp.Rows, importRowResult{Line: res.Line, Status: "failed", Error: rowErrorInfo(res.Err)})
			continue
		}
		resp.Rows = append(resp.Rows, importRowResult{Line: res.Line, Status: "created", ID: res.ID})
	}
	slices.SortFunc(resp.Rows, func(a, b importRowResult) int { return a.Line - b.Line })
	for _, row := range resp.Rows {
		if row.Status == "created" {
			resp.Created++
		} else {
			resp.Failed++
		}
	}
	return resp
}

// rowErrorInfo describes why a row was not stored, with the codes and
// details the API uses for the same errors of single requests.
func rowErrorInfo(err error) *errorInfo {
	var (
		validation *upstream.ValidationError
		exceeded   *ledgerclient.BudgetExceededError
		duplicate  *ledgerclient.DuplicateError
		rowErr     *ledgerclient.RowError
	)
	switch {
	case errors.As(err, &validation):
		return &errorInfo{Code: codeValidation, Message: "row validation failed", Details: validation.Fields}
	case errors.As(err, &exceeded):
		return &errorInfo{Code: codeBudgetExceeded, Message: exceeded.Error(), Details: budgetDetails(exceeded)}
	case errors.As(err, &duplicate):
		return &errorInfo{Code: codeAlreadyExists, Message: duplicate.Error(), Details: duplicateDetails(duplicate)}
	case errors.As(err, &rowErr):
		return &errorInfo{Code: rowErr.Code, Message: rowErr.Message}
	default:
		return &errorInfo{Code: codeInternal, Message: "internal error"}
	}
}
