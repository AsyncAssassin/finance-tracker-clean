// Package redis caches the ledger's reports and budget lists in Redis.
//
// Keys:
//
//	report:v2:ver:{user}                          version of the user's data, INCR on every change
//	report:v2:summary:{instance}:{user}:{version}:{from}:{to}  a built report, JSON, expires after the TTL
//	budget:v1:list:{instance}:{user}:{version}     budget list, JSON, expires after its own TTL
//
// Data is looked up under the current version, so incrementing it invalidates
// all reports and budget lists of the user without scanning keys; the old ones
// simply expire. Version keys have no TTL, so volatile-lru evicts cached data
// under memory pressure but never versions: a lost version could revive old data.
package redis

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// prefix versions the key layout and the JSON format: a change of either
// takes a new prefix, and old entries are never read.
const prefix = "report:v2:"

// Cache stores reports and budget lists in Redis with separate TTLs.
type Cache struct {
	rdb       *redis.Client
	ttl       time.Duration
	budgetTTL time.Duration
	instance  string
}

var _ usecase.Cache = (*Cache)(nil)

// New returns a Cache on rdb.
func New(rdb *redis.Client, ttl, budgetTTL time.Duration) *Cache {
	// A restart loses the service pause after a failed invalidation.
	// Never reuse data whose validity depended on the previous process.
	return &Cache{rdb: rdb, ttl: ttl, budgetTTL: budgetTTL, instance: rand.Text()}
}

// Open returns a client for a validated host:port and database, with short timeouts and no
// retries: a cache that answers slowly is worse than none, and the ledger
// falls back to the database on any error.
func Open(addr string, db int, password string) *redis.Client {
	opts := &redis.Options{Addr: addr, DB: db, Password: password}
	opts.DialTimeout = 500 * time.Millisecond
	opts.DialerRetries = 1 // one attempt per dial, not the default five
	opts.ReadTimeout = 300 * time.Millisecond
	opts.WriteTimeout = 300 * time.Millisecond
	opts.MaxRetries = -1 // no retries of a failed command
	return redis.NewClient(opts)
}

// UseLogger sends the messages of the Redis client, such as failed dials,
// to log at debug level instead of plain lines on stderr. The ledger logs
// cache failures itself as warnings. It affects every Redis client in the
// process, so the command calls it once at startup.
func UseLogger(log *slog.Logger) {
	redis.SetLogger(slogPrinter{log})
}

type slogPrinter struct{ log *slog.Logger }

func (p slogPrinter) Printf(ctx context.Context, format string, v ...any) {
	p.log.DebugContext(ctx, "redis client", "message", fmt.Sprintf(format, v...))
}

func versionKey(userID int64) string {
	return prefix + "ver:" + strconv.FormatInt(userID, 10)
}

func (c *Cache) reportKey(userID, version int64, from, to civil.Date) string {
	return fmt.Sprintf("%ssummary:%s:%d:%d:%s:%s", prefix, c.instance, userID, version, from, to)
}

func (c *Cache) version(ctx context.Context, userID int64) (int64, error) {
	version, err := c.rdb.Get(ctx, versionKey(userID)).Int64()
	var notNumber *strconv.NumError
	switch {
	case errors.As(err, &notNumber):
		// Not a version this cache wrote. Replace it with a value no
		// report key has used, so whatever was cached becomes stale.
		version = time.Now().UnixNano()
		if err := c.rdb.Set(ctx, versionKey(userID), version, 0).Err(); err != nil {
			return 0, fmt.Errorf("reset version: %w", err)
		}
		return version, fmt.Errorf("%w: invalid version was reset", usecase.ErrBrokenEntry)
	case err != nil && !errors.Is(err, redis.Nil):
		return 0, fmt.Errorf("get version: %w", err)
	}
	return version, nil
}

func (c *Cache) Get(ctx context.Context, userID int64, from, to civil.Date) (domain.Summary, string, bool, error) {
	version, err := c.version(ctx, userID)
	if err != nil && !errors.Is(err, usecase.ErrBrokenEntry) {
		return domain.Summary{}, "", false, err
	}
	key := c.reportKey(userID, version, from, to)
	if err != nil {
		return domain.Summary{}, key, false, err
	}
	data, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return domain.Summary{}, key, false, nil
	}
	if err != nil {
		return domain.Summary{}, "", false, fmt.Errorf("get report: %w", err)
	}
	s, err := decode(data)
	if err != nil {
		return domain.Summary{}, key, false, fmt.Errorf("%w: invalid report encoding", usecase.ErrBrokenEntry)
	}
	return s, key, true, nil
}

func (c *Cache) Set(ctx context.Context, key string, s domain.Summary) error {
	data, err := json.Marshal(encode(s))
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := c.rdb.Set(ctx, key, data, c.ttl).Err(); err != nil {
		return fmt.Errorf("set report: %w", err)
	}
	return nil
}

// TTL covers both kinds of cached data after failed invalidation.
func (c *Cache) TTL() time.Duration { return max(c.ttl, c.budgetTTL) }

func (c *Cache) Invalidate(ctx context.Context, userID int64) error {
	if err := c.rdb.Incr(ctx, versionKey(userID)).Err(); err != nil {
		return fmt.Errorf("increment version: %w", err)
	}
	return nil
}

// The JSON form keeps money in kopecks and dates as text, independent of
// how the domain types marshal.
type entry struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Total      int64  `json:"total"`
	Categories []row  `json:"categories"`
}

type row struct {
	Category string `json:"category"`
	Spent    int64  `json:"spent"`
	Budget   *usage `json:"budget,omitempty"`
}

type usage struct {
	Period    string  `json:"period"`
	Limit     int64   `json:"limit"`
	Remaining int64   `json:"remaining"`
	Percent   float64 `json:"percent"`
}

func encode(s domain.Summary) entry {
	e := entry{From: s.From.String(), To: s.To.String(), Total: int64(s.Total), Categories: make([]row, len(s.Categories))}
	for i, c := range s.Categories {
		r := row{Category: c.Category, Spent: int64(c.Spent)}
		if b := c.Budget; b != nil {
			r.Budget = &usage{Period: string(b.Period), Limit: int64(b.Limit), Remaining: int64(b.Remaining), Percent: b.Percent}
		}
		e.Categories[i] = r
	}
	return e
}

func decode(data []byte) (domain.Summary, error) {
	var e entry
	if err := json.Unmarshal(data, &e); err != nil {
		return domain.Summary{}, err
	}
	from, err := civil.Parse(e.From)
	if err != nil {
		return domain.Summary{}, err
	}
	to, err := civil.Parse(e.To)
	if err != nil {
		return domain.Summary{}, err
	}
	s := domain.Summary{From: from, To: to, Total: money.Money(e.Total), Categories: make([]domain.CategorySummary, len(e.Categories))}
	for i, r := range e.Categories {
		c := domain.CategorySummary{Category: r.Category, Spent: money.Money(r.Spent)}
		if b := r.Budget; b != nil {
			c.Budget = &domain.BudgetUsage{Period: domain.Period(b.Period), Limit: money.Money(b.Limit), Remaining: money.Money(b.Remaining), Percent: b.Percent}
		}
		s.Categories[i] = c
	}
	return s, nil
}
