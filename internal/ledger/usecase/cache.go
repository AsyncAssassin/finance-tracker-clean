package usecase

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
)

// Cache keeps reports and budget lists. Implementations are safe for concurrent
// use. A failure falls back to the database; writes invalidate both kinds of data.
type Cache interface {
	// Get returns the cached report of the user for [from, to] and whether
	// there was one. On a miss, key is where to Set the report built now.
	// The key belongs to the version of the user's data seen before the
	// build, so a report built from data that changed meanwhile lands
	// under a stale version and is never served.
	Get(ctx context.Context, userID int64, from, to civil.Date) (s domain.Summary, key string, hit bool, err error)
	// Set stores a report under a key from Get until the TTL passes.
	Set(ctx context.Context, key string, s domain.Summary) error
	GetBudgets(ctx context.Context, userID int64) (list []domain.Budget, key string, hit bool, err error)
	SetBudgets(ctx context.Context, key string, list []domain.Budget) error
	// Invalidate makes all cached data of the user stale. The service
	// calls it after the user's transactions or budgets change.
	Invalidate(ctx context.Context, userID int64) error
	// TTL is the longer of the report and budget TTLs. After failed invalidation
	// neither kind of entry may be read until this time has passed.
	TTL() time.Duration
}

// ErrBrokenEntry is wrapped by a Get error that found an entry it cannot
// use, such as one in an unknown format. The service treats it as a miss
// and rebuilds the value under the returned key, without pausing the
// cache.
var ErrBrokenEntry = errors.New("broken cache entry")

// CacheStatus tells how a report was served.
type CacheStatus string

const (
	CacheHit  CacheStatus = "HIT"  // from the cache
	CacheMiss CacheStatus = "MISS" // built and stored in the cache
	CacheOff  CacheStatus = ""     // no cache configured, or it is failing
)

// Report is a summary with the way it was served.
type Report struct {
	domain.Summary
	Cache CacheStatus
}

// cachePause is how long the service stops asking a failing cache, so a
// cache that is down costs one timeout every few seconds rather than one
// per request.
const cachePause = 5 * time.Second

// Timeouts of the cache writes after a request: they run even when the
// client has gone, because the value is built or the data committed.
const (
	storeTimeout      = time.Second
	invalidateTimeout = 2 * time.Second
)

// WithCache makes Summary and ListBudgets use c and log hits, misses and cache
// failures to log; a nil log keeps the default logger.
func WithCache(c Cache, log *slog.Logger) Option {
	return func(s *Service) {
		s.cache = c
		if log != nil {
			s.log = log
		}
	}
}

// readCache is the shared cache state of a Service.
type readCache struct {
	cache Cache
	log   *slog.Logger
	// pausedUntil is the UnixNano time until which reads skip the cache
	// after a failure.
	pausedUntil atomic.Int64
	// Stores may run together; invalidation waits for active stores, then
	// advances generation. An earlier build cannot store after this point.
	writes     sync.RWMutex
	generation atomic.Uint64
}

func (s *Service) cachedSummary(ctx context.Context, userID int64, from, to civil.Date, build func() (domain.Summary, error)) (Report, error) {
	get := func(ctx context.Context) (domain.Summary, string, bool, error) {
		return s.cache.Get(ctx, userID, from, to)
	}
	set := func(ctx context.Context, key string, value domain.Summary) error {
		return s.cache.Set(ctx, key, value)
	}
	sum, status, err := cachedValue(ctx, s, "report", get, build, set)
	return Report{Summary: sum, Cache: status}, err
}

func (s *Service) cachedBudgets(ctx context.Context, userID int64, build func() ([]domain.Budget, error)) ([]domain.Budget, error) {
	get := func(ctx context.Context) ([]domain.Budget, string, bool, error) {
		return s.cache.GetBudgets(ctx, userID)
	}
	set := func(ctx context.Context, key string, value []domain.Budget) error {
		return s.cache.SetBudgets(ctx, key, value)
	}
	list, _, err := cachedValue(ctx, s, "budget", get, build, set)
	return list, err
}

// cachedValue shares the read/build/store coordination between reports and
// budgets. In particular, neither may renew an old entry after invalidation.
func cachedValue[T any](ctx context.Context, s *Service, kind string,
	get func(context.Context) (T, string, bool, error), build func() (T, error),
	set func(context.Context, string, T) error,
) (T, CacheStatus, error) {
	if s.cache == nil || s.cachePaused() {
		value, err := build()
		return value, CacheOff, err
	}
	generation := s.generation.Load()
	attrs := []any{"request_id", requestid.From(ctx)}
	cached, key, hit, err := get(ctx)
	switch {
	case errors.Is(err, ErrBrokenEntry):
		s.log.WarnContext(ctx, kind+" cache entry is broken, rebuilding it", append(attrs, "error", err)...)
	case err != nil:
		// A caller that gave up is not a failure of the cache: pausing
		// it would turn the cache off for everyone.
		if ctx.Err() == nil {
			s.cacheFailed(ctx, kind, "read", err, cachePause)
		}
		value, err := build()
		return value, CacheOff, err
	case hit:
		s.log.DebugContext(ctx, kind+" cache hit", attrs...)
		return cached, CacheHit, nil
	}
	s.log.DebugContext(ctx, kind+" cache miss", attrs...)
	value, err := build()
	if err != nil {
		var zero T
		return zero, CacheOff, err
	}
	s.writes.RLock()
	defer s.writes.RUnlock()
	if s.cachePaused() || s.generation.Load() != generation {
		return value, CacheOff, nil
	}
	// The value is built: store it even if the caller has gone meanwhile.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := set(storeCtx, key, value); err != nil {
		s.cacheFailed(ctx, kind, "write", err, cachePause)
	}
	return value, CacheMiss, nil
}

// invalidateCache marks the user's reports and budgets stale after a write.
// When the cache is failing, or the invalidation itself fails, a value
// cached before the write could be served later; so instead reads skip the
// cache for a whole TTL after active stores finish. Earlier builds cannot
// renew those entries because generation has changed. While
// the cache is paused, writes do not wait for it.
func (s *Service) invalidateCache(ctx context.Context, userID int64) {
	if s.cache == nil {
		return
	}
	s.writes.Lock()
	defer s.writes.Unlock()
	s.generation.Add(1)
	if s.cachePaused() {
		s.pauseFor(s.cache.TTL())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), invalidateTimeout)
	defer cancel()
	if err := s.cache.Invalidate(ctx, userID); err != nil {
		s.cacheFailed(ctx, "ledger", "invalidate", err, s.cache.TTL())
	}
}

func (s *Service) cachePaused() bool {
	return time.Now().UnixNano() < s.pausedUntil.Load()
}

// pauseFor makes reads skip the cache for at least d from now. It only
// ever extends a pause, so a short pause cannot cut a long one.
func (s *Service) pauseFor(d time.Duration) {
	until := time.Now().Add(d).UnixNano()
	for {
		cur := s.pausedUntil.Load()
		if cur >= until || s.pausedUntil.CompareAndSwap(cur, until) {
			return
		}
	}
}

func (s *Service) cacheFailed(ctx context.Context, kind, op string, err error, pause time.Duration) {
	s.pauseFor(pause)
	s.log.WarnContext(ctx, kind+" cache failed, reading from the database for a while",
		"op", op, "error", err, "pause", pause.String(), "request_id", requestid.From(ctx))
}
