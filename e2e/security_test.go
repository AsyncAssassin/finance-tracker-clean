//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

const isolationReport = "/api/reports/summary?from=2026-10-01&to=2026-10-31"

func requireTotal(t *testing.T, r response, total float64) {
	t.Helper()
	var report struct {
		Total float64 `json:"total"`
	}
	r.json(t, &report)
	if r.status != 200 || report.Total != total {
		t.Fatalf("report: %d %s, want total %.2f", r.status, r.body, total)
	}
}

func TestUserIsolationAcrossAPIAndCache(t *testing.T) {
	a, b := newClient(t), newClient(t)
	a.register("isolation-a")
	b.register("isolation-b")
	if a.userID <= 0 || b.userID <= 0 || a.userID == b.userID {
		t.Fatal("expected two distinct accounts")
	}
	a.expect("POST", "/api/budget", `{"category":"общая","limit":1000,"period":"month"}`, 201)
	b.expect("POST", "/api/budget", `{"category":"общая","limit":2000,"period":"month"}`, 201)
	aRow := a.expect("POST", "/api/transactions", `{"category":"общая","amount":12.34,"date":"2026-10-05","description":"a-private","external_id":"shared-key"}`, 201)
	b.expect("POST", "/api/transactions", `{"category":"общая","amount":56.78,"date":"2026-10-05","description":"b-private","external_id":"shared-key"}`, 201)
	var row struct {
		ID int64 `json:"id"`
	}
	aRow.json(t, &row)
	if row.ID <= 0 {
		t.Fatal("missing transaction ID")
	}

	// Warm both cache entries for exactly the same period, then read them
	// again. A cache key missing the user would fail this alternating test.
	for range 2 {
		requireTotal(t, a.expect("GET", isolationReport, "", 200), 12.34)
		requireTotal(t, b.expect("GET", isolationReport, "", 200), 56.78)
	}
	if os.Getenv("E2E_REQUIRE_CACHE") == "1" {
		for _, c := range []*client{a, b} {
			if r := c.expect("GET", isolationReport, "", 200); r.header.Get("X-Cache") != "HIT" {
				t.Fatalf("expected a cached report for each user: %v", r.header)
			}
		}
	}

	// Identity supplied in query parameters or proxy-like headers cannot
	// override the JWT. Both list/export and the summary must stay private.
	for _, path := range []string{"/api/transactions", "/api/transactions/export.csv", "/api/budget", isolationReport} {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		r := b.do("GET", path+separator+fmt.Sprintf("user_id=%d", a.userID), "", "",
			"X-User-ID", strconv.FormatInt(a.userID, 10), "X-Forwarded-User", strconv.FormatInt(a.userID, 10))
		if r.status != 200 || strings.Contains(r.body, "a-private") {
			t.Fatalf("identity override in %s: %d %s", path, r.status, r.body)
		}
		if path == isolationReport {
			requireTotal(t, r, 56.78)
		} else if path == "/api/budget" {
			var budgets struct {
				Items []struct {
					Limit float64 `json:"limit"`
				} `json:"items"`
			}
			r.json(t, &budgets)
			if len(budgets.Items) != 1 || budgets.Items[0].Limit != 2000 {
				t.Fatalf("another user's budget: %s", r.body)
			}
		} else if !strings.Contains(r.body, "b-private") && path != "/api/budget" {
			t.Fatalf("own data missing from %s: %s", path, r.body)
		}
	}
	b.expect("POST", "/api/budget", fmt.Sprintf(`{"category":"общая","limit":1,"user_id":%d}`, a.userID), 400)
	b.expect("POST", "/api/transactions", fmt.Sprintf(`{"category":"общая","amount":1,"user_id":%d}`, a.userID), 400)

	// Changing only the subject of a real JWT must invalidate its signature.
	parts := strings.Split(b.token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims["sub"] = strconv.FormatInt(a.userID, 10)
	payload, _ = json.Marshal(claims)
	parts[1] = base64.RawURLEncoding.EncodeToString(payload)
	forged := &client{t: t, base: b.base, token: strings.Join(parts, ".")}
	forged.expect("GET", isolationReport, "", 401)
	forged.expect("GET", "/api/transactions/export.csv", "", 401)

	// A foreign CSV source ID is an import hint, not a capability to edit
	// the original row. The caller can import supplied data only as itself.
	file := fmt.Sprintf("id,date,category,amount,description,external_id\n%d,2026-10-05,общая,1.22,b-import,import-key\n", row.ID)
	for attempt := range 2 {
		r := b.do("POST", "/api/transactions/import", "text/csv", file)
		var result struct{ Created, Failed int }
		r.json(t, &result)
		if r.status != 200 || result.Created != 1-attempt || result.Failed != attempt {
			t.Fatalf("foreign source import attempt %d: %d %s", attempt+1, r.status, r.body)
		}
	}
	requireTotal(t, a.expect("GET", isolationReport, "", 200), 12.34)
	requireTotal(t, b.expect("GET", isolationReport, "", 200), 58)
	if r := a.expect("GET", "/api/transactions", "", 200); strings.Contains(r.body, "b-import") {
		t.Fatal("another user's import modified the owner's records")
	}
}

func TestLoginRateLimit(t *testing.T) {
	c := newClient(t)
	email := fmt.Sprintf("throttle-%d@example.com", time.Now().UnixNano())
	body := fmt.Sprintf(`{"email":%q,"password":"wrong test passphrase"}`, email)
	// Allow crossing one fixed-window boundary during HTTP round trips.
	// The exact five-attempt boundary is checked with a controlled clock
	// in the Auth tests. Five attempts can fit on each side of a boundary,
	// so the eleventh rapid request must reach HTTP 429.
	var r response
	for range 11 {
		r = c.do("POST", "/api/auth/login", "application/json", body)
		if r.status == 429 {
			break
		}
		if r.status != 401 {
			t.Fatalf("login before throttle: %d %s", r.status, r.body)
		}
	}
	seconds, err := strconv.Atoi(r.header.Get("Retry-After"))
	if r.status != 429 || err != nil || seconds < 1 || seconds > 60 || !strings.Contains(r.body, `"rate_limited"`) || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("throttle response: %v %s", r.header, r.body)
	}
	// A casing change must not buy another set of password attempts.
	if seconds > 1 { // Avoid asserting across the actual expiry boundary.
		c.expect("POST", "/api/auth/login", fmt.Sprintf(`{"email":%q,"password":"wrong test passphrase"}`, "  "+strings.ToUpper(email)+"  "), 429)
	}
	if r := c.do("GET", "/ping", "", ""); r.status != 200 || r.body != "pong" {
		t.Fatalf("service health after throttle: %d %s", r.status, r.body)
	}
}
