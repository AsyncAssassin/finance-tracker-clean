// Package grpcserver exposes the ledger use cases as the gRPC service
// ledger.v1.LedgerService. The server itself, with health checks,
// reflection and interceptors, comes from grpckit.
package grpcserver

import (
	"log/slog"

	"google.golang.org/grpc"

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
	srv := grpckit.NewServer("ledger", log, grpc.MaxRecvMsgSize(grpckit.MaxMsgSize))
	ledgerv1.RegisterLedgerServiceServer(srv, &handler{svc: svc, log: log})
	return srv
}
