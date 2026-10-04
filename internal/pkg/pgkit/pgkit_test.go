package pgkit

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPermanent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"wrong password", &pgconn.PgError{Code: "28P01"}, true},
		{"authorization", &pgconn.PgError{Code: "28000"}, true},
		{"unknown database", &pgconn.PgError{Code: "3D000"}, true},
		{"wrapped", fmt.Errorf("ping: %w", &pgconn.PgError{Code: "3D000"}), true},
		{"too many connections is transient", &pgconn.PgError{Code: "53300"}, false},
		{"network error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := permanent(tc.err); got != tc.want {
			t.Errorf("%s: permanent(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestConnectFailsFastOnBadPassword checks that a wrong password is not
// retried for the whole wait. It needs TEST_DATABASE_URL.
func TestConnectFailsFastOnBadPassword(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping the PostgreSQL integration test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := Connect(context.Background(), raw, 10*time.Second, 3)
	if err == nil && pool.Config().MaxConns != 3 {
		t.Errorf("MaxConns = %d, want 3", pool.Config().MaxConns)
	}
	if err != nil {
		t.Fatalf("Connect with the right password: %v", err)
	}
	pool.Close()

	u.User = url.UserPassword(u.User.Username(), "definitely-wrong")
	start := time.Now()
	if _, err := Connect(context.Background(), u.String(), time.Minute, 0); err == nil {
		t.Fatal("Connect with a wrong password succeeded")
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("Connect took %s, want a fast failure", elapsed)
	}
}

func TestInvalidURLDoesNotExposeCredentials(t *testing.T) {
	const secret = "private-database-password"
	_, err := Connect(context.Background(), "postgres://finance:"+secret+"@localhost:invalid/finance", 0, 0)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("expected a sanitized configuration error, got %v", err)
	}
}
