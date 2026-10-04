package usecase

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
)

type delayedCache struct {
	mu   sync.Mutex
	sets int
}

func (c *delayedCache) Get(context.Context, int64, civil.Date, civil.Date) (domain.Summary, string, bool, error) {
	return domain.Summary{}, "unchanged-key", false, nil
}
func (c *delayedCache) Set(context.Context, string, domain.Summary) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	return nil
}
func (c *delayedCache) Invalidate(context.Context, int64) error { return errors.New("unavailable") }
func (c *delayedCache) TTL() time.Duration                      { return time.Minute }

// A report started before a write must not renew the lifetime of an old
// cache entry after invalidation failed, even if its build outlives the pause.
func TestFailedInvalidationDiscardsAnEarlierBuild(t *testing.T) {
	for _, expirePause := range []bool{false, true} {
		t.Run(map[bool]string{false: "during pause", true: "after pause"}[expirePause], func(t *testing.T) {
			cache := &delayedCache{}
			s := &Service{reportCache: reportCache{cache: cache, log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			started, finish := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(finish) }) }
			defer release()
			done := make(chan error, 1)
			go func() {
				_, err := s.cachedSummary(context.Background(), 1, civil.Date{}, civil.Date{}, func() (domain.Summary, error) {
					close(started)
					<-finish
					return domain.Summary{Total: 100}, nil
				})
				done <- err
			}()
			<-started
			s.invalidateReports(context.Background(), 1)
			if expirePause {
				s.pausedUntil.Store(0)
			}
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if cache.sets != 0 {
				t.Fatalf("stale report stored %d times after failed invalidation", cache.sets)
			}
		})
	}
}

type activeStoreCache struct {
	delayedCache
	started                chan struct{}
	active                 atomic.Bool
	invalidatedDuringStore atomic.Bool
}

func (c *activeStoreCache) Set(ctx context.Context, _ string, _ domain.Summary) error {
	c.active.Store(true)
	defer c.active.Store(false)
	close(c.started)
	<-ctx.Done()
	return ctx.Err()
}
func (c *activeStoreCache) Invalidate(context.Context, int64) error {
	c.invalidatedDuringStore.Store(c.active.Load())
	return errors.New("unavailable")
}
func TestInvalidationWaitsForActiveStores(t *testing.T) {
	c := &activeStoreCache{started: make(chan struct{})}
	s := &Service{reportCache: reportCache{cache: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.cachedSummary(context.Background(), 1, civil.Date{}, civil.Date{}, func() (domain.Summary, error) { return domain.Summary{}, nil })
	}()
	<-c.started
	s.invalidateReports(context.Background(), 1)
	<-done
	if c.invalidatedDuringStore.Load() {
		t.Fatal("invalidation ran before an active store finished")
	}
	if remaining := time.Until(time.Unix(0, s.pausedUntil.Load())); remaining < c.TTL()-time.Second {
		t.Fatalf("pause must cover a full TTL after the store, got %v", remaining)
	}
}
