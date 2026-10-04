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

// ReportCache keeps built reports, so a repeated request for the same range
// skips the database. Implementations are safe for concurrent use. The
// cache is an optimization: when it fails, the service logs the error and
// builds the report from the database.
type ReportCache interface {
	// Get returns the cached report of the user for [from, to] and whether
	// there was one. On a miss, key is where to Set the report built now.
	// The key belongs to the version of the user's data seen before the
	// build, so a report built from data that changed meanwhile lands
	// under a stale version and is never served.
	Get(ctx context.Context, userID int64, from, to civil.Date) (s domain.Summary, key string, hit bool, err error)
	// Set stores a report under a key from Get until the TTL passes.
	Set(ctx context.Context, key string, s domain.Summary) error
	// Invalidate makes every cached report of the user stale. The service
	// calls it after the user's transactions or budgets change.
	Invalidate(ctx context.Context, userID int64) error
	// TTL is how long a stored report lives.
	TTL() time.Duration
}

// ErrBrokenEntry is wrapped by a Get error that found an entry it cannot
// use, such as one in an unknown format. The service treats it as a miss
// and rebuilds the report under the returned key, without pausing the
// cache.
var ErrBrokenEntry = errors.New("broken report cache entry")

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
// per report.
const cachePause = 5 * time.Second

// Timeouts of the cache writes after a request: they run even when the
// client has gone, because the report is built and the data committed.
const (
	storeTimeout      = time.Second
	invalidateTimeout = 2 * time.Second
)

// WithReportCache makes Summary use c and log hits, misses and cache
// failures to log; a nil log keeps the default logger.
func WithReportCache(c ReportCache, log *slog.Logger) Option {
	return func(s *Service) {
		s.cache = c
		if log != nil {
			s.log = log
		}
	}
}

// reportCache is the cache state of a Service.
type reportCache struct {
	cache ReportCache
	log   *slog.Logger
	// pausedUntil is the UnixNano time until which Summary skips the cache
	// after a failure.
	pausedUntil atomic.Int64
	// Stores may run together; invalidation waits for active stores, then
	// advances generation. An earlier build cannot store after this point.
	writes     sync.RWMutex
	generation atomic.Uint64
}

// cachedSummary serves the report from the cache or builds it with build
// and stores it.
func (s *Service) cachedSummary(ctx context.Context, userID int64, from, to civil.Date, build func() (domain.Summary, error)) (Report, error) {
	if s.cache == nil || s.cachePaused() {
		sum, err := build()
		return Report{Summary: sum, Cache: CacheOff}, err
	}
	generation := s.generation.Load()
	attrs := []any{"request_id", requestid.From(ctx)}
	cached, key, hit, err := s.cache.Get(ctx, userID, from, to)
	switch {
	case errors.Is(err, ErrBrokenEntry):
		s.log.WarnContext(ctx, "report cache entry is broken, rebuilding it", append(attrs, "error", err)...)
	case err != nil:
		// A caller that gave up is not a failure of the cache: pausing
		// it would turn the cache off for everyone.
		if ctx.Err() == nil {
			s.cacheFailed(ctx, "read", err, cachePause)
		}
		sum, err := build()
		return Report{Summary: sum, Cache: CacheOff}, err
	case hit:
		s.log.DebugContext(ctx, "report cache hit", attrs...)
		return Report{Summary: cached, Cache: CacheHit}, nil
	}
	s.log.DebugContext(ctx, "report cache miss", attrs...)
	sum, err := build()
	if err != nil {
		return Report{}, err
	}
	s.writes.RLock()
	defer s.writes.RUnlock()
	if s.cachePaused() || s.generation.Load() != generation {
		return Report{Summary: sum, Cache: CacheOff}, nil
	}
	// The report is built: store it even if the caller has gone meanwhile.
	storeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := s.cache.Set(storeCtx, key, sum); err != nil {
		s.cacheFailed(ctx, "write", err, cachePause)
	}
	return Report{Summary: sum, Cache: CacheMiss}, nil
}

// invalidateReports marks the user's cached reports stale after a write.
// When the cache is failing, or the invalidation itself fails, a report
// cached before the write could be served later; so instead reads skip the
// cache for a whole TTL after active stores finish. Earlier builds cannot
// renew those entries because generation has changed. While
// the cache is paused, writes do not wait for it.
func (s *Service) invalidateReports(ctx context.Context, userID int64) {
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
		s.cacheFailed(ctx, "invalidate", err, s.cache.TTL())
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

func (s *Service) cacheFailed(ctx context.Context, op string, err error, pause time.Duration) {
	s.pauseFor(pause)
	s.log.WarnContext(ctx, "report cache failed, building reports without it for a while",
		"op", op, "error", err, "pause", pause.String(), "request_id", requestid.From(ctx))
}
