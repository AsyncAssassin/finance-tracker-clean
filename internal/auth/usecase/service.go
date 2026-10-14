// Package usecase implements registration and login on top of the domain
// rules, a Repository of accounts, a password Hasher and a TokenIssuer.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
)

// Repository stores accounts. Implementations live under internal/auth/repo.
type Repository interface {
	// CreateUser stores a new account and returns it with its ID and
	// creation time, or domain.ErrEmailTaken when the email is used.
	CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error)
	// UserByEmail returns the account with the normalized email, or
	// domain.ErrNotFound.
	UserByEmail(ctx context.Context, email string) (domain.User, error)
}

// Hasher turns passwords into hashes that are safe to store.
type Hasher interface {
	Hash(ctx context.Context, password string) (string, error)
	Verify(ctx context.Context, password, encoded string) (bool, error)
}

// TokenIssuer creates access tokens.
type TokenIssuer interface {
	Issue(userID int64) (token string, expiresAt time.Time, err error)
}

// Session is the result of a registration or a login.
type Session struct {
	User      domain.User
	Token     string
	ExpiresAt time.Time
}

// Service runs the auth operations.
type Service struct {
	repo               Repository
	hasher             Hasher
	tokens             TokenIssuer
	throttle           authThrottle
	registrationEmails map[string]bool

	// dummyHash is checked for logins with an unknown email, so they take
	// as long as a wrong password. It is made in New, so the first such
	// login does not pay for a hash on top of the check.
	dummyHash string
}

// New returns a Service.
func New(repo Repository, hasher Hasher, tokens TokenIssuer, options ...Option) *Service {
	s := &Service{repo: repo, hasher: hasher, tokens: tokens, throttle: authThrottle{now: time.Now}}
	for _, option := range options {
		option(s)
	}
	s.dummyHash, _ = hasher.Hash(context.Background(), "a password nobody has")
	return s
}

type Option func(*Service)

// WithRegistrationEmails restricts registration, including direct gRPC calls.
// An empty list closes registration. The executable always supplies this option;
// callers embedding Service without it must provide their own admission policy.
func WithRegistrationEmails(emails []string) Option {
	allowed := make(map[string]bool, len(emails))
	for _, email := range emails {
		allowed[strings.Clone(domain.NormalizeEmail(email))] = true
	}
	return func(s *Service) { s.registrationEmails = allowed }
}

// Register creates an account and returns a session for it. A bad email
// or password gives a *domain.ValidationError, an email that is taken
// domain.ErrEmailTaken.
func (s *Service) Register(ctx context.Context, email, password string) (Session, error) {
	// A caller that is gone gets nothing done for it, not even the
	// normalization; a canceled attempt spends no allowance either.
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if err := s.throttle.enter(true); err != nil {
		return Session{}, err
	}
	defer s.throttle.leave(true)
	email = domain.NormalizeEmail(email)
	password = domain.NormalizePassword(password)
	if err := domain.ValidateNewAccount(email, password); err != nil {
		return Session{}, err
	}
	if s.registrationEmails != nil && !s.registrationEmails[email] {
		return Session{}, domain.ErrRegistrationClosed
	}
	if err := s.throttle.register(); err != nil {
		return Session{}, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return Session{}, fmt.Errorf("hash password: %w", err)
	}
	user, err := s.repo.CreateUser(ctx, email, hash)
	if err != nil {
		if errors.Is(err, domain.ErrEmailTaken) {
			return Session{}, err
		}
		return Session{}, fmt.Errorf("create user: %w", err)
	}
	return s.session(user)
}

// Login checks the credentials and returns a new session. An unknown email
// and a wrong password both give domain.ErrInvalidCredentials.
func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if err := s.throttle.enter(false); err != nil {
		return Session{}, err
	}
	defer s.throttle.leave(false)
	email = domain.NormalizeEmail(email)
	password = domain.NormalizePassword(password)
	if err := domain.ValidateLogin(email, password); err != nil {
		return Session{}, err
	}
	if err := s.throttle.claim(email); err != nil {
		return Session{}, err
	}
	defer s.throttle.release(email)
	user, err := s.repo.UserByEmail(ctx, email)
	known := !errors.Is(err, domain.ErrNotFound)
	if err != nil && known {
		return Session{}, fmt.Errorf("find user: %w", err)
	}
	if err := s.throttle.login(email, known); err != nil {
		return Session{}, err
	}
	failed, success := false, false
	defer func() { s.throttle.finish(email, known, failed, success) }()
	if !known {
		// Spend the time of a real check, so the response time does not
		// tell whether the email has an account; and fail the same way as
		// a real check when the hasher is too busy to answer in time.
		if err := s.verifyDummy(ctx, password); err != nil {
			return Session{}, fmt.Errorf("verify password: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return Session{}, err
		}
		failed = true
		return Session{}, domain.ErrInvalidCredentials
	}
	ok, err := s.hasher.Verify(ctx, password, user.PasswordHash)
	if err != nil {
		return Session{}, fmt.Errorf("verify password: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if !ok {
		failed = true
		return Session{}, domain.ErrInvalidCredentials
	}
	success = true
	return s.session(user)
}

func (s *Service) session(user domain.User) (Session, error) {
	token, expires, err := s.tokens.Issue(user.ID)
	if err != nil {
		return Session{}, fmt.Errorf("issue token: %w", err)
	}
	return Session{User: user, Token: token, ExpiresAt: expires}, nil
}

// verifyDummy checks password against dummyHash. Only an error matters:
// the result is always a mismatch.
func (s *Service) verifyDummy(ctx context.Context, password string) error {
	if s.dummyHash == "" {
		return nil
	}
	_, err := s.hasher.Verify(ctx, password, s.dummyHash)
	return err
}
