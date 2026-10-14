package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
)

// Error codes of the API. They are stable identifiers for clients; the
// message is for people.
const (
	codeInvalidJSON      = "invalid_json"
	codeValidation       = "validation_error"
	codeInvalidRequest   = "invalid_request"
	codeBudgetExceeded   = "budget_exceeded"
	codeAlreadyExists    = "already_exists"
	codeConflict         = "conflict"
	codeEmailTaken       = "email_taken"
	codeUnauthorized     = "unauthorized"
	codeBadCredentials   = "invalid_credentials"
	codeRateLimited      = "rate_limited"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codePayloadTooLarge  = "payload_too_large"
	codeUnsupportedType  = "unsupported_media_type"
	codeInvalidCSV       = "invalid_csv"
	codeLedgerDown       = "ledger_unavailable"
	codeAuthDown         = "auth_unavailable"
	codeServiceDown      = "service_unavailable" // a service without its own code
	codeTimeout          = "timeout"
	codeClientClosed     = "client_closed_request"
	codeInternal         = "internal_error"
)

// statusClientClosedRequest is the nginx convention for a request the
// client abandoned; net/http has no constant for it.
const statusClientClosedRequest = 499

// errorResponse is the one JSON shape of every error the API returns.
type errorResponse struct {
	Error errorInfo `json:"error"`
}

type errorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// maxBodyBytes limits the size of a JSON request body.
const maxBodyBytes = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep ">" and "&" readable in messages
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	writeJSON(w, status, errorResponse{Error: errorInfo{Code: code, Message: message, Details: details}})
}

// decodeJSON reads the request body into dst and answers 400 or 413 itself
// when the body is not acceptable. It reports whether decoding succeeded.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeJSONLimit(w, r, dst, maxBodyBytes)
}

func decodeJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		err = validateJSONObject(body)
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		err = dec.Decode(dst)
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	var timedOut net.Error
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge,
			fmt.Sprintf("request body must be at most %d bytes", limit), nil)
	} else if errors.As(err, &timedOut) && timedOut.Timeout() {
		writeError(w, http.StatusRequestTimeout, codeTimeout, "request body was not received in time", nil)
	} else {
		writeError(w, http.StatusBadRequest, codeInvalidJSON, "invalid JSON body: "+err.Error(), nil)
	}
	return false
}

