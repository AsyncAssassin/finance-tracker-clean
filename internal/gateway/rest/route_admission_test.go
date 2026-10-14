package rest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
)

func TestRejectedRoutesDoNotHoldAdmissionWhileDrainingBody(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"long path", "POST", "/api/" + strings.Repeat("x", 8192)},
		{"long method", strings.Repeat("X", 8192), "/api/budgets"},
		{"long redirect path", "GET", "/api//" + strings.Repeat("x", 8192)},
		{"long redirect dot segment", "GET", "/api/../" + strings.Repeat("x", 8192)},
		{"long redirect query", "GET", "/api//budgets?probe=" + strings.Repeat("x", 8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finished := make(chan struct{}, maxActiveAPIRequests)
			h := newHandler(&fakeLedger{listBudgets: func(context.Context, int64) ([]ledgerclient.Budget, error) { return nil, nil }})
			s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.ServeHTTP(w, r)
				if r.Method == tc.method && r.URL.RequestURI() == tc.path {
					finished <- struct{}{}
				}
			}))
			s.Config.ReadTimeout = 5 * time.Second
			s.Start()
			defer s.Close()
			var conns []net.Conn
			defer func() {
				for _, c := range conns {
					c.Close()
				}
			}()
			for range maxActiveAPIRequests {
				c, err := net.DialTimeout("tcp", s.Listener.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				conns = append(conns, c)
				if _, err := fmt.Fprintf(c, "%s %s HTTP/1.1\r\nHost: localhost\r\nContent-Length: 100\r\n\r\nx", tc.method, tc.path); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for range maxActiveAPIRequests {
				select {
				case <-finished:
				case <-deadline.C:
					t.Fatal("rejected route kept an admission slot waiting for the body")
				}
			}
			req, _ := http.NewRequest("GET", s.URL+"/api/budget", nil)
			req.Header.Set("Authorization", "Bearer "+testToken)
			client := &http.Client{Timeout: time.Second}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("independent request: %d", res.StatusCode)
			}
		})
	}
}

func TestPathCleaningDoesNotRedirect(t *testing.T) {
	h := newHandler(&fakeLedger{listBudgets: func(context.Context, int64) ([]ledgerclient.Budget, error) { return nil, nil }})
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/", 404},
		{"/api/budgets", 200},
		{"/api/budgets/", 404},
		{"/api/%62udgets", 200},
		{"/api/%2Fbudgets", 404},
		{"/api/%2e/budgets", 404},
		{"/api/budgets?probe=../food//", 200},
		{"/api//budgets", 400},
		{"/api/./budgets", 400},
		{"/api/../api/budgets", 400},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res, body := do(t, h, "GET", tc.path, "")
			if res.StatusCode != tc.want || res.Header.Get("Location") != "" {
				t.Fatalf("status=%d location=%q, want %d without redirect", res.StatusCode, res.Header.Get("Location"), tc.want)
			}
			if tc.want == 400 {
				decodeError(t, res, body, 400, codeInvalidRequest)
			}
		})
	}
}
