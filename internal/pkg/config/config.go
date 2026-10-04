// Package config reads the settings of the services from environment
// variables. Every variable has a default suitable for local runs, except
// secrets, which have none.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
)

// Default addresses of the gRPC services: the loopback interface, so a
// service started by hand is not reachable from the network. The gateway
// dials the same addresses by default.
const (
	DefaultLedgerGRPCAddr = "localhost:9090"
	DefaultAuthGRPCAddr   = "localhost:9091"
	DefaultHTTPAddr       = ":8080"
)

// GRPCAddr returns GRPC_ADDR or def. The -healthcheck mode of a service
// reads only this, so a probe does not need the service's secrets.
func GRPCAddr(def string) string {
	return String("GRPC_ADDR", def)
}

// Gateway holds the settings of the gateway service.
type Gateway struct {
	HTTPAddr        string        // HTTP_ADDR, address to listen on
	LogLevel        string        // LOG_LEVEL: debug, info, warn or error
	LogFormat       string        // LOG_FORMAT: json or text
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT, how long to wait for in-flight requests
	LedgerAddr      string        // LEDGER_GRPC_ADDR, host:port of the ledger gRPC server
	LedgerTimeout   time.Duration // LEDGER_TIMEOUT, the longest a call to the ledger may take
	AuthAddr        string        // AUTH_GRPC_ADDR, host:port of the auth gRPC server
	AuthTimeout     time.Duration // AUTH_TIMEOUT, the longest a call to auth may take
	ImportTimeout   time.Duration // IMPORT_TIMEOUT, the deadline of each phase of a CSV import on its own: reading the body, then the ledger call
	JWTSecret       string        // JWT_SECRET, the key the access tokens are signed with
}

// LoadGateway reads the gateway settings from the environment.
func LoadGateway() (Gateway, error) {
	cfg := Gateway{
		HTTPAddr:   String("HTTP_ADDR", DefaultHTTPAddr),
		LogLevel:   String("LOG_LEVEL", "info"),
		LogFormat:  String("LOG_FORMAT", "json"),
		LedgerAddr: String("LEDGER_GRPC_ADDR", DefaultLedgerGRPCAddr),
		AuthAddr:   String("AUTH_GRPC_ADDR", DefaultAuthGRPCAddr),
	}
	var err error
	if cfg.ShutdownTimeout, err = Duration("SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Gateway{}, err
	}
	if cfg.LedgerTimeout, err = positiveDuration("LEDGER_TIMEOUT", 10*time.Second); err != nil {
		return Gateway{}, err
	}
	if cfg.AuthTimeout, err = positiveDuration("AUTH_TIMEOUT", 10*time.Second); err != nil {
		return Gateway{}, err
	}
	if cfg.ImportTimeout, err = positiveDuration("IMPORT_TIMEOUT", time.Minute); err != nil {
		return Gateway{}, err
	}
	if cfg.JWTSecret, err = jwtSecret(); err != nil {
		return Gateway{}, err
	}
	return cfg, nil
}

// Auth holds the settings of the auth service.
type Auth struct {
	GRPCAddr           string        // GRPC_ADDR, address to listen on; loopback by default
	LogLevel           string        // LOG_LEVEL: debug, info, warn or error
	LogFormat          string        // LOG_FORMAT: json or text
	ShutdownTimeout    time.Duration // SHUTDOWN_TIMEOUT, how long to wait for running calls
	DatabaseURL        string        // DATABASE_URL; empty means an in-memory store
	DBConnectWait      time.Duration // DB_CONNECT_WAIT, how long to wait for the database at startup
	JWTSecret          string        // JWT_SECRET, the key the access tokens are signed with
	JWTTTL             time.Duration // JWT_TTL, how long an access token is valid
	RegistrationEmails []string      // AUTH_REGISTRATION_EMAILS, empty closes registration
}

// MaxJWTTTL caps the token lifetime: tokens cannot be revoked, so a
// mistyped "8760h" must not issue tokens valid for a year.
const MaxJWTTTL = 30 * 24 * time.Hour

// LoadAuth reads the auth settings from the environment.
func LoadAuth() (Auth, error) {
	cfg := Auth{
		GRPCAddr:    GRPCAddr(DefaultAuthGRPCAddr),
		LogLevel:    String("LOG_LEVEL", "info"),
		LogFormat:   String("LOG_FORMAT", "json"),
		DatabaseURL: String("DATABASE_URL", ""),
	}
	var err error
	for _, raw := range strings.Split(os.Getenv("AUTH_REGISTRATION_EMAILS"), ",") {
		email := domain.NormalizeEmail(raw)
		if email == "" {
			continue
		}
		if domain.ValidateEmail(email) != nil {
			return Auth{}, fmt.Errorf("AUTH_REGISTRATION_EMAILS: expected comma-separated email addresses")
		}
		cfg.RegistrationEmails = append(cfg.RegistrationEmails, email)
	}
	if len(cfg.RegistrationEmails) > domain.MaxAccounts {
		return Auth{}, fmt.Errorf("AUTH_REGISTRATION_EMAILS: at most %d addresses", domain.MaxAccounts)
	}
	if cfg.ShutdownTimeout, err = Duration("SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Auth{}, err
	}
	if cfg.DBConnectWait, err = Duration("DB_CONNECT_WAIT", 30*time.Second); err != nil {
		return Auth{}, err
	}
	if cfg.JWTSecret, err = jwtSecret(); err != nil {
		return Auth{}, err
	}
	if cfg.JWTTTL, err = positiveDuration("JWT_TTL", 24*time.Hour); err != nil {
		return Auth{}, err
	}
	if cfg.JWTTTL > MaxJWTTTL {
		return Auth{}, fmt.Errorf("JWT_TTL: must be at most %s, got %s", MaxJWTTTL, cfg.JWTTTL)
	}
	return cfg, nil
}

