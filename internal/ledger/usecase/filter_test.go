package usecase

import (
	"errors"
	"strings"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/ledger/domain"
)

func TestNormalizeFilterLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      int
		want    int
		wantErr bool
	}{
		{"default", 0, DefaultPageSize, false},
		{"explicit", 5, 5, false},
		{"max", MaxPageSize, MaxPageSize, false},
		{"above max is capped", MaxPageSize + 1, MaxPageSize, false},
		{"export limit is capped too", ExportLimit, MaxPageSize, false},
		{"negative", -1, 0, true},
	}
	for _, tc := range tests {
		got, err := normalizeFilter(TransactionFilter{Limit: tc.in}, DefaultPageSize, MaxPageSize)
		if tc.wantErr {
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("%s: error = %v, want ErrValidation", tc.name, err)
			}
			continue
		}
		if err != nil || got.Limit != tc.want {
			t.Errorf("%s: limit %d -> (%d, %v), want %d", tc.name, tc.in, got.Limit, err, tc.want)
		}
	}
}

func TestCategoryFilterUsesInputRules(t *testing.T) {
	for _, category := range []string{"x" + strings.Repeat("\u0301", 31), "é" + strings.Repeat("\u0301", 30), strings.Repeat("a", 65), "a\x00b"} {
		if _, err := normalizeFilter(TransactionFilter{Category: category}, DefaultPageSize, MaxPageSize); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid category filter: %v", err)
		}
	}
	for _, category := range []string{"", "  ", "cafe\u0301", "é" + strings.Repeat("\u0301", 29)} {
		if _, err := normalizeFilter(TransactionFilter{Category: category}, DefaultPageSize, MaxPageSize); err != nil {
			t.Fatal(err)
		}
	}
}
