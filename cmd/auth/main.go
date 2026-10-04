// Command auth is the gRPC service that registers users, checks their
// passwords and issues the access tokens the gateway verifies. It stores
// accounts in PostgreSQL when DATABASE_URL is set, using a schema prepared by cmd/migrate,
// and in memory otherwise.
//
// With -healthcheck it instead asks a running auth service on GRPC_ADDR
// for its health and exits with 0 when it serves.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	authv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/auth/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/hasher"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/config"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/logging"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
)

func main() {
	os.Exit(start())
}

// start runs the auth service, or the health check with -healthcheck, and
// returns the exit code: 2 for a bad configuration, 1 for a failure.
func start() int {
	healthcheck := flag.Bool("healthcheck", false, "check a running auth service on GRPC_ADDR and exit with 0 when it serves")
	flag.Parse()

	if *healthcheck {
		// Only the address: a probe needs no secrets.
		addr := config.GRPCAddr(config.DefaultAuthGRPCAddr)
		if err := grpckit.CheckHealth(addr, authv1.AuthService_ServiceDesc.ServiceName); err != nil {
			fmt.Fprintln(os.Stderr, "auth: unhealthy:", err)
			return 1
		}
		return 0
	}
	cfg, err := config.LoadAuth()
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth: bad configuration:", err)
		return 2
	}
	log, err := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth: bad configuration:", err)
		return 2
	}
	slog.SetDefault(log)
	tokens, err := jwtauth.NewIssuer([]byte(cfg.JWTSecret), cfg.JWTTTL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "auth: bad configuration:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, closeRepo, err := openRepository(ctx, cfg, log)
	if err != nil {
		log.Error("auth failed to start", "error", err)
		return 1
	}
	defer closeRepo()

	svc := usecase.New(repo, hasher.New(hasher.DefaultParams, 1), tokens, usecase.WithRegistrationEmails(cfg.RegistrationEmails))
	srv := grpcserver.New(svc, log)
	if err := srv.Run(ctx, cfg.GRPCAddr, cfg.ShutdownTimeout); err != nil {
		log.Error("auth stopped with error", "error", err)
		return 1
	}
	return 0
}

// openRepository connects to the prepared PostgreSQL schema when
// DATABASE_URL is set, and falls back to the in-memory store otherwise.
func openRepository(ctx context.Context, cfg config.Auth, log *slog.Logger) (usecase.Repository, func(), error) {
	if cfg.DatabaseURL == "" {
		log.Warn("DATABASE_URL is not set: using the in-memory store, accounts are lost on restart")
		return memory.New(), func() {}, nil
	}
	pool, err := pgkit.Connect(ctx, cfg.DatabaseURL, cfg.DBConnectWait, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	log.Info("database ready")
	return postgres.New(pool), pool.Close, nil
}
