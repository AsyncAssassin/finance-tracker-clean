package grpckit

import (
	"testing"
	"time"
)

func TestWithScheme(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"ledger:9090", "passthrough:///ledger:9090"},
		{"localhost:9090", "passthrough:///localhost:9090"},
		{"127.0.0.1:9090", "passthrough:///127.0.0.1:9090"},
		{"[::1]:9090", "passthrough:///[::1]:9090"},
		{"dns:///ledger:9090", "dns:///ledger:9090"},
		{"dns:ledger:9090", "dns:ledger:9090"},
		{"unix:/run/ledger.sock", "unix:/run/ledger.sock"},
		{"passthrough:///bufnet", "passthrough:///bufnet"},
	} {
		if got := withScheme(tc.in); got != tc.want {
			t.Errorf("withScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDialKeepsTheTarget(t *testing.T) {
	t.Parallel()
	conn, err := Dial("ledger:9090", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.Target(); got != "passthrough:///ledger:9090" {
		t.Errorf("Target() = %q", got)
	}
}
