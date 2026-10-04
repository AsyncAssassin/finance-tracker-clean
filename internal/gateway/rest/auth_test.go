package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
	"github.com/golang-jwt/jwt/v5"
)

// protectedRoutes are every route that needs a token.
var protectedRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/api/transactions", `{"amount":1,"category":"еда"}`},
	{http.MethodGet, "/api/transactions", ""},
	{http.MethodGet, "/api/transactions/export.csv", ""},
	{http.MethodPost, "/api/transactions/import", "category,amount\nеда,1\n"},
	{http.MethodPost, "/api/budget", `{"category":"еда","limit":100}`},
	{http.MethodGet, "/api/budget", ""},
	{http.MethodGet, "/api/reports/summary?from=2026-10-01&to=2026-10-31", ""},
}

// openLedger answers every ledger call successfully and records the user.
func openLedger(gotUser *int64) *fakeLedger {
	return &fakeLedger{
		create: func(_ context.Context, u int64, _ ledgerclient.TransactionInput) (ledgerclient.Transaction, error) {
			*gotUser = u
			return ledgerclient.Transaction{ID: 1}, nil
		},
		list: func(_ context.Context, u int64, _ ledgerclient.TransactionFilter) ([]ledgerclient.Transaction, error) {
			*gotUser = u
			return nil, nil
		},
		export: func(_ context.Context, u int64, _ ledgerclient.TransactionFilter) ([]byte, error) {
			*gotUser = u
			return []byte("id\n"), nil
		},
		importRows: func(_ context.Context, u int64, _ []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
			*gotUser = u
			return []ledgerclient.ImportResult{{Line: 2, ID: 1}}, nil
		},
		upsert: func(_ context.Context, u int64, _ ledgerclient.BudgetInput) (ledgerclient.Budget, bool, error) {
			*gotUser = u
			return ledgerclient.Budget{}, true, nil
		},
		listBudgets: func(_ context.Context, u int64) ([]ledgerclient.Budget, error) {
			*gotUser = u
			return nil, nil
		},
		summary: func(_ context.Context, u int64, _, _ civil.Date) (ledgerclient.Summary, error) {
			*gotUser = u
			return ledgerclient.Summary{}, nil
		},
	}
}

func TestProtectedRoutesNeedAToken(t *testing.T) {
	t.Parallel()
	for _, rt := range protectedRoutes {
		for name, header := range map[string]string{
			"no header":     "",
			"basic auth":    "Basic YW5uYTpzZWNyZXQ=",
			"empty bearer":  "Bearer ",
			"unknown token": "Bearer forged",
			"bare token":    testToken,
			"extra token":   "Bearer " + testToken + " another-token",
		} {
			var gotUser int64
			h := newHandler(openLedger(&gotUser))
			res, body := do(t, h, rt.method, rt.path, rt.body, "Authorization", header)
			decodeError(t, res, body, http.StatusUnauthorized, codeUnauthorized)
			if !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s %s with %s: WWW-Authenticate = %q", rt.method, rt.path, name, res.Header.Get("WWW-Authenticate"))
			}
			if gotUser != 0 {
				t.Errorf("%s %s with %s reached the ledger", rt.method, rt.path, name)
			}
		}
	}
}

func TestTokenChoosesTheUser(t *testing.T) {
	t.Parallel()
	for _, rt := range protectedRoutes {
		var gotUser int64
		h := newHandler(openLedger(&gotUser))
		// The scheme name is case-insensitive.
		contentType := "application/json"
		if rt.path == importPath {
			contentType = "text/csv"
		}
		res, body := do(t, h, rt.method, rt.path, rt.body, "Authorization", "bearer token-of-7", "Content-Type", contentType)
		if res.StatusCode >= 300 {
			t.Errorf("%s %s: status %d: %s", rt.method, rt.path, res.StatusCode, body)
		}
		if gotUser != otherUser {
			t.Errorf("%s %s acted as user %d, want %d from the token", rt.method, rt.path, gotUser, otherUser)
		}
	}
}

