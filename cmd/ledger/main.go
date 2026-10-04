// Command ledger is the gRPC service that owns transactions, budgets and
// reports. It stores them in PostgreSQL when DATABASE_URL is set, using a schema
// prepared by cmd/migrate, and in memory otherwise. With REDIS_URL it
// caches reports in Redis for REPORT_CACHE_TTL.
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

	opts, closeCache := openReportCache(ctx, cfg, log)
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

// openReportCache returns the service option for the Redis report cache
// when REDIS_URL is set. An unreachable Redis does not stop the ledger:
// the client reconnects by itself, and until then reports are built from
// the database.
func openReportCache(ctx context.Context, cfg config.Ledger, log *slog.Logger) ([]usecase.Option, func()) {
	if cfg.RedisURL == "" {
		log.Warn("REDIS_URL is not set: reports are built without a cache")
		return nil, func() {}
	}
	if cfg.DatabaseURL == "" {
		// Redis outlives this process, the in-memory store does not: after
		// a restart, cached reports would describe data that is gone.
		log.Warn("REDIS_URL is ignored with the in-memory store: reports are built without a cache")
		return nil, func() {}
	}
	rediscache.UseLogger(log)
	rdb, err := rediscache.Open(cfg.RedisURL, cfg.RedisPassword)
	if err != nil {
		log.Error("bad REDIS_URL: reports are built without a cache", "error", err)
		return nil, func() {}
	}
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		log.Warn("report cache is not reachable yet, reports are built from the database until it is", "error", err)
	} else {
		log.Info("report cache ready", "ttl", cfg.ReportCacheTTL.String())
	}
	cache := rediscache.New(rdb, cfg.ReportCacheTTL)
	return []usecase.Option{usecase.WithReportCache(cache, log)}, func() { _ = rdb.Close() }
}
