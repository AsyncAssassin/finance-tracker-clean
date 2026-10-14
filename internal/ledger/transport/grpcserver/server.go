// Package grpcserver exposes the ledger use cases as the gRPC service
// ledger.v1.LedgerService. The server itself, with health checks,
// reflection and interceptors, comes from grpckit.
package grpcserver

import (
	"context"
	"log/slog"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
)

// New returns the gRPC server of the ledger for svc that logs every call
// to log.
func New(svc *usecase.Service, log *slog.Logger) *grpckit.Server {
	if log == nil {
		log = slog.Default()
	}
	srv := grpckit.NewServer("ledger", log, grpc.MaxRecvMsgSize(grpckit.MaxMsgSize),
		grpc.ChainUnaryInterceptor(admission()))
	ledgerv1.RegisterLedgerServiceServer(srv, &handler{svc: svc, log: log})
	return srv
}

const maxActiveRequests = 32

// Keep the slot until the actual work ends, including cache invalidation after
// cancellation. HTTP admission alone cannot bound these remaining handlers.
// Imports hold one slot; their row workers retain their existing separate limit.
func admission() grpc.UnaryServerInterceptor {
	slots := make(chan struct{}, maxActiveRequests)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, "/ledger.v1.LedgerService/") {
			return next(ctx, req)
		}
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			return next(ctx, req)
		default:
			return nil, grpckit.WithDetails(codes.ResourceExhausted, "too many active requests, retry later",
				&errdetails.ErrorInfo{Reason: "LEDGER_BUSY", Domain: "finance-tracker"})
		}
	}
}
