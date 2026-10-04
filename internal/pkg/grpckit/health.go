package grpckit

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/healthcheck"
)

// CheckHealth asks the gRPC server listening on listenAddr whether service
// is SERVING; see the healthcheck package.
func CheckHealth(listenAddr, service string) error {
	conn, err := grpc.NewClient("passthrough:///"+healthcheck.LocalAddr(listenAddr), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), healthcheck.Timeout)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: service})
	if err != nil {
		return err
	}
	if s := resp.GetStatus(); s != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("status %s", s)
	}
	return nil
}
