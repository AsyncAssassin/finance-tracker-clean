package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var secret = strings.Repeat("test", 8) // Synthetic 32-byte key for configuration tests.

var allKeys = []string{
	"HTTP_ADDR", "GRPC_ADDR", "LOG_LEVEL", "LOG_FORMAT", "SHUTDOWN_TIMEOUT",
	"LEDGER_GRPC_ADDR", "LEDGER_TIMEOUT", "AUTH_GRPC_ADDR", "AUTH_TIMEOUT",
	"DATABASE_URL", "DB_CONNECT_WAIT", "JWT_SECRET", "JWT_TTL", "REDIS_URL", "REPORT_CACHE_TTL", "IMPORT_TIMEOUT", "IMPORT_WORKERS", "DB_MAX_CONNS",
}

// clearEnv unsets every variable and sets the required secret.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range allKeys {
		t.Setenv(key, "")
	}
	t.Setenv("JWT_SECRET", secret)
	t.Setenv("AUTH_REGISTRATION_EMAILS", "")
	t.Setenv("REDIS_PASSWORD", "")
}

func TestLoadGatewayDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadGateway()
	if err != nil {
		t.Fatal(err)
	}
	want := Gateway{HTTPAddr: ":8080", LogLevel: "info", LogFormat: "json", ShutdownTimeout: 10 * time.Second,
		LedgerAddr: "localhost:9090", LedgerTimeout: 10 * time.Second,
		AuthAddr: "localhost:9091", AuthTimeout: 10 * time.Second, ImportTimeout: time.Minute, JWTSecret: secret}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadGateway() = %+v, want %+v", cfg, want)
	}
}

func TestLoadGatewayFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("HTTP_ADDR", "127.0.0.1:8081")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("SHUTDOWN_TIMEOUT", "1m")
	t.Setenv("LEDGER_GRPC_ADDR", "ledger:9090")
	t.Setenv("LEDGER_TIMEOUT", "3s")
	t.Setenv("AUTH_GRPC_ADDR", "auth:9091")
	t.Setenv("AUTH_TIMEOUT", "4s")
	t.Setenv("IMPORT_TIMEOUT", "2m")
	cfg, err := LoadGateway()
	if err != nil {
		t.Fatal(err)
	}
	want := Gateway{HTTPAddr: "127.0.0.1:8081", LogLevel: "debug", LogFormat: "text", ShutdownTimeout: time.Minute,
		LedgerAddr: "ledger:9090", LedgerTimeout: 3 * time.Second,
		AuthAddr: "auth:9091", AuthTimeout: 4 * time.Second, ImportTimeout: 2 * time.Minute, JWTSecret: secret}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadGateway() = %+v, want %+v", cfg, want)
	}
}

func TestLoadGatewayRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"SHUTDOWN_TIMEOUT", "soon"},
		{"LEDGER_TIMEOUT", "fast"},
		{"LEDGER_TIMEOUT", "0s"},
		{"LEDGER_TIMEOUT", "-1s"},
		{"AUTH_TIMEOUT", "0s"},
		{"IMPORT_TIMEOUT", "-5s"},
		{"JWT_SECRET", ""},
		{"JWT_SECRET", secret[:31]},
	} {
		clearEnv(t)
		t.Setenv(tc.key, tc.value)
		if _, err := LoadGateway(); err == nil {
			t.Errorf("%s=%q was accepted", tc.key, tc.value)
		}
	}
}

func TestSecretErrorsExplainHowToFix(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "")
	_, err := LoadAuth()
	if err == nil || !strings.Contains(err.Error(), "openssl rand") {
		t.Errorf("err = %v, want a hint how to generate a secret", err)
	}
}

func TestLoadLedgerDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadLedger()
	if err != nil {
		t.Fatal(err)
	}
	want := Ledger{GRPCAddr: "localhost:9090", LogLevel: "info", LogFormat: "json", ShutdownTimeout: 10 * time.Second, DBConnectWait: 30 * time.Second, ReportCacheTTL: 5 * time.Minute, ImportWorkers: 4, DBMaxConns: 16}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadLedger() = %+v, want %+v", cfg, want)
	}
}

func TestLoadLedgerFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("GRPC_ADDR", "127.0.0.1:9191")
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("SHUTDOWN_TIMEOUT", "2s")
	t.Setenv("DATABASE_URL", "postgres://u:p@db:5432/finance")
	t.Setenv("DB_CONNECT_WAIT", "5s")
	t.Setenv("REDIS_URL", "redis://redis:6379/0")
	t.Setenv("REPORT_CACHE_TTL", "30s")
	t.Setenv("IMPORT_WORKERS", "8")
	cfg, err := LoadLedger()
	if err != nil {
		t.Fatal(err)
	}
	want := Ledger{GRPCAddr: "127.0.0.1:9191", LogLevel: "warn", LogFormat: "text", ShutdownTimeout: 2 * time.Second, DatabaseURL: "postgres://u:p@db:5432/finance", DBConnectWait: 5 * time.Second,
		RedisURL: "redis://redis:6379/0", ReportCacheTTL: 30 * time.Second, ImportWorkers: 8, DBMaxConns: 16}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadLedger() = %+v, want %+v", cfg, want)
	}
}

func TestLoadLedgerRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"SHUTDOWN_TIMEOUT", "soon"},
		{"DB_CONNECT_WAIT", "forever"},
		{"REPORT_CACHE_TTL", "0s"},
		{"REPORT_CACHE_TTL", "long"},
		{"IMPORT_WORKERS", "0"},
		{"IMPORT_WORKERS", "65"},
		{"IMPORT_WORKERS", "four"},
		{"DB_MAX_CONNS", "1"},
		{"IMPORT_WORKERS", "9"}, // more than half of the default pool of 16
	} {
		clearEnv(t)
		t.Setenv(tc.key, tc.value)
		if _, err := LoadLedger(); err == nil {
			t.Errorf("%s=%q was accepted", tc.key, tc.value)
		}
	}
}

func TestLoadAuthDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadAuth()
	if err != nil {
		t.Fatal(err)
	}
	want := Auth{GRPCAddr: "localhost:9091", LogLevel: "info", LogFormat: "json", ShutdownTimeout: 10 * time.Second,
		DBConnectWait: 30 * time.Second, JWTSecret: secret, JWTTTL: 24 * time.Hour}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadAuth() = %+v, want %+v", cfg, want)
	}
}

func TestLoadAuthFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("GRPC_ADDR", ":9091")
	t.Setenv("DATABASE_URL", "postgres://db:5432/finance")
	t.Setenv("JWT_TTL", "2h")
	cfg, err := LoadAuth()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":9091" || cfg.DatabaseURL != "postgres://db:5432/finance" || cfg.JWTTTL != 2*time.Hour {
		t.Errorf("LoadAuth() = %+v", cfg)
	}
}

func TestLoadAuthRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"JWT_SECRET", ""},
		{"JWT_SECRET", "short"},
		{"JWT_TTL", "0s"},
		{"JWT_TTL", "-1h"},
		{"JWT_TTL", "721h"},
		{"JWT_TTL", "a day"},
		{"SHUTDOWN_TIMEOUT", "soon"},
		{"DB_CONNECT_WAIT", "forever"},
	} {
		clearEnv(t)
		t.Setenv(tc.key, tc.value)
		if _, err := LoadAuth(); err == nil {
			t.Errorf("%s=%q was accepted", tc.key, tc.value)
		}
	}
}

func TestRegistrationEmailConfiguration(t *testing.T) {
	clearEnv(t)
	cfg, err := LoadAuth()
	if err != nil || len(cfg.RegistrationEmails) != 0 {
		t.Fatalf("default registration: %v", err)
	}
	t.Setenv("AUTH_REGISTRATION_EMAILS", " Anna@Example.com, boris@example.com ")
	cfg, err = LoadAuth()
	if err != nil || !reflect.DeepEqual(cfg.RegistrationEmails, []string{"anna@example.com", "boris@example.com"}) {
		t.Fatalf("allowlist: %v", err)
	}
	t.Setenv("AUTH_REGISTRATION_EMAILS", "private-invalid-email-marker")
	_, err = LoadAuth()
	if err == nil || strings.Contains(err.Error(), "private-invalid-email-marker") {
		t.Fatal("invalid allowlist accepted or disclosed")
	}
}
