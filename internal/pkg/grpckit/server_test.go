package grpckit

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// echoDesc is a service with no methods, enough to register and check.
var echoDesc = grpc.ServiceDesc{ServiceName: "test.v1.Echo", HandlerType: (*any)(nil)}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestRunServesHealthAndStops(t *testing.T) {
	t.Parallel()
	srv := NewServer("test", slog.New(slog.DiscardHandler))
	srv.RegisterService(&echoDesc, struct{}{})
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, addr, time.Second) }()

	deadline := time.Now().Add(5 * time.Second)
	for CheckHealth(addr, echoDesc.ServiceName) != nil {
		if time.Now().After(deadline) {
			t.Fatal("the server never reported SERVING")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := CheckHealth(addr, "test.v1.Unknown"); err == nil {
		t.Error("an unregistered service is reported healthy")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if err := CheckHealth(addr, echoDesc.ServiceName); err == nil {
		t.Error("a stopped server is reported healthy")
	}
}

func TestRunReportsBusyPort(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := NewServer("test", slog.New(slog.DiscardHandler))
	if err := srv.Run(context.Background(), ln.Addr().String(), time.Second); err == nil {
		t.Error("Run on a busy port returned nil")
	}
}
