package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/validation"
)

const goodPassword = "зелёный трамвай едет"

// fieldProblem returns the problem reported for field, or "".
func fieldProblem(t *testing.T, err error, field string) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var v *ValidationError
	if !errors.As(err, &v) || !errors.Is(err, validation.ErrValidation) {
		t.Fatalf("error = %v, want a *ValidationError", err)
	}
	return v.Fields[field]
}

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()
	if got := NormalizeEmail("  Anna.Petrova@Example.COM \n"); got != "anna.petrova@example.com" {
		t.Errorf("NormalizeEmail = %q", got)
	}
}

func TestEmailRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		email string
		ok    bool
	}{
		{"anna@example.com", true},
		{"a.b+tag@sub.example.co", true},
		{"", false},
		{"anna", false},
		{"anna@", false},
		{"@example.com", false},
		{"anna@example", false},
		{"anna@.example.com", false},
		{"anna@example.com.", false},
		{"anna@@example.com", false},
		{"anna <anna@example.com>", false},
		{"anna petrova@example.com", false},
		{strings.Repeat("a", 250) + "@x.ru", false},
	}
	for _, tc := range tests {
		err := ValidateNewAccount(tc.email, goodPassword)
		if problem := fieldProblem(t, err, "email"); (problem == "") != tc.ok {
			t.Errorf("email %q: problem %q, want ok=%v", tc.email, problem, tc.ok)
		}
	}
}

func TestPasswordRules(t *testing.T) {
	t.Parallel()
	const email = "anna.petrova.1990@example.com"
	tests := []struct {
		name     string
		password string
		problem  string // a substring of the expected problem, "" for valid
	}{
		{"phrase in Russian", goodPassword, ""},
		{"exactly 15 characters", strings.Repeat("ab", 7) + "c", ""},
		{"15 Cyrillic letters are 30 bytes but 15 characters", "абвгдеёжзийклмн", ""},
		{"128 characters", strings.Repeat("xy", 64), ""},
		{"no composition rules: lowercase letters only", "correct horse battery staple", ""},
		{"empty", "", "required"},
		{"14 characters", strings.Repeat("ab", 7), "at least 15"},
		{"129 characters", strings.Repeat("xy", 64) + "z", "at most 128"},
		{"invalid UTF-8", "valid prefix then \xff\xfe bytes", "UTF-8"},
		{"blocklisted", "qwertyuiopasdfgh", "too common"},
		{"blocklisted in another case", "QwertyuiopASDFGH", "too common"},
		{"blocklisted Cyrillic keyboard walk", "йцукенгшщзхъфыва", "too common"},
		{"service name", "finance-tracker", "too common"},
		{"the email itself", email, "too common"},
		{"the name part of the email", "anna.petrova.1990", "too common"},
		{"one character repeated", strings.Repeat("a", 20), "too common"},
		{"one Cyrillic character repeated", strings.Repeat("я", 16), "too common"},
	}
	for _, tc := range tests {
		problem := fieldProblem(t, ValidateNewAccount(email, tc.password), "password")
		if tc.problem == "" && problem != "" || !strings.Contains(problem, tc.problem) {
			t.Errorf("%s: problem %q, want %q", tc.name, problem, tc.problem)
		}
	}
}

func TestNormalizePassword(t *testing.T) {
	t.Parallel()
	composed := "пароль на йогурт и кофе"
	decomposed := strings.ReplaceAll(composed, "й", "й")
	if decomposed == composed {
		t.Fatal("the test input did not change")
	}
	if NormalizePassword(decomposed) != NormalizePassword(composed) {
		t.Error("composed and decomposed й normalize differently")
	}
	if got := NormalizePassword("１２３ＡＢＣ"); got != "123ABC" {
		t.Errorf("fullwidth characters normalize to %q, want 123ABC", got)
	}
}

func TestValidateLogin(t *testing.T) {
	t.Parallel()
	if err := ValidateLogin("anna@example.com", "short"); err != nil {
		t.Errorf("login with a short password is a credentials question, not a validation error: %v", err)
	}
	err := ValidateLogin("", "")
	if fieldProblem(t, err, "email") == "" || fieldProblem(t, err, "password") == "" {
		t.Errorf("empty login = %v, want both fields required", err)
	}
	if fieldProblem(t, ValidateLogin("anna@example.com", strings.Repeat("x", 129)), "password") == "" {
		t.Error("a password longer than any accepted one passes")
	}
}

func TestBlocklistIsUsable(t *testing.T) {
	t.Parallel()
	if len(blocklist) < 20 {
		t.Fatalf("blocklist has %d entries", len(blocklist))
	}
	for p := range blocklist {
		if p != strings.ToLower(p) || strings.HasPrefix(p, "#") {
			t.Errorf("blocklist entry %q is not a lowercased password", p)
		}
		if passwordLen(p) < MinPasswordLen {
			t.Errorf("blocklist entry %q is shorter than the minimum and never matches", p)
		}
	}
}

func TestLoginRejectsInvalidEmailButPreservesNULPassword(t *testing.T) {
	for _, email := range []string{"anna\x00@example.com", "anna\xff@example.com"} {
		if err := ValidateLogin(email, "long valid passphrase"); err == nil {
			t.Fatal("invalid email accepted")
		}
	}
	if err := ValidateLogin("anna@example.com", "long\x00valid passphrase"); err != nil {
		t.Fatalf("existing password rejected: %v", err)
	}
}
