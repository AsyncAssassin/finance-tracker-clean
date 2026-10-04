// Package grpckit holds the gRPC plumbing shared by the services: a server
// with health checks, reflection and interceptors for request IDs, logging
// and panics; helpers to report errors as statuses with details; the client
// side of the health check; and the dial options of the gateway's clients.
package grpckit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// Server is a gRPC server with the standard health service, which the
// container health checks call, and reflection, so grpcurl works without
// the proto files.
type Server struct {
	name   string
	log    *slog.Logger
	grpc   *grpc.Server
	health *health.Server
}

var _ grpc.ServiceRegistrar = (*Server)(nil)

// NewServer returns a server for the service called name that logs every
// call to log. opts are added to the defaults, such as a larger message
// size limit.
func NewServer(name string, log *slog.Logger, opts ...grpc.ServerOption) *Server {
	if log == nil {
		log = slog.Default()
	}
	opts = append([]grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unaryRequestID, unaryLogging(log), unaryRecovery(log)),
		grpc.ChainStreamInterceptor(streamRequestID, streamLogging(log), streamRecovery(log)),
	}, opts...)
	s := &Server{
		name:   name,
		log:    log,
		grpc:   grpc.NewServer(opts...),
		health: health.NewServer(),
	}
	healthpb.RegisterHealthServer(s.grpc, s.health)
	reflection.Register(s.grpc)
	return s
}

// RegisterService registers a service and reports it as SERVING to health
// checks. It implements grpc.ServiceRegistrar, so the generated
// RegisterXxxServer functions accept a *Server.
func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	s.grpc.RegisterService(desc, impl)
	s.health.SetServingStatus(desc.ServiceName, healthpb.HealthCheckResponse_SERVING)
}

// Serve accepts connections on ln until Shutdown is called. It returns nil
// after a shutdown.
func (s *Server) Serve(ln net.Listener) error {
	return s.grpc.Serve(ln)
}

// Shutdown reports NOT_SERVING to health checks, stops accepting calls and
// waits for the running ones. When ctx ends first, the remaining calls are
// canceled.
func (s *Server) Shutdown(ctx context.Context) {
	s.health.Shutdown()
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
}

// Run listens on addr and serves until ctx is done, then shuts down within
// shutdownTimeout. A busy port is reported as an error before anything is
// logged as listening.
func (s *Server) Run(ctx context.Context, addr string, shutdownTimeout time.Duration) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ln) }()
	s.log.Info(s.name+" listening", "addr", ln.Addr().String(), "protocol", "grpc")

	select {
	case err := <-errc:
		if err == nil || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	s.log.Info(s.name+" shutting down", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	s.Shutdown(shutdownCtx)
	return nil
}