// jwtSecret reads JWT_SECRET, which has no default: a secret known from
// the source code would let anyone forge tokens.
func jwtSecret() (string, error) {
	s := os.Getenv("JWT_SECRET")
	switch {
	case s == "":
		return "", fmt.Errorf("JWT_SECRET: is required; generate one with: openssl rand -base64 32")
	case len(s) < jwtauth.MinSecretLen:
		return "", fmt.Errorf("JWT_SECRET: must be at least %d bytes, got %d; generate one with: openssl rand -base64 32",
			jwtauth.MinSecretLen, len(s))
	}
	return s, nil
}

// Ledger holds the settings of the ledger service.
type Ledger struct {
	GRPCAddr        string        // GRPC_ADDR, address to listen on; loopback by default, see LoadLedger
	LogLevel        string        // LOG_LEVEL: debug, info, warn or error
	LogFormat       string        // LOG_FORMAT: json or text
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT, how long to wait for running calls
	DatabaseURL     string        // DATABASE_URL; empty means an in-memory store
	DBConnectWait   time.Duration // DB_CONNECT_WAIT, how long to wait for the database at startup
	RedisURL        string        // REDIS_URL of the report cache; empty means no cache
	RedisPassword   string        // REDIS_PASSWORD, separate from the loggable endpoint
	ReportCacheTTL  time.Duration // REPORT_CACHE_TTL, how long a cached report lives
	ImportWorkers   int           // IMPORT_WORKERS, how many import rows are stored at once
	DBMaxConns      int32         // DB_MAX_CONNS, the size of the database connection pool
}

// LoadLedger reads the ledger settings from the environment. The ledger
// trusts the user ID in every call, so by default it listens on the
// loopback interface only; compose sets GRPC_ADDR=:9090 inside its network.
func LoadLedger() (Ledger, error) {
	cfg := Ledger{
		GRPCAddr:      GRPCAddr(DefaultLedgerGRPCAddr),
		LogLevel:      String("LOG_LEVEL", "info"),
		LogFormat:     String("LOG_FORMAT", "json"),
		DatabaseURL:   String("DATABASE_URL", ""),
		RedisURL:      String("REDIS_URL", ""),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
	}
	var err error
	if cfg.ShutdownTimeout, err = Duration("SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return Ledger{}, err
	}
	if cfg.DBConnectWait, err = Duration("DB_CONNECT_WAIT", 30*time.Second); err != nil {
		return Ledger{}, err
	}
	if cfg.ReportCacheTTL, err = positiveDuration("REPORT_CACHE_TTL", 5*time.Minute); err != nil {
		return Ledger{}, err
	}
	if cfg.ImportWorkers, err = intInRange("IMPORT_WORKERS", 4, 1, 64); err != nil {
		return Ledger{}, err
	}
	maxConns, err := intInRange("DB_MAX_CONNS", 16, 2, 1000)
	if err != nil {
		return Ledger{}, err
	}
	cfg.DBMaxConns = int32(maxConns)
	// Every import row holds a connection while it is stored; imports may
	// take at most half of the pool, so other requests are not starved.
	if cfg.ImportWorkers > maxConns/2 {
		return Ledger{}, fmt.Errorf("IMPORT_WORKERS (%d) must be at most half of DB_MAX_CONNS (%d)", cfg.ImportWorkers, maxConns)
	}
	return cfg, nil
}

// intInRange returns the variable parsed as an integer within [lo, hi], or
// def when it is unset.
func intInRange(key string, def, lo, hi int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s: %q is not an integer from %d to %d", key, v, lo, hi)
	}
	return n, nil
}

// positiveDuration is Duration that rejects zero and negative values.
func positiveDuration(key string, def time.Duration) (time.Duration, error) {
	d, err := Duration(key, def)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: must be positive, got %s", key, d)
	}
	return d, nil
}

// String returns the value of the variable or def when it is unset or empty.
func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Duration returns the variable parsed as a Go duration such as "10s",
// or def when it is unset.
func Duration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration such as 10s or 1m", key, v)
	}
	return d, nil
}
