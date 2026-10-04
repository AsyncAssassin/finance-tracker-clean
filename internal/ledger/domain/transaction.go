package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/civil"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/money"
)

// Limits on text fields, in characters.
const (
	MaxCategoryLen    = 64
	MaxDescriptionLen = 255
	MaxExternalIDLen  = 64
)

// Accepted range for amounts and budget limits.
const (
	MinAmount money.Money = 1 // one kopeck
	MaxAmount             = money.Max
)

// Transaction is a single expense of a user.
type Transaction struct {
	ID          int64
	UserID      int64
	Amount      money.Money
	Category    string // normalized, see NormalizeCategory
	Description string
	Date        civil.Date // the day of the expense
	ExternalID  string     // optional client-side ID that makes repeated submissions idempotent
	CreatedAt   time.Time
}

// TransactionInput is what a client sends to record a transaction.
type TransactionInput struct {
	Amount      money.Money
	Category    string
	Description string
	Date        civil.Date // zero means today
	ExternalID  string
}

// NewTransaction validates and normalizes in for the user and returns the
// transaction to store. today replaces a zero date. Invalid input yields
// a *ValidationError listing every problem.
func NewTransaction(userID int64, in TransactionInput, today civil.Date) (Transaction, error) {
	v := new(ValidationError)
	category, err := NormalizeCategory(in.Category)
	if err != nil {
		v.Add("category", err.Error())
	}
	ValidateText(v, "description", in.Description)
	ValidateText(v, "external_id", in.ExternalID)
	tx := Transaction{
		UserID:      userID,
		Amount:      in.Amount,
		Category:    category,
		Description: strings.TrimSpace(in.Description),
		Date:        in.Date,
		ExternalID:  strings.TrimSpace(in.ExternalID),
	}
	validateAmount(v, "amount", tx.Amount)
	if utf8.RuneCountInString(tx.Description) > MaxDescriptionLen {
		v.Add("description", fmt.Sprintf("must be at most %d characters", MaxDescriptionLen))
	}
	if utf8.RuneCountInString(tx.ExternalID) > MaxExternalIDLen {
		v.Add("external_id", fmt.Sprintf("must be at most %d characters", MaxExternalIDLen))
	}
	if tx.Date.IsZero() {
		tx.Date = today
	}
	if tx.Date.Before(MinDate) || tx.Date.After(MaxDate) {
		v.Add("date", "must be between "+MinDate.String()+" and "+MaxDate.String())
	}
	if err := v.ErrOrNil(); err != nil {
		return Transaction{}, err
	}
	return tx, nil
}

// NormalizeCategory validates a category and returns its common storage key.
// Unsupported combining sequences are rejected instead of inserting separators.
func NormalizeCategory(s string) (string, error) {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return "", errors.New("must be valid UTF-8 without NUL characters")
	}
	originalJoiners := strings.Count(s, norm.GraphemeJoiner)
	// Compose before changing case: U+0130 and I + U+0307 must share a key.
	s = norm.NFC.String(strings.Join(strings.Fields(s), " "))
	s = norm.NFC.String(strings.ToLower(s))
	// NFC never removes an existing CGJ. A new one means the library exceeded
	// its stream-safe bound, including marks inside decomposed characters.
	if strings.Count(s, norm.GraphemeJoiner) != originalJoiners {
		return "", errors.New("combining character sequence is too long")
	}
	switch {
	case s == "":
		return "", errors.New("must not be empty")
	case utf8.RuneCountInString(s) > MaxCategoryLen:
		return "", fmt.Errorf("must be at most %d characters", MaxCategoryLen)
	}
	return s, nil
}

func validateAmount(v *ValidationError, field string, amount money.Money) {
	switch {
	case amount < MinAmount:
		v.Add(field, "must be greater than 0")
	case amount > MaxAmount:
		v.Add(field, "must be at most "+MaxAmount.String())
	}
}

// ValidateText rejects text PostgreSQL cannot store without changing it.
func ValidateText(v *ValidationError, field, value string) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		v.Add(field, "must be valid UTF-8 without NUL characters")
	}
}
