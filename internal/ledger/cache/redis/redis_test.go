package redis

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

const ttl = time.Minute

func TestInvalidURLDoesNotExposeCredentials(t *testing.T) {
	t.Parallel()
	const private = "private-input-marker"
	for _, suffix := range []string{"@localhost:bad/0", "@localhost/" + private, "@localhost/0?db=" + private} {
		_, err := Open("redis://user:"+private+suffix, "")
		if err == nil {
			t.Fatal("invalid URL accepted")
		}
		if strings.Contains(err.Error(), private) {
			t.Fatal("connection credentials or input reached a loggable error")
		}
	}
}

var (
	oct1  = civil.New(2026, time.October, 1)
	oct31 = civil.New(2026, time.October, 31)
)

func sample() domain.Summary {
	return domain.Summary{
		From: oct1, To: oct31, Total: 545050,
		Categories: []domain.CategorySummary{
			{Category: "еда", Spent: 475050, Budget: &domain.BudgetUsage{Period: domain.PeriodMonth, Limit: 500000, Remaining: 24950, Percent: 95.01}},
			{Category: "здоровье", Spent: 70000},
		},
	}
}

// backend is a Redis the tests run against, with a way to move time.
type backend struct {
	name    string
	rdb     *redis.Client
	advance func(time.Duration) // makes keys older; nil when not possible
	stop    func()              // takes Redis down
}

// backends returns miniredis always and a real Redis when TEST_REDIS_URL
// is set. The real one gets its database flushed.
func backends(t *testing.T) []backend {
	t.Helper()
	mr := miniredis.RunT(t)
	mini := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = mini.Close() })
	list := []backend{{name: "miniredis", rdb: mini, advance: mr.FastForward, stop: mr.Close}}
	if url := os.Getenv("TEST_REDIS_URL"); url != "" {
		real, err := Open(url, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := real.FlushDB(context.Background()).Err(); err != nil {
			t.Fatalf("flush the test database: %v", err)
		}
		t.Cleanup(func() { _ = real.Close() })
		list = append(list, backend{name: "redis", rdb: real})
	}
	return list
}

func TestCache(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			c := New(b.rdb, ttl)
			ctx := context.Background()

			_, key, hit, err := c.Get(ctx, 1, oct1, oct31)
			if err != nil || hit || key == "" {
				t.Fatalf("first Get = key %q, hit %v, %v; want a miss with a key", key, hit, err)
			}
			if err := c.Set(ctx, key, sample()); err != nil {
				t.Fatal(err)
			}
			got, _, hit, err := c.Get(ctx, 1, oct1, oct31)
			if err != nil || !hit || !reflect.DeepEqual(got, sample()) {
				t.Fatalf("Get after Set = %+v, hit %v, %v", got, hit, err)
			}
			if d := b.rdb.TTL(ctx, key).Val(); d <= 0 || d > ttl {
				t.Errorf("TTL of the report = %s, want up to %s", d, ttl)
			}

			// Another range and another user are separate entries.
			if _, _, hit, _ := c.Get(ctx, 1, oct1, oct1); hit {
				t.Error("another range hits the cache")
			}
			if _, _, hit, _ := c.Get(ctx, 2, oct1, oct31); hit {
				t.Error("another user hits the cache")
			}

			if err := c.Invalidate(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if _, newKey, hit, _ := c.Get(ctx, 1, oct1, oct31); hit || newKey == key {
				t.Errorf("after Invalidate: hit %v, key %q (old %q); want a miss under a new key", hit, newKey, key)
			}
		})
	}
}

// TestStaleWriteIsNeverServed covers the race the versions exist for: a
// report built from data read before a change is stored after the change.
func TestStaleWriteIsNeverServed(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			c := New(b.rdb, ttl)
			ctx := context.Background()
			_, key, _, _ := c.Get(ctx, 1, oct1, oct31)   // a report starts building
			if err := c.Invalidate(ctx, 1); err != nil { // a transaction commits meanwhile
				t.Fatal(err)
			}
			if err := c.Set(ctx, key, sample()); err != nil { // the old report is stored
				t.Fatal(err)
			}
			if _, _, hit, _ := c.Get(ctx, 1, oct1, oct31); hit {
				t.Error("a report built before the change is served after it")
			}
		})
	}
}

func TestExpiry(t *testing.T) {
	b := backends(t)[0] // only miniredis can move time
	c := New(b.rdb, ttl)
	ctx := context.Background()
	_, key, _, _ := c.Get(ctx, 1, oct1, oct31)
	if err := c.Set(ctx, key, sample()); err != nil {
		t.Fatal(err)
	}
	b.advance(ttl + time.Second)
	if _, _, hit, _ := c.Get(ctx, 1, oct1, oct31); hit {
		t.Error("an expired report is served")
	}
}

func TestBrokenEntryIsAMiss(t *testing.T) {
	b := backends(t)[0]
	c := New(b.rdb, ttl)
	ctx := context.Background()
	_, key, _, _ := c.Get(ctx, 1, oct1, oct31)
	b.rdb.Set(ctx, key, "{not json", ttl)
	if _, k, hit, err := c.Get(ctx, 1, oct1, oct31); hit || !errors.Is(err, usecase.ErrBrokenEntry) || k != key {
		t.Errorf("broken entry: hit %v, key %q, %v; want ErrBrokenEntry and a miss under the same key", hit, k, err)
	}
}

