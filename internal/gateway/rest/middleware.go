package rest

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
)

type ctxKey int

const userIDKey ctxKey = iota

// authenticated runs h only for a request with a valid access token in
// "Authorization: Bearer <token>" and gives h the user ID from the token.
// Otherwise it answers 401 with a WWW-Authenticate header (RFC 6750).
func (s *server) authenticated(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="finance-tracker"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized,
				"an access token is required: send Authorization: Bearer <token> from POST /api/auth/login", nil)
			return
		}
		userID, err := s.tokens.Verify(token)
		if err != nil {
			// Parser errors can quote untrusted claims from the token.
			s.log.InfoContext(r.Context(), "rejected access token", "request_id", requestIDFrom(r.Context()))
			w.Header().Set("WWW-Authenticate", `Bearer realm="finance-tracker", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, codeUnauthorized, "the access token is invalid or expired, log in again", nil)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userIDKey, userID)))
	}
}

// bearerToken takes the token from an Authorization header value. The
// scheme name is case-insensitive (RFC 7235).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// userIDFrom returns the user of the request; 0 when none is attached.
func userIDFrom(ctx context.Context) int64 {
	id, _ := ctx.Value(userIDKey).(int64)
	return id
}

// requestID takes the X-Request-ID header from the client or generates one,
// stores it in the context and echoes it in the response. The ledger client
// forwards it to the ledger, so both services log the same ID.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestid.Header)
		if !requestid.Valid(id) {
			id = requestid.New()
		}
		w.Header().Set(requestid.Header, id)
		next.ServeHTTP(w, r.WithContext(requestid.With(r.Context(), id)))
	})
}

func requestIDFrom(ctx context.Context) string {
	return requestid.From(ctx)
}

// API responses contain credentials or personal financial data, including CSV.
// Apply the policy before routing so errors and admission rejections keep it.
func responseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests writes one log line per request with its status and duration.
func logRequests(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			level := slog.LevelInfo
			if r.URL.Path == "/ping" && sw.status() == http.StatusOK {
				// The container health check calls it every few seconds.
				level = slog.LevelDebug
			}
			log.Log(r.Context(), level, "request",
				"method", r.Method,
				// The registered pattern has no client-supplied path or query data.
				"route", r.Pattern,
				"status", sw.status(),
				"bytes", sw.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", requestIDFrom(r.Context()),
			)
		})
	}
}

// recoverer turns a panic in a handler into a 500 response and a log line
// with the stack, so one bad request cannot take the gateway down.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Track whether the handler already answered, without relying
			// on an outer middleware having wrapped w.
			sw, ok := w.(*statusWriter)
			if !ok {
				sw = &statusWriter{ResponseWriter: w}
			}
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if err, ok := p.(error); ok && err == http.ErrAbortHandler {
					panic(p)
				}
				log.ErrorContext(r.Context(), "panic in handler",
					"panic", p, "stack", string(debug.Stack()), "request_id", requestIDFrom(r.Context()))
				if !sw.written {
					writeError(sw, http.StatusInternalServerError, codeInternal, "internal error", map[string]any{
						"request_id": requestIDFrom(r.Context()),
					})
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// statusWriter records the status code and size of a response.
type statusWriter struct {
	http.ResponseWriter
	code    int
	bytes   int
	written bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.written {
		w.code = code
		w.written = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.code = http.StatusOK
		w.written = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) status() int {
	if !w.written {
		return http.StatusOK
	}
	return w.code
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
