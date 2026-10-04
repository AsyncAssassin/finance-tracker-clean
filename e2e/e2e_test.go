//go:build e2e

// The end-to-end test runs the README scenario against a running stack:
//
//	docker compose up -d --build --wait
//	E2E_BASE_URL=http://localhost:8080 go test -tags e2e -count=1 ./e2e/
//
// Every run registers new users, so it can run against a stack with data.
// Auth counts ten registration attempts per process in a window of one
// minute that starts with the first counted attempt after the previous
// window ended; a repeated registration answered 409 counts too. A full
// run creates five accounts and spends six attempts, the live sheet test
// one account and two attempts, so a second run against the same Auth in
// the same window gets 429: wait for the window to end (Retry-After) or
// use a fresh, isolated Auth. The test does not retry on purpose.
// With E2E_REQUIRE_CACHE=1 the report cache must answer (Redis is up).

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// httpClient fails a hung request quickly instead of at the test timeout.
var httpClient = &http.Client{Timeout: 30 * time.Second}

type client struct {
	t      *testing.T
	base   string
	token  string
	userID int64
}

type response struct {
	status int
	header http.Header
	body   string
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.body), v); err != nil {
		t.Fatalf("body is not the expected JSON: %v: %s", err, r.body)
	}
}

func (c *client) do(method, path, contentType, body string, headers ...string) response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: string(b)}
}

func (c *client) expect(method, path, body string, status int) response {
	c.t.Helper()
	r := c.do(method, path, "application/json", body)
	if r.status != status {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, status, r.body)
	}
	return r
}

func newClient(t *testing.T) *client {
	base := os.Getenv("E2E_BASE_URL")
	if base == "" {
		t.Skip("E2E_BASE_URL is not set")
	}
	return &client{t: t, base: strings.TrimRight(base, "/")}
}

// register makes a new account and keeps its token.
func (c *client) register(name string) string {
	c.t.Helper()
	run := os.Getenv("E2E_RUN_ID")
	if run == "" {
		run = fmt.Sprint(time.Now().UnixNano())
	}
	email := fmt.Sprintf("%s-%s@example.com", name, run)
	r := c.expect("POST", "/api/auth/register", fmt.Sprintf(`{"email":%q,"password":"зелёный трамвай едет"}`, email), 201)
	var s struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	r.json(c.t, &s)
	if s.AccessToken == "" || s.TokenType != "Bearer" || r.header.Get("Cache-Control") != "no-store" {
		c.t.Fatalf("registration answer: %s", r.body)
	}
	c.token = s.AccessToken
	c.userID = s.User.ID
	return email
}

