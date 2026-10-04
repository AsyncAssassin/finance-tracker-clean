package rest

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestAdmissionBoundsWorkAndKeepsPingAvailable(t *testing.T) {
	for _, tc := range []struct {
		path string
		max  int
	}{
		{"/api/reports/summary", 32},
		{"/api/auth/login", 4},
		{"/api/auth/register", 4},
		{"/api/transactions/export.csv", 1},
		{"/api/transactions/import", 2},
	} {
		t.Run(tc.path, func(t *testing.T) {
			entered := make(chan struct{}, tc.max)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			h := admission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					entered <- struct{}{}
					<-release
				}
				w.WriteHeader(http.StatusOK)
			}))
			var wg sync.WaitGroup
			for i := 0; i < tc.max; i++ {
				wg.Go(func() { h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", tc.path, nil)) })
				<-entered
			}
			for i := 0; i < 40; i++ {
				res, body := do(t, h, "GET", tc.path, "")
				decodeError(t, res, body, 503, "server_busy")
				if res.Header.Get("Retry-After") != "1" {
					t.Fatal("missing retry delay")
				}
			}
			res, _ := do(t, h, "GET", "/ping", "")
			if res.StatusCode != 200 {
				t.Fatal("ping was blocked by API work")
			}
			if tc.max < 32 {
				res, _ = do(t, h, "GET", "/api/budget", "")
				if res.StatusCode != 200 {
					t.Fatal("heavy request rejection leaked an API slot")
				}
			}
			unblock()
			wg.Wait()
			res, _ = do(t, h, "GET", tc.path, "")
			if res.StatusCode != 200 {
				t.Fatal("slot was not released after completing the request")
			}
		})
	}
}

func TestAdmissionReleasesSlotsAfterPanic(t *testing.T) {
	h := admission(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test") }))
	for i := 0; i < 40; i++ {
		func() {
			defer func() {
				if recover() != "test" {
					t.Error("handler was not called or panic changed")
				}
			}()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("HEAD", "/api/transactions/export.csv", nil))
		}()
	}
}
