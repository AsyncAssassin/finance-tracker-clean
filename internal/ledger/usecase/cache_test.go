package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	rediscache "github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/cache/redis"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

// logBuffer collects log output written from several goroutines.
type logBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func newCachedService(t *testing.T, cache usecase.ReportCache) (*usecase.Service, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	clock := func() time.Time { return time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC) }
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return usecase.New(memory.New(), usecase.WithClock(clock), usecase.WithReportCache(cache, log)), logs
}

func redisCache(t *testing.T) (*rediscache.Cache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return rediscache.New(rdb, time.Minute), mr
}

func summary(t *testing.T, s *usecase.Service) usecase.Report {
	t.Helper()
	r, err := s.Summary(context.Background(), user, oct1, oct31)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReportCacheHitMissAndInvalidation(t *testing.T) {
	t.Parallel()
	cache, _ := redisCache(t)
	s, logs := newCachedService(t, cache)
	if _, err := create(s, 1000, "еда", today); err != nil {
		t.Fatal(err)
	}

	if r := summary(t, s); r.Cache != usecase.CacheMiss || r.Total != 1000 {
		t.Fatalf("first report: %s, total %v; want MISS and 1000", r.Cache, r.Total)
	}
	if r := summary(t, s); r.Cache != usecase.CacheHit || r.Total != 1000 {
		t.Fatalf("second report: %s, total %v; want HIT and 1000", r.Cache, r.Total)
	}

	// A new transaction makes the cached report stale.
	if _, err := create(s, 500, "еда", today); err != nil {
		t.Fatal(err)
	}
	if r := summary(t, s); r.Cache != usecase.CacheMiss || r.Total != 1500 {
		t.Fatalf("after a transaction: %s, total %v; want MISS and 1500", r.Cache, r.Total)
	}
	// So does a budget change.
	setBudget(t, s, "еда", 500000, "month")
	r := summary(t, s)
	if r.Cache != usecase.CacheMiss || len(r.Categories) != 1 || r.Categories[0].Budget == nil {
		t.Fatalf("after a budget change: %s, %+v; want MISS with the budget", r.Cache, r.Categories)
	}
	if !strings.Contains(logs.String(), "report cache hit") || !strings.Contains(logs.String(), "report cache miss") {
		t.Errorf("hits and misses are not logged:\n%s", logs)
	}
	if strings.Contains(logs.String(), "user_id=") || strings.Contains(logs.String(), "from=") || strings.Contains(logs.String(), "to=") {
		t.Error("cache diagnostics include account or report data")
	}
}

func TestReportCacheIsPerUser(t *testing.T) {
	t.Parallel()
	cache, _ := redisCache(t)
	s, _ := newCachedService(t, cache)
	ctx := context.Background()
	if _, err := s.CreateTransaction(ctx, 2, domain.TransactionInput{Amount: 700, Category: "еда", Date: today}); err != nil {
		t.Fatal(err)
	}
	summary(t, s) // user 1, cached with nothing spent
	other, err := s.Summary(ctx, 2, oct1, oct31)
	if err != nil || other.Cache != usecase.CacheMiss || other.Total != 700 {
		t.Errorf("another user's report = %s, total %v, %v; want their own MISS", other.Cache, other.Total, err)
	}
}

func TestWithoutCache(t *testing.T) {
	t.Parallel()
	if r := summary(t, newService(t)); r.Cache != usecase.CacheOff {
		t.Errorf("Cache = %q without a cache, want empty", r.Cache)
	}
}

// flakyCache fails as told and counts the calls.
type flakyCache struct {
	getErr, setErr, invalidateErr error
	gets, sets, invalidations     atomic.Int32
}

func (c *flakyCache) Get(context.Context, int64, civil.Date, civil.Date) (domain.Summary, string, bool, error) {
	c.gets.Add(1)
	return domain.Summary{}, "key", false, c.getErr
}

func (c *flakyCache) Set(context.Context, string, domain.Summary) error {
	c.sets.Add(1)
	return c.setErr
}

func (c *flakyCache) TTL() time.Duration { return time.Minute }

func (c *flakyCache) Invalidate(ctx context.Context, _ int64) error {
	c.invalidations.Add(1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.invalidateErr
}

func TestFailingCacheFallsBackAndPauses(t *testing.T) {
	t.Parallel()
	cache := &flakyCache{getErr: errors.New("connection refused")}
	s, logs := newCachedService(t, cache)
	if _, err := create(s, 1000, "еда", today); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if r := summary(t, s); r.Cache != usecase.CacheOff || r.Total != 1000 {
			t.Fatalf("report with a failing cache: %s, total %v; want it built without the cache", r.Cache, r.Total)
		}
	}
	if n := cache.gets.Load(); n != 1 {
		t.Errorf("the failing cache was asked %d times, want once and then a pause", n)
	}
	if !strings.Contains(logs.String(), "report cache failed") {
		t.Errorf("the failure is not logged:\n%s", logs)
	}
}

func TestFailingCacheWriteStillServes(t *testing.T) {
	t.Parallel()
	s, logs := newCachedService(t, &flakyCache{setErr: errors.New("OOM command not allowed")})
	if r := summary(t, s); r.Cache != usecase.CacheMiss {
		t.Errorf("Cache = %q, want MISS: the report was built, only storing it failed", r.Cache)
	}
	if !strings.Contains(logs.String(), "OOM") {
		t.Errorf("the write failure is not logged:\n%s", logs)
	}
}

func TestFailedInvalidationKeepsStaleReportsAway(t *testing.T) {
	t.Parallel()
	cache := &flakyCache{invalidateErr: errors.New("i/o timeout")}
	s, logs := newCachedService(t, cache)
	if _, err := create(s, 1000, "еда", today); err != nil {
		t.Fatal(err)
	}
	if cache.invalidations.Load() != 1 {
		t.Fatalf("invalidations = %d, want 1 per write", cache.invalidations.Load())
	}
	// The failed invalidation pauses reads for a TTL: a report cached
	// before the write must not be served.
	if r := summary(t, s); r.Cache != usecase.CacheOff || cache.gets.Load() != 0 {
		t.Errorf("report after a failed invalidation: %s with %d cache reads, want it built without the cache", r.Cache, cache.gets.Load())
	}
	if !strings.Contains(logs.String(), "op=invalidate") {
		t.Errorf("the failed invalidation is not logged:\n%s", logs)
	}
}

func TestWritesDoNotWaitForAPausedCache(t *testing.T) {
	t.Parallel()
	cache := &flakyCache{getErr: errors.New("connection refused")}
	s, _ := newCachedService(t, cache)
	summary(t, s) // fails and pauses the cache
	if _, err := create(s, 1, "еда", today); err != nil {
		t.Fatal(err)
	}
	if n := cache.invalidations.Load(); n != 0 {
		t.Errorf("a write called the paused cache %d times, want 0", n)
	}
}

func TestCanceledCallerDoesNotPauseTheCache(t *testing.T) {
	t.Parallel()
	cache := &flakyCache{getErr: context.Canceled}
	s, logs := newCachedService(t, cache)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = s.Summary(ctx, user, oct1, oct31)
	cache.getErr = nil
	summary(t, s)
	if n := cache.gets.Load(); n != 2 {
		t.Errorf("cache reads = %d, want 2: a caller that gave up must not pause the cache", n)
	}
	if strings.Contains(logs.String(), "report cache failed") {
		t.Errorf("a canceled request is logged as a cache failure:\n%s", logs)
	}
}

func TestBrokenEntryIsRebuilt(t *testing.T) {
	t.Parallel()
	cache := &flakyCache{getErr: fmt.Errorf("%w: unknown format", usecase.ErrBrokenEntry)}
	s, logs := newCachedService(t, cache)
	if r := summary(t, s); r.Cache != usecase.CacheMiss || cache.sets.Load() != 1 {
		t.Errorf("broken entry: %s, %d stores; want a MISS that stores the report again", r.Cache, cache.sets.Load())
	}
	summary(t, s)
	if cache.gets.Load() != 2 {
		t.Error("a broken entry paused the cache")
	}
	if !strings.Contains(logs.String(), "broken") {
		t.Errorf("the broken entry is not logged:\n%s", logs)
	}
}

func TestNilLoggerKeepsTheDefault(t *testing.T) {
	t.Parallel()
	cache, _ := redisCache(t)
	s := usecase.New(memory.New(), usecase.WithReportCache(cache, nil))
	if _, err := s.Summary(context.Background(), user, oct1, oct31); err != nil {
		t.Fatal(err)
	}
}