func TestScenario(t *testing.T) {
	c := newClient(t)
	if r := c.do("GET", "/ping", "", ""); r.status != 200 || r.body != "pong" {
		t.Fatalf("ping: %d %q", r.status, r.body)
	}
	if r := c.do("GET", "/api/budget", "", ""); r.status != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("no token: %d %v", r.status, r.header)
	}
	c.expect("POST", "/api/auth/register", `{"email":"weak@example.com","password":"qwerty"}`, 400)
	email := c.register("anna")
	// The same email again, and a login with a wrong password.
	c.expect("POST", "/api/auth/register", fmt.Sprintf(`{"email":%q,"password":"зелёный трамвай едет"}`, email), 409)
	c.expect("POST", "/api/auth/login", fmt.Sprintf(`{"email":%q,"password":"зелёный трамвай стоит"}`, email), 401)
	c.expect("POST", "/api/auth/login", fmt.Sprintf(`{"email":%q,"password":"зелёный трамвай едет"}`, strings.ToUpper(email)), 200)

	c.expect("POST", "/api/budget", `{"category":"Еда","limit":5000}`, 201)
	c.expect("POST", "/api/budget", `{"category":"еда","limit":5000,"period":"month"}`, 200)
	c.expect("POST", "/api/transactions", `{"amount":"4750,50","category":"еда","description":"продукты","date":"2026-10-03","external_id":"e2e-1"}`, 201)
	over := c.expect("POST", "/api/transactions", `{"amount":500,"category":"еда","date":"2026-10-04"}`, 409)
	if !strings.Contains(over.body, `"budget_exceeded"`) || !strings.Contains(over.body, `"remaining":249.50`) {
		t.Errorf("over the budget: %s", over.body)
	}
	dup := c.expect("POST", "/api/transactions", `{"amount":"4750,50","category":"еда","date":"2026-10-03","external_id":"e2e-1"}`, 409)
	if !strings.Contains(dup.body, `"already_exists"`) {
		t.Errorf("resent transaction: %s", dup.body)
	}
	c.expect("POST", "/api/transactions", `{"amount":700,"category":"здоровье","date":"2026-10-04"}`, 201)

	// The report, served from the cache the second time and rebuilt after
	// a change.
	const report = "/api/reports/summary?from=2026-10-01&to=2026-10-31"
	first := c.expect("GET", report, "", 200)
	var sum struct {
		Total      float64 `json:"total"`
		Categories []struct {
			Category string `json:"category"`
			Budget   *struct {
				Percent float64 `json:"percent"`
			} `json:"budget"`
		} `json:"categories"`
	}
	first.json(t, &sum)
	if sum.Total != 5450.50 || len(sum.Categories) != 2 || sum.Categories[0].Budget == nil || sum.Categories[0].Budget.Percent != 95.01 {
		t.Errorf("report: %s", first.body)
	}
	second := c.expect("GET", report, "", 200)
	c.expect("POST", "/api/transactions", `{"amount":1,"category":"кафе","date":"2026-10-05"}`, 201)
	third := c.expect("GET", report, "", 200)
	got := []string{first.header.Get("X-Cache"), second.header.Get("X-Cache"), third.header.Get("X-Cache")}
	switch {
	case got[0] == "" && os.Getenv("E2E_REQUIRE_CACHE") == "":
		t.Log("the report cache is off; X-Cache is not checked")
	case got[0] != "MISS" || got[1] != "HIT" || got[2] != "MISS":
		t.Errorf("X-Cache = %v, want MISS, HIT, MISS", got)
	}

	csv := c.expect("GET", "/api/transactions/export.csv?from=2026-10-01&to=2026-10-31", "", 200)
	if !strings.HasPrefix(csv.body, "id,date,category,amount,description,external_id\n") || strings.Count(csv.body, "\n") != 4 {
		t.Errorf("export:\n%s", csv.body)
	}

	// Import: a fresh row, a resent one, a broken amount; then the export
	// back into the same account stores nothing.
	file := "date,category,amount,external_id\n2026-10-06,кафе,10,e2e-2\n2026-10-03,еда,4750.50,e2e-1\n2026-10-07,кафе,много,e2e-3\n"
	imp := c.do("POST", "/api/transactions/import", "text/csv", file)
	var res struct {
		Total, Created, Failed int
		Rows                   []struct {
			Line   int    `json:"line"`
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"rows"`
	}
	imp.json(t, &res)
	code := func(i int) string {
		if i >= len(res.Rows) || res.Rows[i].Error == nil {
			return ""
		}
		return res.Rows[i].Error.Code
	}
	if imp.status != 200 || res.Total != 3 || res.Created != 1 || code(1) != "already_exists" || code(2) != "validation_error" {
		t.Errorf("import: %d %s", imp.status, imp.body)
	}
	again := c.do("POST", "/api/transactions/import", "text/csv", csv.body)
	again.json(t, &res)
	if res.Created != 0 || res.Failed != res.Total {
		t.Errorf("export imported back into the same account: %s", again.body)
	}
	if r := c.do("POST", "/api/transactions/import", "text/csv", "category,ammount\nеда,1\n"); r.status != 400 || !strings.Contains(r.body, "invalid_csv") {
		t.Errorf("broken file: %d %s", r.status, r.body)
	}

	// Another user sees none of it.
	other := newClient(t)
	other.register("boris")
	if r := other.expect("GET", "/api/budget", "", 200); r.body != "{\"items\":[]}\n" {
		t.Errorf("another user's budgets: %s", r.body)
	}
	if r := other.expect("GET", "/api/transactions", "", 200); r.body != "{\"items\":[]}\n" {
		t.Errorf("another user's transactions: %s", r.body)
	}
}