// All request objects are flat. Validate the complete body before calling
// custom field decoders, and reject keys that target the same Go field.
func validateJSONObject(body []byte) error {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' || !utf8.Valid(body) || !json.Valid(body) {
		return errors.New("expected one valid UTF-8 JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	_, _ = dec.Token() // json.Valid already checked the complete syntax.
	seen := make(map[string]bool)
	for dec.More() {
		token, _ := dec.Token()
		key := token.(string)
		// encoding/json also matches field names with Unicode simple folding.
		folded := strings.Map(func(r rune) rune {
			lowest := r
			for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
				lowest = min(lowest, next)
			}
			return lowest
		}, key)
		if seen[folded] {
			return errors.New("duplicate object field")
		}
		seen[folded] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	return nil
}

// fieldErrors collects validation problems by field.
type fieldErrors map[string]string

func (f fieldErrors) add(field, problem string) {
	if _, seen := f[field]; !seen {
		f[field] = problem
	}
}

// respond writes 400 with the collected problems and reports whether it did.
func (f fieldErrors) respond(w http.ResponseWriter) bool {
	if len(f) == 0 {
		return false
	}
	writeError(w, http.StatusBadRequest, codeValidation, "request validation failed", f)
	return true
}

// writeServiceError maps an error from a service client to an HTTP response.
func (s *server) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *upstream.ValidationError
	var exceeded *ledgerclient.BudgetExceededError
	var duplicate *ledgerclient.DuplicateError
	var unavailable *upstream.UnavailableError
	var throttled *authclient.ThrottledError
	switch {
	case errors.Is(err, authclient.ErrRegistrationClosed):
		writeError(w, http.StatusForbidden, "registration_closed", authclient.ErrRegistrationClosed.Error(), nil)
	case errors.Is(err, authclient.ErrAccountQuota):
		writeError(w, http.StatusConflict, "quota_exceeded", authclient.ErrAccountQuota.Error(), nil)
	case errors.Is(err, upstream.ErrQuotaExceeded):
		writeError(w, http.StatusConflict, "quota_exceeded", upstream.ErrQuotaExceeded.Error(), nil)
	case errors.Is(err, authclient.ErrBusy), errors.Is(err, upstream.ErrBusy):
		busy(w)
	case errors.As(err, &throttled):
		seconds := max(1, int(math.Ceil(throttled.RetryAfter.Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		w.Header().Set("Cache-Control", "no-store")
		writeError(w, http.StatusTooManyRequests, codeRateLimited, throttled.Error(), nil)
	case errors.As(err, &validation):
		writeError(w, http.StatusBadRequest, codeValidation, "request validation failed", validation.Fields)
	case errors.As(err, &exceeded):
		writeError(w, http.StatusConflict, codeBudgetExceeded, exceeded.Error(), budgetDetails(exceeded))
	case errors.As(err, &duplicate):
		writeError(w, http.StatusConflict, codeAlreadyExists, duplicate.Error(), duplicateDetails(duplicate))
	case errors.Is(err, authclient.ErrEmailTaken):
		writeError(w, http.StatusConflict, codeEmailTaken, "an account with this email already exists, log in instead", nil)
	case errors.Is(err, authclient.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, codeBadCredentials, "invalid email or password", nil)
	case errors.Is(err, upstream.ErrInvalidArgument):
		// No field details: a separate code keeps validation_error details
		// always a map of fields, as documented.
		writeError(w, http.StatusBadRequest, codeInvalidRequest, err.Error(), nil)
	case errors.Is(err, upstream.ErrConflict):
		writeError(w, http.StatusConflict, codeConflict, err.Error(), nil)
	case errors.Is(err, upstream.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, err.Error(), nil)
	case errors.As(err, &unavailable):
		s.log.ErrorContext(r.Context(), "service unavailable", "error", err, "request_id", requestIDFrom(r.Context()))
		writeError(w, http.StatusBadGateway, unavailableCode(unavailable.Service),
			"the "+unavailable.Service+" service is unavailable, try again later", nil)
	case errors.Is(err, context.DeadlineExceeded):
		s.log.ErrorContext(r.Context(), "service timeout", "error", err, "request_id", requestIDFrom(r.Context()))
		writeError(w, http.StatusGatewayTimeout, codeTimeout, "a service behind the gateway did not answer in time", nil)
	case errors.Is(err, context.Canceled):
		// The client disconnected; nobody reads the response, so log quietly.
		s.log.InfoContext(r.Context(), "request canceled by the client", "request_id", requestIDFrom(r.Context()))
		writeError(w, statusClientClosedRequest, codeClientClosed, "request was canceled", nil)
	default:
		s.log.ErrorContext(r.Context(), "internal error", "error", err, "request_id", requestIDFrom(r.Context()))
		writeError(w, http.StatusInternalServerError, codeInternal, "internal error", map[string]any{
			"request_id": requestIDFrom(r.Context()),
		})
	}
}

// unavailableCode returns the documented error code for a service that
// cannot be reached.
func unavailableCode(service string) string {
	switch service {
	case "ledger":
		return codeLedgerDown
	case "auth":
		return codeAuthDown
	default:
		return codeServiceDown
	}
}

// budgetDetails and duplicateDetails are the details of the errors of the
// same codes, in a response and in an import row alike.
func budgetDetails(e *ledgerclient.BudgetExceededError) map[string]any {
	return map[string]any{
		"category":     e.Category,
		"period":       e.Period,
		"period_label": e.PeriodLabel,
		"limit":        e.Limit,
		"spent":        e.Spent,
		"amount":       e.Amount,
		"remaining":    e.Remaining,
	}
}

func duplicateDetails(e *ledgerclient.DuplicateError) map[string]any {
	details := map[string]any{"id": e.ExistingID}
	if e.ExternalID != "" {
		details["external_id"] = e.ExternalID
	}
	return details
}
