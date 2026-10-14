package postgres_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	rediscache "github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/cache/redis"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/postgres"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

// Drop the server's COMMIT confirmation after it has committed the data.
// Using the wire distinguishes an unknown outcome from a rolled-back write.
func lostCommitPool(t *testing.T, direct *pgxpool.Pool, onCommit func()) (*pgxpool.Pool, *atomic.Bool) {
	t.Helper()
	cfg := direct.Config()
	target := net.JoinHostPort(cfg.ConnConfig.Host, strconv.Itoa(int(cfg.ConnConfig.Port)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			backend, err := net.DialTimeout("tcp", target, time.Second)
			if err != nil {
				client.Close()
				continue
			}
			wg.Add(2)
			go func() { defer wg.Done(); defer client.Close(); defer backend.Close(); io.Copy(backend, client) }()
			go func() {
				defer wg.Done()
				defer client.Close()
				defer backend.Close()
				for {
					var header [5]byte
					if _, err := io.ReadFull(backend, header[:]); err != nil {
						return
					}
					size := int(binary.BigEndian.Uint32(header[1:])) - 4
					if size < 0 || size > 16<<20 {
						return
					}
					payload := make([]byte, size)
					if _, err := io.ReadFull(backend, payload); err != nil {
						return
					}
					if header[0] == 'C' && bytes.Equal(payload, []byte("COMMIT\x00")) && armed.CompareAndSwap(true, false) {
						if onCommit != nil {
							onCommit()
						}
						return
					}
					if _, err := client.Write(append(header[:], payload...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	cfg.ConnConfig.Host = "127.0.0.1"
	cfg.ConnConfig.Port = uint16(ln.Addr().(*net.TCPAddr).Port)
	cfg.ConnConfig.TLSConfig = nil
	cfg.ConnConfig.Fallbacks = nil
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		ln.Close()
		wg.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(); ln.Close(); wg.Wait() })
	return p, &armed
}

type failingInvalidation struct {
	usecase.ReportCache
	fail bool
}

func (c *failingInvalidation) Invalidate(ctx context.Context, id int64) error {
	if c.fail {
		return io.ErrUnexpectedEOF
	}
	return c.ReportCache.Invalidate(ctx, id)
}

func TestLostCommitInvalidatesReports(t *testing.T) {
	direct := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	date := civil.New(2026, time.October, 14)
	for _, kind := range []string{"transaction", "budget", "import", "canceled_import"} {
		for _, fail := range []bool{false, true} {
			t.Run(kind+"/redis_failure="+strconv.FormatBool(fail), func(t *testing.T) {
				freshRepo(t)
				writeCtx, cancelWrite := context.WithCancel(ctx)
				defer cancelWrite()
				var onCommit func()
				if kind == "canceled_import" {
					// Cancel only after PostgreSQL confirms the durable COMMIT.
					onCommit = cancelWrite
				}
				proxy, armed := lostCommitPool(t, direct, onCommit)
				mr := miniredis.RunT(t)
				rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
				defer rdb.Close()
				cache := &failingInvalidation{ReportCache: rediscache.New(rdb, time.Minute), fail: fail}
				svc := usecase.New(postgres.New(proxy), usecase.WithReportCache(cache, slog.New(slog.DiscardHandler)))
				for range 2 {
					if _, err := svc.Summary(ctx, 1, date, date); err != nil {
						t.Fatal(err)
					}
				}
				before, err := svc.Summary(ctx, 1, date, date)
				if err != nil || before.Cache != usecase.CacheHit || len(before.Categories) != 0 {
					t.Fatal("cache was not primed")
				}
				in := domain.TransactionInput{Category: "food", Amount: 100, Date: date, ExternalID: "test-commit-key"}
				armed.Store(true)
				var writeErr error
				switch kind {
				case "transaction":
					_, writeErr = svc.CreateTransaction(writeCtx, 1, in)
				case "budget":
					_, _, writeErr = svc.UpsertBudget(writeCtx, 1, domain.BudgetInput{Category: "food", Limit: 100, Period: "none"})
				case "import", "canceled_import":
					results, err := svc.Import(writeCtx, 1, []usecase.ImportRow{{Line: 2, Input: in}})
					if err != nil || len(results) != 1 {
						t.Fatalf("import: %v", err)
					}
					writeErr = results[0].Err
				}
				if !errors.Is(writeErr, usecase.ErrCommitUncertain) || armed.Load() {
					t.Fatalf("commit outcome was not marked uncertain: %v", writeErr)
				}
				if kind == "canceled_import" && !errors.Is(writeErr, context.Canceled) {
					t.Fatalf("cancellation cause was lost: %v", writeErr)
				}
				var rows int
				if err := direct.QueryRow(ctx, "SELECT (SELECT count(*) FROM transactions)+(SELECT count(*) FROM budgets)").Scan(&rows); err != nil {
					t.Fatal(err)
				}
				after, err := svc.Summary(ctx, 1, date, date)
				want := usecase.CacheMiss
				if fail {
					want = usecase.CacheOff
				}
				if err != nil || rows != 1 || after.Cache != want || len(after.Categories) != 1 {
					t.Fatalf("stale report: rows=%d cache=%s categories=%d error=%v", rows, after.Cache, len(after.Categories), err)
				}
				if kind != "budget" {
					if after.Total != 100 {
						t.Fatalf("wrong total: %v", after.Total)
					}
					_, err := svc.CreateTransaction(ctx, 1, in)
					var duplicate *domain.DuplicateError
					if !errors.As(err, &duplicate) {
						t.Fatalf("retry did not find the saved row: %v", err)
					}
				}
			})
		}
	}
}
