package rest

import (
	"net/http"
	"strings"
	"sync"
)

const maxActiveAPIRequests = 32
const maxActiveAuthRequests = 4

// admission bounds simultaneous allocations and upstream work per gateway.
// It rejects excess work instead of building an unbounded waiting queue.
// This is not a per-user rate limit or protection against a network DDoS.
func admission(next http.Handler) http.Handler {
	all := make(chan struct{}, maxActiveAPIRequests)
	auth := make(chan struct{}, maxActiveAuthRequests)
	exports := make(chan struct{}, 1)
	imports := make(chan struct{}, 2)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// Take this smaller allowance first, so rejected auth work cannot
		// occupy the pool used by authenticated operations.
		if r.URL.Path == "/api/auth/login" || r.URL.Path == "/api/auth/register" {
			if !takeSlot(auth) {
				busy(w)
				return
			}
			defer func() { <-auth }()
		}
		if !takeSlot(all) {
			busy(w)
			return
		}
		defer func() { <-all }()
		var heavy chan struct{}
		switch r.URL.Path {
		case "/api/transactions/export.csv":
			heavy = exports
		case "/api/transactions/import":
			heavy = imports
		}
		if heavy != nil {
			if !takeSlot(heavy) {
				busy(w)
				return
			}
			defer func() { <-heavy }()
		}
		next.ServeHTTP(w, r)
	})
}

// This wrapper runs after authentication. The global import allowance still
// caps total work, while one caller cannot occupy both upload slots.
func oneImportPerUser(next http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	active := make(map[int64]bool)
	return func(w http.ResponseWriter, r *http.Request) {
		id := userIDFrom(r.Context())
		mu.Lock()
		if active[id] {
			mu.Unlock()
			busy(w)
			return
		}
		active[id] = true
		mu.Unlock()
		defer func() { mu.Lock(); delete(active, id); mu.Unlock() }()
		next(w, r)
	}
}

func takeSlot(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "server_busy", "too many active requests, retry later", nil)
}