func TestOpenRoutesNeedNoToken(t *testing.T) {
	t.Parallel()
	auth := &fakeAuth{login: func(context.Context, string, string) (authclient.Session, error) {
		return authclient.Session{AccessToken: "t"}, nil
	}}
	h := newHandlerWithAuth(&fakeLedger{}, auth)
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/ping", ""},
		{http.MethodGet, "/openapi.yaml", ""},
		{http.MethodGet, "/docs", ""},
		{http.MethodPost, "/api/auth/login", `{"email":"anna@example.com","password":"x"}`},
	} {
		res, body := do(t, h, rt.method, rt.path, rt.body, "Authorization", "")
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s %s without a token: %d %s", rt.method, rt.path, res.StatusCode, body)
		}
	}
}

func TestRealJWTVerificationProtectsEveryRoute(t *testing.T) {
	t.Parallel()
	secret := []byte("0123456789abcdef0123456789abcdef")
	verifier, err := jwtauth.NewVerifier(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	valid := jwt.MapClaims{"sub": "7", "iss": "finance-tracker-auth", "aud": "finance-tracker-api",
		"iat": now.Add(-time.Hour).Unix(), "exp": now.Add(time.Hour).Unix()}
	sign := func(claims jwt.MapClaims, key []byte) string {
		t.Helper()
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	expired := jwt.MapClaims{"sub": "7", "iss": "finance-tracker-auth", "aud": "finance-tracker-api",
		"iat": now.Add(-time.Hour).Unix(), "exp": now.Add(-time.Minute).Unix()}
	for name, token := range map[string]string{
		"valid":           sign(valid, secret),
		"expired":         sign(expired, secret),
		"wrong signature": sign(valid, []byte("another-test-secret-of-32-bytes!!")),
	} {
		for _, rt := range protectedRoutes {
			var gotUser int64
			h := NewHandler(Deps{Ledger: openLedger(&gotUser), Auth: &fakeAuth{}, Tokens: verifier})
			contentType := "application/json"
			if rt.path == importPath {
				contentType = "text/csv"
			}
			res, body := do(t, h, rt.method, rt.path, rt.body, "Authorization", "Bearer "+token, "Content-Type", contentType)
			if name == "valid" {
				if res.StatusCode >= 300 || gotUser != 7 {
					t.Errorf("valid token at %s: status=%d user=%d", rt.path, res.StatusCode, gotUser)
				}
			} else {
				decodeError(t, res, body, 401, codeUnauthorized)
				if gotUser != 0 {
					t.Errorf("%s reached %s as user %d", name, rt.path, gotUser)
				}
			}
		}
	}
}

func TestRegisterAndLogin(t *testing.T) {
	t.Parallel()
	expires := time.Date(2026, time.October, 5, 12, 0, 0, 0, time.UTC)
	session := authclient.Session{
		User:        authclient.User{ID: 5, Email: "anna@example.com", CreatedAt: time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)},
		AccessToken: "jwt-for-5",
		ExpiresAt:   expires,
	}
	var got [2]string
	auth := &fakeAuth{
		register: func(_ context.Context, email, password string) (authclient.Session, error) {
			got = [2]string{email, password}
			return session, nil
		},
		login: func(context.Context, string, string) (authclient.Session, error) { return session, nil },
	}
	h := newHandlerWithAuth(&fakeLedger{}, auth)

	for path, wantStatus := range map[string]int{"/api/auth/register": http.StatusCreated, "/api/auth/login": http.StatusOK} {
		res, body := do(t, h, http.MethodPost, path, `{"email":"Anna@Example.com","password":"зелёный трамвай едет"}`, "Authorization", "")
		if res.StatusCode != wantStatus {
			t.Fatalf("%s: status %d, want %d: %s", path, res.StatusCode, wantStatus, body)
		}
		if res.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, res.Header.Get("Cache-Control"))
		}
		var resp struct {
			User struct {
				ID    int64  `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
			AccessToken string    `json:"access_token"`
			TokenType   string    `json:"token_type"`
			ExpiresAt   time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.User.ID != 5 || resp.User.Email != "anna@example.com" || resp.AccessToken != "jwt-for-5" ||
			resp.TokenType != "Bearer" || !resp.ExpiresAt.Equal(expires) {
			t.Errorf("%s: response = %+v", path, resp)
		}
	}
	if got != [2]string{"Anna@Example.com", "зелёный трамвай едет"} {
		t.Errorf("the auth service got %q; the gateway must pass the credentials as they are", got)
	}
}

func TestAuthErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		authErr    error
		wantStatus int
		wantCode   string
	}{
		{"missing fields", `{"email":" "}`, nil, 400, codeValidation},
		{"unknown field", `{"email":"a@b.ru","password":"x","name":"Anna"}`, nil, 400, codeInvalidJSON},
		{"policy violation", `{"email":"a@b.ru","password":"short"}`, &upstream.ValidationError{Fields: map[string]string{"password": "must be at least 15 characters"}}, 400, codeValidation},
		{"email taken", `{"email":"a@b.ru","password":"x"}`, authclient.ErrEmailTaken, 409, codeEmailTaken},
		{"busy", `{"email":"a@b.ru","password":"x"}`, authclient.ErrBusy, 503, "server_busy"},
		{"registration closed", `{"email":"a@b.ru","password":"x"}`, authclient.ErrRegistrationClosed, 403, "registration_closed"},
		{"account quota", `{"email":"a@b.ru","password":"x"}`, authclient.ErrAccountQuota, 409, "quota_exceeded"},
		{"wrong credentials", `{"email":"a@b.ru","password":"x"}`, authclient.ErrInvalidCredentials, 401, codeBadCredentials},
		{"throttled", `{"email":"a@b.ru","password":"x"}`, &authclient.ThrottledError{RetryAfter: 1500 * time.Millisecond}, 429, codeRateLimited},
		{"auth down", `{"email":"a@b.ru","password":"x"}`, &upstream.UnavailableError{Service: "auth", Reason: "refused"}, 502, codeAuthDown},
		{"unknown service down", `{"email":"a@b.ru","password":"x"}`, &upstream.UnavailableError{Service: "Auth", Reason: "refused"}, 502, codeServiceDown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fail := func(context.Context, string, string) (authclient.Session, error) {
				return authclient.Session{}, tc.authErr
			}
			h := newHandlerWithAuth(&fakeLedger{}, &fakeAuth{register: fail, login: fail})
			for _, path := range []string{"/api/auth/register", "/api/auth/login"} {
				res, body := do(t, h, http.MethodPost, path, tc.body, "Authorization", "")
				decodeError(t, res, body, tc.wantStatus, tc.wantCode)
				if tc.wantStatus == 429 {
					delay, err := strconv.Atoi(res.Header.Get("Retry-After"))
					if err != nil || delay != 2 || res.Header.Get("Cache-Control") != "no-store" {
						t.Errorf("throttle headers = %v, want Retry-After 2 and no-store", res.Header)
					}
				}
			}
		})
	}
}

func TestAuthBodyHasSeparateSizeLimit(t *testing.T) {
	called := false
	auth := &fakeAuth{login: func(context.Context, string, string) (authclient.Session, error) {
		called = true
		return authclient.Session{}, nil
	}}
	h := newHandlerWithAuth(&fakeLedger{}, auth)
	res, body := do(t, h, "POST", "/api/auth/login", `{"email":"anna@example.com","password":"`+strings.Repeat("a", maxAuthBodyBytes)+`"}`)
	decodeError(t, res, body, 413, codePayloadTooLarge)
	if called {
		t.Fatal("oversized body reached auth")
	}
	res, _ = do(t, h, "POST", "/api/auth/login", `{"email":"anna@example.com","password":"`+strings.Repeat(`\u0449`, 128)+`"}`)
	if res.StatusCode != 200 || !called {
		t.Fatalf("escaped valid password rejected: %d", res.StatusCode)
	}
}
