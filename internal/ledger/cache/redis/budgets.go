package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

func (c *Cache) budgetKey(userID, version int64) string {
	return fmt.Sprintf("budget:v1:list:%s:%d:%d", c.instance, userID, version)
}

func (c *Cache) GetBudgets(ctx context.Context, userID int64) ([]domain.Budget, string, bool, error) {
	version, err := c.version(ctx, userID)
	if err != nil && !errors.Is(err, usecase.ErrBrokenEntry) {
		return nil, "", false, err
	}
	key := c.budgetKey(userID, version)
	if err != nil {
		return nil, key, false, err
	}
	data, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, key, false, nil
	}
	if err != nil {
		return nil, "", false, fmt.Errorf("get budgets: %w", err)
	}
	list, err := decodeBudgets(data, userID)
	if err != nil {
		// Do not put financial data, account IDs or raw JSON errors in logs.
		return nil, key, false, fmt.Errorf("%w: invalid budget encoding", usecase.ErrBrokenEntry)
	}
	return list, key, true, nil
}

// budgetEntry stores money as integer kopecks. The owner is taken from the
// requested key, not from JSON; the cache never combines different users.
type budgetEntry struct {
	Category string `json:"category"`
	Limit    int64  `json:"limit_kopecks"`
	Period   string `json:"period"`
}

func (c *Cache) SetBudgets(ctx context.Context, key string, list []domain.Budget) error {
	entries := make([]budgetEntry, len(list))
	for i, b := range list {
		entries[i] = budgetEntry{Category: b.Category, Limit: int64(b.Limit), Period: string(b.Period)}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode budgets: %w", err)
	}
	if err := c.rdb.Set(ctx, key, data, c.budgetTTL).Err(); err != nil {
		return fmt.Errorf("set budgets: %w", err)
	}
	return nil
}

func decodeBudgets(data []byte, userID int64) ([]domain.Budget, error) {
	var entries []budgetEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if entries == nil || len(entries) > domain.MaxCategories {
		return nil, errors.New("invalid budget list")
	}
	list := make([]domain.Budget, len(entries))
	seen := make(map[string]bool, len(entries))
	for i, entry := range entries {
		b, err := domain.NewBudget(userID, domain.BudgetInput{Category: entry.Category, Limit: money.Money(entry.Limit), Period: entry.Period})
		if err != nil || b.Category != entry.Category || string(b.Period) != entry.Period || seen[entry.Category] {
			return nil, errors.New("invalid budget entry")
		}
		// Preserve the database ordering, which depends on its collation.
		seen[entry.Category] = true
		list[i] = b
	}
	return list, nil
}
