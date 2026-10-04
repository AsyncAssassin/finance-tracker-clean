// Command gateway is the HTTP entry point of the project. It validates
// requests, checks access tokens, forwards the calls to the ledger and auth
// services over gRPC and answers with JSON, mapping their gRPC errors to
// HTTP status codes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/api"
	authgrpc "github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient/grpcclient"
	ledgergrpc "github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient/grpcclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/rest"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/config"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/healthcheck"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/logging"
)

func main() {
	os.Exit(start())
}

// start runs the gateway and returns the exit code: 2 for a bad
// configuration, 1 for a failure. Returning instead of calling os.Exit
// lets the deferred cleanups run.
func start() int {
	check := flag.Bool("healthcheck", false, "check a running gateway on HTTP_ADDR and exit with 0 when /ping answers")
	flag.Parse()
	if *check {
		// Only the address: a probe needs no secrets.
		if err := healthcheck.HTTP(config.String("HTTP_ADDR", config.DefaultHTTPAddr), "/ping"); err != nil {
			fmt.Fprintln(os.Stderr, "gateway: unhealthy:", err)
			return 1
		}
		return 0
	}
	cfg, err := config.LoadGateway()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway: bad configuration:", err)
		return 2
	}
	log, err := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway: bad configuration:", err)
		return 2
	}
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokens, err := jwtauth.NewVerifier([]byte(cfg.JWTSecret))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway: bad configuration:", err)
		return 2
	}
	ledger, err := ledgergrpc.New(cfg.LedgerAddr, cfg.LedgerTimeout)
	if err != nil {
		log.Error("gateway failed to start", "error", err)
		return 1
	}
	ledger.WithImportTimeout(cfg.ImportTimeout)
	defer ledger.Close()
	auth, err := authgrpc.New(cfg.AuthAddr, cfg.AuthTimeout)
	if err != nil {
		log.Error("gateway failed to start", "error", err)
		return 1
	}
	defer auth.Close()
	log.Info("service clients ready", "ledger", cfg.LedgerAddr, "auth", cfg.AuthAddr)

	handler := rest.NewHandler(rest.Deps{
		Ledger:  ledger,
		Auth:    auth,
		Tokens:  tokens,
		Log:     log,
		OpenAPI: api.OpenAPI,
		Docs:    api.Docs,

		ImportTimeout: cfg.ImportTimeout,
	})

	if err := serve(ctx, cfg, log, handler); err != nil {
		log.Error("gateway stopped with error", "error", err)
		return 1
	}
	return 0
}

// serve serves handler on cfg.HTTPAddr until ctx is done, then shuts the
// server down gracefully within cfg.ShutdownTimeout.
func serve(ctx context.Context, cfg config.Gateway, log *slog.Logger, handler http.Handler) error {
	// Listen before logging, so a busy port is reported as a startup error.
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// The response to a slow service call is written after its timeout,
		// so the write deadline must leave room for it after that.
		WriteTimeout: max(cfg.LedgerTimeout, cfg.AuthTimeout) + 20*time.Second,
		IdleTimeout:  60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("gateway listening", "addr", ln.Addr().String(), "docs", "/docs")

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	log.Info("gateway shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