func TestBrokenVersionIsReset(t *testing.T) {
	b := backends(t)[0]
	c := New(b.rdb, ttl)
	ctx := context.Background()
	_, key, _, _ := c.Get(ctx, 1, oct1, oct31)
	if err := c.Set(ctx, key, sample()); err != nil {
		t.Fatal(err)
	}
	b.rdb.Set(ctx, versionKey(1), "not a number", 0)
	_, newKey, hit, err := c.Get(ctx, 1, oct1, oct31)
	if hit || !errors.Is(err, usecase.ErrBrokenEntry) || newKey == key {
		t.Fatalf("broken version: hit %v, key %q, %v; want ErrBrokenEntry and a new key", hit, newKey, err)
	}
	// The version is a number again, so the cache works from now on.
	if err := c.Set(ctx, newKey, sample()); err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := c.Get(ctx, 1, oct1, oct31); !hit || err != nil {
		t.Errorf("after the reset: hit %v, %v; want a hit", hit, err)
	}
	if err := c.Invalidate(ctx, 1); err != nil {
		t.Errorf("Invalidate after the reset: %v", err)
	}
	if c.TTL() != ttl {
		t.Errorf("TTL() = %s", c.TTL())
	}
}

func TestRedisDown(t *testing.T) {
	b := backends(t)[0]
	c := New(b.rdb, ttl)
	b.stop()
	ctx := context.Background()
	if _, _, _, err := c.Get(ctx, 1, oct1, oct31); err == nil {
		t.Error("Get with Redis down returned no error")
	}
	if err := c.Set(ctx, "k", sample()); err == nil {
		t.Error("Set with Redis down returned no error")
	}
	if err := c.Invalidate(ctx, 1); err == nil {
		t.Error("Invalidate with Redis down returned no error")
	}
}

func TestOpen(t *testing.T) {
	if _, err := Open("http://not-redis", ""); err == nil {
		t.Error("Open accepted a non-redis URL")
	}
	rdb, err := Open("redis://localhost:6379/3", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	o := rdb.Options()
	// go-redis stores MaxRetries -1 ("no retries") as 0.
	if o.DB != 3 || o.MaxRetries > 0 || o.DialerRetries != 1 || o.ReadTimeout > time.Second || o.DialTimeout > time.Second {
		t.Errorf("options = db %d, retries %d, dial attempts %d, read %s, dial %s", o.DB, o.MaxRetries, o.DialerRetries, o.ReadTimeout, o.DialTimeout)
	}
}

func TestUseLogger(t *testing.T) {
	var buf strings.Builder
	UseLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer UseLogger(slog.New(slog.DiscardHandler))
	rdb, _ := Open("redis://127.0.0.1:1/0", "") // nothing listens on port 1
	defer rdb.Close()
	_ = rdb.Ping(context.Background()).Err()
	if !strings.Contains(buf.String(), "redis client") {
		t.Errorf("the client's messages did not reach slog: %q", buf.String())
	}
}

func TestKeyLayout(t *testing.T) {
	c := &Cache{instance: "test-instance"}
	if got := c.reportKey(7, 3, oct1, oct31); got != "report:v2:summary:test-instance:7:3:2026-10-01:2026-10-31" {
		t.Errorf("reportKey = %q", got)
	}
	if got := versionKey(7); !strings.HasPrefix(got, prefix) || got != "report:v2:ver:7" {
		t.Errorf("versionKey = %q", got)
	}
}

// A service restart loses the local pause after failed invalidation. Its
// new cache instance must not reuse a possibly stale report from before it.
func TestNewInstanceDoesNotReusePreviousReports(t *testing.T) {
	b := backends(t)[0]
	old := New(b.rdb, ttl)
	ctx := context.Background()
	_, key, _, err := old.Get(ctx, 1, oct1, oct31)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Set(ctx, key, sample()); err != nil {
		t.Fatal(err)
	}
	next := New(b.rdb, ttl)
	_, newKey, hit, err := next.Get(ctx, 1, oct1, oct31)
	if err != nil || hit || newKey == key {
		t.Fatalf("new instance reused an old report: hit=%v sameKey=%v err=%v", hit, newKey == key, err)
	}
}

func TestBrokenCacheErrorDoesNotExposeData(t *testing.T) {
	b := backends(t)[0]
	c := New(b.rdb, ttl)
	ctx := context.Background()
	const userID = 987654321
	const marker = "private-cache-marker"
	_, key, _, _ := c.Get(ctx, userID, oct1, oct31)
	for _, data := range []string{"{" + marker, `{"From":"` + marker + `"}`, `{"Total":"` + marker + `"}`} {
		if err := b.rdb.Set(ctx, key, data, ttl).Err(); err != nil {
			t.Fatal(err)
		}
		_, _, hit, err := c.Get(ctx, userID, oct1, oct31)
		if hit || !errors.Is(err, usecase.ErrBrokenEntry) {
			t.Fatalf("corruption not detected: %v", err)
		}
		for _, private := range []string{marker, "987654321", key, oct1.String(), oct31.String()} {
			if strings.Contains(err.Error(), private) {
				t.Fatal("cache data reached a loggable error")
			}
		}
	}
	if err := b.rdb.Set(ctx, versionKey(userID), marker, 0).Err(); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := c.Get(ctx, userID, oct1, oct31)
	if !errors.Is(err, usecase.ErrBrokenEntry) || strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "987654321") {
		t.Fatalf("unsafe version error: %v", err)
	}
}
