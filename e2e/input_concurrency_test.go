//go:build e2e

package e2e

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// These bounded adversarial requests are only for an owned local stack.
func TestInputAndConcurrentWrites(t *testing.T) {
	c := newClient(t)
	u, err := url.Parse(c.base)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		t.Skip("requires a local stack")
	}
	c.register("input-concurrency")
	t.Run("malformed input does not store data", func(t *testing.T) {
		c := *c
		c.t = t
		for _, body := range []string{
			`{"category":"bad","amount":1}]`,
			`{"category":"bad","amount":1,"Amount":2}`,
			`{"category":"bad\u0000","amount":1}`,
			`{"category":"bad","amount":"1e1000000000"}`,
			`{"category":"bad","amount":1.001}`,
			`{"category":"bad","amount":1,"date":"2026-02-29"}`,
		} {
			c.expect("POST", "/api/transactions", body, 400)
		}
		c.expect("POST", "/api/transactions", `{"category":"bad","amount":1}`+strings.Repeat(" ", 1<<20), 413)
		for _, category := range []string{"%00", "%FF", "%C3%28"} {
			c.expect("GET", "/api/transactions?category="+category, "", 400)
			c.expect("GET", "/api/transactions/export.csv?category="+category, "", 400)
		}
		c.expect("GET", "/api/reports/summary?from=2026-01-01&to=2036-01-02", "", 400)
		r := c.expect("GET", "/api/transactions", "", 200)
		if r.body != "{\"items\":[]}\n" {
			t.Fatalf("invalid input stored: %s", r.body)
		}
	})
	t.Run("SQL and HTML remain literal data", func(t *testing.T) {
		c := *c
		c.t = t
		category := "x'); drop table budgets;--"
		description := `<script>alert("test")</script>`
		body, _ := json.Marshal(map[string]any{"category": category, "amount": 1, "date": "2026-10-05", "description": description})
		c.expect("POST", "/api/transactions", string(body), 201)
		r := c.expect("GET", "/api/transactions?category="+url.QueryEscape(category), "", 200)
		var rows struct {
			Items []struct{ Category, Description string }
		}
		r.json(t, &rows)
		if len(rows.Items) != 1 || rows.Items[0].Category != category || rows.Items[0].Description != description {
			t.Fatalf("literal input changed: %s", r.body)
		}
		if !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
			t.Fatal("wrong response type")
		}
		c.expect("GET", "/api/budget", "", 200)
	})
	t.Run("calendar boundaries and limit reduction", func(t *testing.T) {
		c := *c
		c.t = t
		for _, tc := range []struct{ period, first, last, next string }{
			{"month", "2024-02-01", "2024-02-29", "2024-03-01"},
			{"year", "2024-01-01", "2024-12-31", "2025-01-01"},
			{"none", "0001-01-01", "2026-10-05", "9999-12-31"},
		} {
			c.expect("POST", "/api/budget", fmt.Sprintf(`{"category":%q,"limit":1,"period":%q}`, tc.period, tc.period), 201)
			add := func(date string, amount float64, status int) {
				c.expect("POST", "/api/transactions", fmt.Sprintf(`{"category":%q,"amount":%.2f,"date":%q}`, tc.period, amount, date), status)
			}
			add(tc.first, 0.99, 201)
			add(tc.last, 0.01, 201)
			add(tc.last, 0.01, 409)
			if tc.period == "none" {
				add(tc.next, 0.01, 409)
			} else {
				add(tc.next, 1, 201)
			}
			c.expect("POST", "/api/budget", fmt.Sprintf(`{"category":%q,"limit":0.5,"period":%q}`, tc.period, tc.period), 200)
			add(tc.last, 0.01, 409)
		}
	})
	t.Run("concurrent budget and idempotency", func(t *testing.T) {
		c := *c
		c.t = t
		c.expect("POST", "/api/budget", `{"category":"race","limit":10,"period":"month"}`, 201)
		responses := parallelPosts(&c, 24, func(i int) string {
			return fmt.Sprintf(`{"category":"race","amount":1,"date":"2026-10-05","external_id":"race-%d"}`, i)
		})
		created, blocked := 0, 0
		for _, r := range responses {
			switch {
			case r.status == 201:
				created++
			case r.status == 409 && strings.Contains(r.body, `"budget_exceeded"`):
				blocked++
			default:
				t.Fatalf("concurrent budget: %d %s", r.status, r.body)
			}
		}
		if created != 10 || blocked != 14 {
			t.Fatalf("created=%d blocked=%d", created, blocked)
		}
		responses = parallelPosts(&c, 24, func(i int) string {
			return fmt.Sprintf(`{"category":"duplicate-%d","amount":1,"date":"2026-10-05","external_id":"one-key"}`, i%2)
		})
		created, blocked = 0, 0
		var storedID int64
		var duplicateIDs []int64
		for _, r := range responses {
			if r.status == 201 {
				created++
				var row struct{ ID int64 }
				r.json(t, &row)
				storedID = row.ID
			} else if r.status == 409 && strings.Contains(r.body, `"already_exists"`) {
				blocked++
				var out struct {
					Error struct{ Details struct{ ID int64 } }
				}
				r.json(t, &out)
				duplicateIDs = append(duplicateIDs, out.Error.Details.ID)
			} else {
				t.Fatalf("concurrent retry: %d %s", r.status, r.body)
			}
		}
		if created != 1 || blocked != 23 || storedID <= 0 {
			t.Fatalf("created=%d duplicates=%d id=%d", created, blocked, storedID)
		}
		for _, id := range duplicateIDs {
			if id != storedID {
				t.Fatalf("duplicate ID %d, want %d", id, storedID)
			}
		}
	})
	t.Run("CSV limits and invalid rows", func(t *testing.T) {
		c := *c
		c.t = t
		for _, tc := range []struct {
			file   string
			status int
		}{
			{"category,amount,Amount\nx,1,2\n", 400},
			{"category,amount\n" + strings.Repeat("x,1\n", 10001), 400},
			{"category,amount\n" + strings.Repeat(" ", 5<<20), 413},
		} {
			r := c.do("POST", "/api/transactions/import", "text/csv", tc.file)
			if r.status != tc.status {
				t.Fatalf("CSV size/shape: %d %s", r.status, r.body)
			}
		}
		file := "date,category,amount,description,external_id\n2026-10-05,csv,1,bad\x00text,csv-bad\n2026-10-05,csv,1,bad\xfftext,csv-bad-utf8\n2026-10-05,csv,1,valid,csv-good\n"
		for attempt := range 2 {
			r := c.do("POST", "/api/transactions/import", "text/csv", file)
			var result struct{ Created, Failed int }
			r.json(t, &result)
			if r.status != 200 || result.Created != 1-attempt || result.Failed != 2+attempt {
				t.Fatalf("CSV partial/retry: %d %s", r.status, r.body)
			}
		}
		for _, formula := range []string{"=1+1", "+1+1", "-1+1", "@sum(1)", "'=1+1"} {
			body, _ := json.Marshal(map[string]any{"category": "formula", "amount": 1, "date": "2026-10-05", "description": formula})
			c.expect("POST", "/api/transactions", string(body), 201)
		}
		exported := c.expect("GET", "/api/transactions/export.csv?category=formula", "", 200)
		rows, err := csv.NewReader(strings.NewReader(exported.body)).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 6 {
			t.Fatalf("exported %d rows", len(rows))
		}
		for _, row := range rows[1:] {
			if !strings.HasPrefix(row[4], "'") {
				t.Fatalf("unescaped formula: %q", row[4])
			}
		}
	})
	if r := c.do("GET", "/ping", "", ""); r.status != 200 || r.body != "pong" {
		t.Fatal("unhealthy after checks")
	}
}

func parallelPosts(c *client, n int, body func(int) string) []response {
	var wg sync.WaitGroup
	out := make(chan response, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() { <-start; out <- c.do("POST", "/api/transactions", "application/json", body(i)) })
	}
	close(start)
	wg.Wait()
	close(out)
	var results []response
	for r := range out {
		results = append(results, r)
	}
	return results
}
