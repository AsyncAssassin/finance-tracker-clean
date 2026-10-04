// Package domain holds the accounts of the auth service and the rules for
// their emails and passwords. It knows nothing about storage, hashing
// algorithms or tokens.
package domain

import (
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/validation"
)

// MaxEmailLen is the longest email address, from RFC 5321.
const MaxEmailLen = 254

// MaxAccounts bounds a single pilot installation, including old allowlist entries.
const MaxAccounts = 100

// Errors of the auth service.
var (
	// ErrEmailTaken means an account with the email already exists.
	ErrEmailTaken = errors.New("email is already registered")
	// ErrInvalidCredentials means the email and password match no account.
	// An unknown email and a wrong password give the same error, so the
	// answer does not reveal which accounts exist.
	ErrInvalidCredentials = errors.New("invalid email or password")
	// ErrNotFound means a repository has no such account.
	ErrNotFound = errors.New("not found")
	// ErrBusy is a temporary resource refusal, separate from an account limit.
	ErrBusy               = errors.New("authentication is busy, retry later")
	ErrRegistrationClosed = errors.New("registration is not available for this address")
	ErrAccountQuota       = errors.New("the account quota for this installation is reached")
)

// ValidationError lists the invalid fields of a request; it is the error
// type shared by all services.
type ValidationError = validation.Error

// ThrottledError tells a caller when the next authentication attempt is
// allowed. It never discloses whether an email exists.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (*ThrottledError) Error() string { return "too many authentication attempts, try again later" }

// User is an account.
type User struct {
	ID           int64
	Email        string // normalized, see NormalizeEmail
	PasswordHash string // in the format of the hasher, never the password
	CreatedAt    time.Time
}

// NormalizeEmail trims spaces and lowercases the address, so one person
// cannot register "Anna@Mail.ru" and "anna@mail.ru" as two accounts.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validateEmail checks a normalized address: a plain addr-spec such as
// name@example.com, without a display name, with a dot in the domain to
// catch typos like name@gmail.
func validateEmail(v *validation.Error, email string) {
	switch {
	case email == "":
		v.Add("email", "is required")
	case len(email) > MaxEmailLen:
		v.Add("email", "must be at most 254 characters")
	default:
		addr, err := mail.ParseAddress(email)
		at := strings.LastIndexByte(email, '@')
		if err != nil || addr.Address != email || at < 0 || !validDomain(email[at+1:]) {
			v.Add("email", "must be an email address such as name@example.com")
		}
	}
}

func validDomain(d string) bool {
	return strings.Contains(d, ".") && !strings.HasPrefix(d, ".") && !strings.HasSuffix(d, ".")
}

// ValidateEmail validates a normalized address without applying a password policy.
func ValidateEmail(email string) error {
	v := new(validation.Error)
	validateEmail(v, email)
	return v.ErrOrNil()
}

// ValidateNewAccount checks the email and the password of a registration.
// Both must be normalized with NormalizeEmail and NormalizePassword.
func ValidateNewAccount(email, password string) error {
	v := new(validation.Error)
	validateEmail(v, email)
	validatePassword(v, password, email)
	return v.ErrOrNil()
}

// ValidateLogin checks only what a login needs: both fields present and
// the password not longer than any accepted one. The password policy may
// change later, so it is not applied to existing accounts.
func ValidateLogin(email, password string) error {
	v := new(validation.Error)
	if email == "" {
		v.Add("email", "is required")
	} else if len(email) > MaxEmailLen {
		v.Add("email", "must be at most 254 characters")
	} else if !utf8.ValidString(email) || strings.ContainsRune(email, 0) {
		v.Add("email", "must be valid UTF-8 without NUL characters")
	}
	if password == "" {
		v.Add("password", "is required")
	} else if passwordLen(password) > MaxPasswordLen {
		v.Add("password", "must be at most 128 characters")
	}
	return v.ErrOrNil()
}
