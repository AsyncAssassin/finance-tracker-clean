package domain

import (
	"math"
	"slices"
	"strings"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Summary is the expense report for a date range.
type Summary struct {
	From, To   civil.Date
	Total      money.Money       // all expenses in the range
	Categories []CategorySummary // sorted by category
}

// CategorySummary is one row of the report: a category with expenses in
// the range or with a budget.
type CategorySummary struct {
	Category string
	Spent    money.Money
	Budget   *BudgetUsage // nil when the category has no budget
}

// BudgetUsage compares the expenses of a category with its budget.
type BudgetUsage struct {
	Period    Period
	Limit     money.Money // the limit for the whole range, see Budget.LimitFor
	Remaining money.Money // Limit minus spent, never negative
	Percent   float64     // spent as a percentage of Limit, rounded to two decimals
}

// BuildSummary assembles the report for [from, to] from the amount spent per
// category and the budgets of the user. Categories with a budget appear even
// when nothing was spent, so the report shows unused budgets too.
func BuildSummary(from, to civil.Date, spent map[string]money.Money, budgets []Budget) (Summary, error) {
	s := Summary{From: from, To: to, Categories: []CategorySummary{}}
	byCategory := make(map[string]Budget, len(budgets))
	for _, b := range budgets {
		byCategory[b.Category] = b
	}
	seen := make(map[string]bool, len(spent)+len(budgets))
	add := func(category string) error {
		if seen[category] {
			return nil
		}
		seen[category] = true
		row := CategorySummary{Category: category, Spent: spent[category]}
		if b, ok := byCategory[category]; ok {
			count := b.Period.Count(from, to)
			if b.Limit > 0 && count > math.MaxInt64/int64(b.Limit) {
				return money.ErrOverflow
			}
			limit := b.LimitFor(from, to)
			row.Budget = &BudgetUsage{
				Period:    b.Period,
				Limit:     limit,
				Remaining: max(0, limit-row.Spent),
				Percent:   percent(row.Spent, limit),
			}
		}
		var err error
		s.Total, err = money.Add(s.Total, row.Spent)
		if err != nil {
			return err
		}
		s.Categories = append(s.Categories, row)
		return nil
	}
	for category := range spent {
		if err := add(category); err != nil {
			return Summary{}, err
		}
	}
	for _, b := range budgets {
		if err := add(b.Category); err != nil {
			return Summary{}, err
		}
	}
	slices.SortFunc(s.Categories, func(a, b CategorySummary) int {
		return strings.Compare(a.Category, b.Category)
	})
	return s, nil
}

// percent returns part as a percentage of whole, rounded to two decimals.
func percent(part, whole money.Money) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Round(float64(part)*10000/float64(whole)) / 100
}
