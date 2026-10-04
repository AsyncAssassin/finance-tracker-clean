package rest

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
)

func TestRequestLogsExcludeClientData(t *testing.T) {
	t.Parallel()
	const private = "private-input-marker"
	verifier, err := jwtauth.NewVerifier([]byte(strings.Repeat("test", 8)))
	if err != nil {
		t.Fatal(err)
	}
	// A malformed numeric claim makes the JWT parser include its input in an error.
	token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(`{"iat":"`+private+`"}`)) + ".signature"
	for name, path := range map[string]string{
		"unknown path": "/" + private,
		"bad token":    "/api/budget?ignored=" + private,
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			h := NewHandler(Deps{Tokens: verifier, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
			res, _ := do(t, h, http.MethodGet, path, "", "Authorization", "Bearer "+token)
			if res.StatusCode != http.StatusNotFound && res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unexpected status: %d", res.StatusCode)
			}
			if strings.Contains(logs.String(), private) || strings.Contains(logs.String(), token) {
				t.Fatal("request data reached the log")
			}
			if !strings.Contains(logs.String(), `"request_id":`) || !strings.Contains(logs.String(), `"status":`) {
				t.Fatal("request diagnostics are missing")
			}
		})
	}
}

func TestFinancialResponsesCannotBeStored(t *testing.T) {
	t.Parallel()
	for _, route := range protectedRoutes {
		for _, token := range []string{"", "Bearer " + testToken} {
			var user int64
			h := newHandler(openLedger(&user))
			contentType := "application/json"
			if route.path == importPath {
				contentType = "text/csv"
			}
			res, _ := do(t, h, route.method, route.path, route.body,
				"Authorization", token, "Content-Type", contentType)
			if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s %s status %d: missing privacy headers", route.method, route.path, res.StatusCode)
			}
		}
	}
}
