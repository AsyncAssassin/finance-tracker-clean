package domain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

func TestNewTransaction(t *testing.T) {
	t.Parallel()
	today := civil.New(2026, time.October, 3)
	tests := []struct {
		name       string
		in         TransactionInput
		want       Transaction
		wantFields []string
	}{
		{
			name: "full input is normalized",
			in:   TransactionInput{Amount: 125050, Category: "  Еда   Дома ", Description: "  продукты ", Date: civil.New(2026, time.September, 30), ExternalID: " row-1 "},
			want: Transaction{UserID: 7, Amount: 125050, Category: "еда дома", Description: "продукты", Date: civil.New(2026, time.September, 30), ExternalID: "row-1"},
		},
		{
			name: "zero date means today",
			in:   TransactionInput{Amount: 1, Category: "еда"},
			want: Transaction{UserID: 7, Amount: 1, Category: "еда", Date: today},
		},
		{
			name: "the first day of year 1 is kept, not replaced by today",
			in:   TransactionInput{Amount: 1, Category: "еда", Date: MinDate},
			want: Transaction{UserID: 7, Amount: 1, Category: "еда", Date: MinDate},
		},
		{name: "zero amount", in: TransactionInput{Category: "еда"}, wantFields: []string{"amount"}},
		{name: "negative amount", in: TransactionInput{Amount: -1, Category: "еда"}, wantFields: []string{"amount"}},
		{name: "amount above max", in: TransactionInput{Amount: money.Max + 1, Category: "еда"}, wantFields: []string{"amount"}},
		{name: "empty category", in: TransactionInput{Amount: 1, Category: " "}, wantFields: []string{"category"}},
		{name: "long category", in: TransactionInput{Amount: 1, Category: strings.Repeat("я", MaxCategoryLen+1)}, wantFields: []string{"category"}},
		{name: "long description", in: TransactionInput{Amount: 1, Category: "еда", Description: strings.Repeat("я", MaxDescriptionLen+1)}, wantFields: []string{"description"}},
		{name: "long external id", in: TransactionInput{Amount: 1, Category: "еда", ExternalID: strings.Repeat("x", MaxExternalIDLen+1)}, wantFields: []string{"external_id"}},
		{name: "date before year 1", in: TransactionInput{Amount: 1, Category: "еда", Date: civil.New(0, time.June, 15)}, wantFields: []string{"date"}},
		{name: "date after year 9999", in: TransactionInput{Amount: 1, Category: "еда", Date: civil.New(10000, time.January, 1)}, wantFields: []string{"date"}},
		{name: "everything wrong", in: TransactionInput{}, wantFields: []string{"amount", "category"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewTransaction(7, tc.in, today)
			if len(tc.wantFields) > 0 {
				assertValidationFields(t, err, tc.wantFields...)
				return
			}
			if err != nil {
				t.Fatalf("NewTransaction returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("NewTransaction = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLongTextAtTheLimitIsAccepted(t *testing.T) {
	t.Parallel()
	in := TransactionInput{
		Amount:      1,
		Category:    strings.Repeat("я", MaxCategoryLen),
		Description: strings.Repeat("я", MaxDescriptionLen),
		ExternalID:  strings.Repeat("x", MaxExternalIDLen),
	}
	if _, err := NewTransaction(1, in, civil.New(2026, time.October, 3)); err != nil {
		t.Fatalf("text at the limit was rejected: %v", err)
	}
}

func TestNormalizeCategory(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"еда":              "еда",
		"Еда":              "еда",
		"  ЕДА  ":          "еда",
		"Еда \t дома":      "еда дома",
		"Transport/Taxi":   "transport/taxi",
		"Кафе и рестораны": "кафе и рестораны",
		"café":             "café",
		"cafe\u0301":       "café",
		"\u0130":           "i",
		"I\u0307":          "i",
	}
	for in, want := range tests {
		if got, err := NormalizeCategory(in); err != nil || got != want {
			t.Errorf("NormalizeCategory(%q) = %q, want %q", in, got, want)
		}
		if got, err := NormalizeCategory(want); err != nil || got != want {
			t.Errorf("normalization changed its own result: %q -> %q", want, got)
		}
	}
}

func TestDuplicateError(t *testing.T) {
	t.Parallel()
	var err error = &DuplicateError{ExternalID: "row-1", ExistingID: 5}
	if !errors.Is(err, ErrAlreadyExists) {
		t.Error("errors.Is(err, ErrAlreadyExists) = false")
	}
	if got := err.Error(); got != `transaction with external_id "row-1" already exists as #5` {
		t.Errorf("Error() = %q", got)
	}
}

func TestTextFieldsRejectNULAndInvalidUTF8(t *testing.T) {
	for _, bad := range []string{"a\x00b", "a\xffb"} {
		for _, field := range []string{"category", "description", "external_id"} {
			in := TransactionInput{Amount: 1, Category: "food"}
			switch field {
			case "category":
				in.Category = bad
			case "description":
				in.Description = bad
			case "external_id":
				in.ExternalID = bad
			}
			_, err := NewTransaction(1, in, MinDate)
			assertValidationFields(t, err, field)
		}
		_, err := NewBudget(1, BudgetInput{Category: bad, Limit: 1})
		assertValidationFields(t, err, "category")
	}
}

func TestCategoryCombiningSequences(t *testing.T) {
	tests := []struct {
		name, category string
		valid          bool
	}{
		{"30 marks", "x" + strings.Repeat("\u0301", 30), true},
		{"31 marks", "x" + strings.Repeat("\u0301", 31), false},
		{"reordered tail", "x" + strings.Repeat("\u0301", 31) + "\u0327", false},
		{"reordered head", "x\u0327" + strings.Repeat("\u0301", 31), false},
		{"precomposed at limit", "é" + strings.Repeat("\u0301", 29), true},
		{"precomposed over limit", "é" + strings.Repeat("\u0301", 30), false},
		{"other Unicode block", "x" + strings.Repeat("\u1ab0", 31), false},
		{"existing separator", "x" + strings.Repeat("\u0301", 30) + "\u034f\u0301", true},
		{"leading marks at limit", strings.Repeat("\u0301", 30), true},
		{"leading marks over limit", strings.Repeat("\u0301", 31), false},
		{"empty", " ", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			normalized, err := NormalizeCategory(tc.category)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if tc.valid {
				again, err := NormalizeCategory(normalized)
				if err != nil || again != normalized {
					t.Fatal("normalization is not idempotent")
				}
			} else if normalized != "" {
				t.Fatal("invalid category returned a storage key")
			}
			_, err = NewTransaction(1, TransactionInput{Amount: 1, Category: tc.category}, MinDate)
			if (err == nil) != tc.valid {
				t.Fatalf("transaction validation: %v", err)
			}
			if !tc.valid {
				assertValidationFields(t, err, "category")
			}
			_, err = NewBudget(1, BudgetInput{Limit: 1, Category: tc.category})
			if (err == nil) != tc.valid {
				t.Fatalf("budget validation: %v", err)
			}
			if !tc.valid {
				assertValidationFields(t, err, "category")
			}
		})
	}
}
