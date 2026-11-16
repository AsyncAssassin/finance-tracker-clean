package redis

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
)

func TestBudgetCache(t *testing.T) {
	for _, backend := range backends(t) {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			c := New(backend.rdb, time.Minute, 20*time.Second)
			want := []domain.Budget{
				{UserID: 1, Category: "еда", Limit: 12345, Period: domain.PeriodMonth},
				// Locale-based database ordering can differ from Go byte ordering.
				{UserID: 1, Category: "ёж", Limit: 34567, Period: domain.PeriodNone},
				{UserID: 1, Category: "транспорт", Limit: 23456, Period: domain.PeriodYear},
			}
			_, key, hit, err := c.GetBudgets(ctx, 1)
			if err != nil || hit || key == "" {
				t.Fatalf("first read: key=%q hit=%v err=%v", key, hit, err)
			}
			if err := c.SetBudgets(ctx, key, want); err != nil {
				t.Fatal(err)
			}
			got, _, hit, err := c.GetBudgets(ctx, 1)
			if err != nil || !hit || !reflect.DeepEqual(got, want) {
				t.Fatalf("cached budgets: %+v hit=%v err=%v", got, hit, err)
			}
			if ttl := backend.rdb.PTTL(ctx, key).Val(); ttl <= 0 || ttl > 20*time.Second {
				t.Fatalf("budget TTL: %s", ttl)
			}
			if _, otherKey, hit, err := c.GetBudgets(ctx, 2); err != nil || hit || key == otherKey {
				t.Fatalf("another user: key=%q hit=%v err=%v", otherKey, hit, err)
			}
			if err := c.SetBudgets(ctx, key, nil); err != nil {
				t.Fatal(err)
			}
			got, _, hit, err = c.GetBudgets(ctx, 1)
			if err != nil || !hit || got == nil || len(got) != 0 {
				t.Fatalf("cached empty list: %#v hit=%v err=%v", got, hit, err)
			}
			if err := c.Invalidate(ctx, 1); err != nil {
				t.Fatal(err)
			}
			_, nextKey, hit, err := c.GetBudgets(ctx, 1)
			if err != nil || hit || nextKey == key {
				t.Fatalf("invalidation: key=%q hit=%v err=%v", nextKey, hit, err)
			}
			// Finishing an earlier read cannot populate the new version.
			if err := c.SetBudgets(ctx, key, want); err != nil {
				t.Fatal(err)
			}
			if _, _, hit, err := c.GetBudgets(ctx, 1); err != nil || hit {
				t.Fatalf("old version returned: %v %v", hit, err)
			}
			if err := c.SetBudgets(ctx, nextKey, want); err != nil {
				t.Fatal(err)
			}
			next := New(backend.rdb, time.Minute, 20*time.Second)
			if _, restartedKey, hit, err := next.GetBudgets(ctx, 1); err != nil || hit || restartedKey == nextKey {
				t.Fatalf("restart reused entry: %v %v", hit, err)
			}
		})
	}
}

func TestBudgetExpiryAndLongestTTL(t *testing.T) {
	for _, backend := range backends(t) {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			// Budgets may have the longer TTL; failure recovery must cover it.
			c := New(backend.rdb, time.Second, 2*time.Second)
			if c.TTL() != 2*time.Second {
				t.Fatal("invalidation does not cover both TTLs")
			}
			_, key, _, _ := c.GetBudgets(ctx, 1)
			if err := c.SetBudgets(ctx, key, nil); err != nil {
				t.Fatal(err)
			}
			if backend.advance != nil {
				backend.advance(3 * time.Second)
			} else {
				time.Sleep(2100 * time.Millisecond)
			}
			if _, _, hit, err := c.GetBudgets(ctx, 1); err != nil || hit {
				t.Fatalf("expired list: hit=%v err=%v", hit, err)
			}
		})
	}
}

func TestBrokenBudgetEntry(t *testing.T) {
	backend := backends(t)[0]
	c := New(backend.rdb, time.Minute, 20*time.Second)
	ctx := context.Background()
	_, key, _, _ := c.GetBudgets(ctx, 987654321)
	for _, data := range []string{
		`{"private-marker":`, `null`, `{}`, `[{}]`,
		`[{"category":"food","limit_kopecks":-1,"period":"month"}]`,
		`[{"category":"food","limit_kopecks":1,"period":"week"}]`,
		`[{"category":" Food ","limit_kopecks":1,"period":"month"}]`,
		`[{"category":"food","limit_kopecks":1,"period":"month"},{"category":"food","limit_kopecks":2,"period":"month"}]`,
		`[{"category":"food","limit_kopecks":1,"period":"month"},{"category":"travel","limit_kopecks":1,"period":"month"},{"category":"food","limit_kopecks":2,"period":"month"}]`,
	} {
		if err := backend.rdb.Set(ctx, key, data, time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
		_, gotKey, hit, err := c.GetBudgets(ctx, 987654321)
		if hit || gotKey != key || !errors.Is(err, usecase.ErrBrokenEntry) {
			t.Fatalf("bad data accepted: hit=%v err=%v", hit, err)
		}
		if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "987654321") || strings.Contains(err.Error(), key) {
			t.Fatal("error exposes cached data")
		}
	}
	if err := backend.rdb.Set(ctx, versionKey(987654321), "private-marker", 0).Err(); err != nil {
		t.Fatal(err)
	}
	_, newKey, hit, err := c.GetBudgets(ctx, 987654321)
	if hit || newKey == key || !errors.Is(err, usecase.ErrBrokenEntry) {
		t.Fatalf("bad version: %v %v", hit, err)
	}
	if err := c.SetBudgets(ctx, newKey, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, hit, err := c.GetBudgets(ctx, 987654321); err != nil || !hit {
		t.Fatalf("recovery: %v %v", hit, err)
	}
}

func TestBudgetCacheUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := Open(mr.Addr(), 0, "")
	t.Cleanup(func() { _ = rdb.Close() })
	c := New(rdb, time.Minute, 20*time.Second)
	mr.Close()
	if _, _, _, err := c.GetBudgets(context.Background(), 1); err == nil {
		t.Fatal("read accepted unavailable Redis")
	}
	if err := c.SetBudgets(context.Background(), "key", nil); err == nil {
		t.Fatal("write accepted unavailable Redis")
	}
}

func TestOpenPasswordAndDatabase(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("synthetic-redis-password")
	rdb := Open(mr.Addr(), 3, "synthetic-redis-password")
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Set(context.Background(), "probe", "ok", time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	if mr.DB(0).Exists("probe") || !mr.DB(3).Exists("probe") {
		t.Fatal("wrong database")
	}
}
