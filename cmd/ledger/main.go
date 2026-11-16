// Command ledger is the gRPC service that owns transactions, budgets and
// reports. It stores them in PostgreSQL when DATABASE_URL is set, using a schema
// prepared by cmd/migrate, and in memory otherwise. With REDIS_ADDR it
// caches reports and budget lists in Redis.
//
// With -healthcheck it instead asks a running ledger on GRPC_ADDR for its
// health and exits with 0 when it serves; the container health check uses
// it because the image has no shell or curl.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	ledgerv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/ledger/v1"
	rediscache "github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/cache/redis"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/config"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/logging"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/pgkit"
)

func main() {
	os.Exit(start())
}

// start runs the ledger, or the health check with -healthcheck, and
// returns the exit code: 2 for a bad configuration, 1 for a failure.
// Returning instead of calling os.Exit lets the deferred cleanups run,
// such as closing the database pool.
func start() int {
	healthcheck := flag.Bool("healthcheck", false, "check a running ledger on GRPC_ADDR and exit with 0 when it serves")
	flag.Parse()

	if *healthcheck {
		addr := config.GRPCAddr(config.DefaultLedgerGRPCAddr)
		if err := grpckit.CheckHealth(addr, ledgerv1.LedgerService_ServiceDesc.ServiceName); err != nil {
			fmt.Fprintln(os.Stderr, "ledger: unhealthy:", err)
			return 1
		}
		return 0
	}
	cfg, err := config.LoadLedger()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: bad configuration:", err)
		return 2
	}
	log, err := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger: bad configuration:", err)
		return 2
	}
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, closeRepo, err := openRepository(ctx, cfg, log)
	if err != nil {
		log.Error("ledger failed to start", "error", err)
		return 1
	}
	defer closeRepo()

	opts, closeCache := openCache(ctx, cfg, log)
	defer closeCache()
	opts = append(opts, usecase.WithImportWorkers(cfg.ImportWorkers))
	srv := grpcserver.New(usecase.New(repo, opts...), log)
	if err := srv.Run(ctx, cfg.GRPCAddr, cfg.ShutdownTimeout); err != nil {
		log.Error("ledger stopped with error", "error", err)
		return 1
	}
	return 0
}

// openRepository connects to the prepared PostgreSQL schema when
// DATABASE_URL is set, and falls back to the in-memory store otherwise.
func openRepository(ctx context.Context, cfg config.Ledger, log *slog.Logger) (usecase.Repository, func(), error) {
	if cfg.DatabaseURL == "" {
		log.Warn("DATABASE_URL is not set: using the in-memory store, data is lost on restart")
		return memory.New(), func() {}, nil
	}
	pool, err := pgkit.Connect(ctx, cfg.DatabaseURL, cfg.DBConnectWait, cfg.DBMaxConns)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to database: %w", err)
	}
	log.Info("database ready")
	return postgres.New(pool), pool.Close, nil
}

// openCache enables caching with PostgreSQL. An unreachable Redis does not
// stop the ledger: reads use the database until the client reconnects.
func openCache(ctx context.Context, cfg config.Ledger, log *slog.Logger) ([]usecase.Option, func()) {
	if cfg.RedisAddr == "" {
		log.Info("Redis cache is disabled: REDIS_ADDR is empty")
		return nil, func() {}
	}
	if cfg.DatabaseURL == "" {
		// Redis outlives this process, the in-memory store does not: after
		// a restart, cached reports would describe data that is gone.
		log.Info("Redis cache is disabled with the in-memory store")
		return nil, func() {}
	}
	rediscache.UseLogger(log)
	rdb := rediscache.Open(cfg.RedisAddr, cfg.RedisDB, cfg.RedisPassword)
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		log.Warn("Redis cache is not reachable yet, reads use the database until it is", "error", err)
	} else {
		log.Info("Redis cache ready", "report_ttl", cfg.ReportCacheTTL.String(), "budget_ttl", cfg.BudgetCacheTTL.String())
	}
	cache := rediscache.New(rdb, cfg.ReportCacheTTL, cfg.BudgetCacheTTL)
	return []usecase.Option{usecase.WithCache(cache, log)}, func() { _ = rdb.Close() }
}
