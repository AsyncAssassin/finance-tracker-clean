package healthcheck

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLocalAddr(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{":9090", "localhost:9090"},
		{"0.0.0.0:9090", "localhost:9090"},
		{"[::]:9090", "localhost:9090"},
		{"127.0.0.1:9191", "127.0.0.1:9191"},
		{"ledger:9090", "ledger:9090"},
		{"no-port", "no-port"},
	} {
		if got := LocalAddr(tc.listen); got != tc.want {
			t.Errorf("LocalAddr(%q) = %q, want %q", tc.listen, got, tc.want)
		}
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ping" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if err := HTTP(addr, "/ping"); err != nil {
		t.Errorf("healthy server: %v", err)
	}
	if err := HTTP(addr, "/missing"); err == nil {
		t.Error("a 404 passed the check")
	}
	srv.Close()
	if err := HTTP(addr, "/ping"); err == nil {
		t.Error("a stopped server passed the check")
	}
}
