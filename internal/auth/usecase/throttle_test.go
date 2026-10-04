package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
)

// These counters check that rejected attempts never reach password work.
type observedHasher struct {
	hashes, checks atomic.Int32
}

// Local repository double avoids importing an adapter back into usecase.
// Tests populate accounts before their concurrent read-only login calls.
type throttleRepo struct{ users map[string]domain.User }

func (r *throttleRepo) CreateUser(_ context.Context, email, hash string) (domain.User, error) {
	if _, exists := r.users[email]; exists {
		return domain.User{}, domain.ErrEmailTaken
	}
	u := domain.User{ID: int64(len(r.users) + 1), Email: email, PasswordHash: hash}
	r.users[email] = u
	return u, nil
}

func (r *throttleRepo) UserByEmail(_ context.Context, email string) (domain.User, error) {
	if u, exists := r.users[email]; exists {
		return u, nil
	}
	return domain.User{}, domain.ErrNotFound
}

func (h *observedHasher) Hash(context.Context, string) (string, error) {
	h.hashes.Add(1)
	return "stored-hash", nil
}

func (h *observedHasher) Verify(_ context.Context, password, _ string) (bool, error) {
	h.checks.Add(1)
	return password == "valid test passphrase", nil
}

func throttledService(t *testing.T) (*Service, *observedHasher, *time.Time) {
	t.Helper()
	issuer, err := jwtauth.NewIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &observedHasher{}
	s := New(&throttleRepo{users: make(map[string]domain.User)}, h, issuer)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.throttle.now = func() time.Time { return now }
	return s, h, &now
}

func requireThrottle(t *testing.T, err error, delay time.Duration) {
	t.Helper()
	var limited *domain.ThrottledError
	if !errors.As(err, &limited) || limited.RetryAfter != delay {
		t.Fatalf("error = %v, want a throttle with delay %s", err, delay)
	}
}

func TestLoginThrottleNormalizationAndExpiry(t *testing.T) {
	t.Parallel()
	s, h, now := throttledService(t)
	ctx := context.Background()
	if _, err := s.Register(ctx, "anna@example.com", "valid test passphrase"); err != nil {
		t.Fatal(err)
	}
	// Existing and nonexistent accounts get the same allowance and delay.
	for _, email := range []string{"anna@example.com", "unknown@example.com"} {
		for i := range 5 {
			variant := email
			if i%2 == 0 {
				variant = "  " + strings.ToUpper(email) + "  "
			}
			if _, err := s.Login(ctx, variant, "wrong passphrase"); !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Fatalf("attempt %d: %v", i+1, err)
			}
		}
		_, err := s.Login(ctx, email, "valid test passphrase")
		requireThrottle(t, err, time.Minute)
	}
	if h.checks.Load() != 10 {
		t.Fatalf("password checks = %d, want 10", h.checks.Load())
	}
	// A rejected retry does not prolong the block or block another account.
	*now = now.Add(59 * time.Second)
	_, err := s.Login(ctx, "anna@example.com", "wrong passphrase")
	requireThrottle(t, err, time.Second)
	if _, err := s.Login(ctx, "other@example.com", "wrong passphrase"); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("other email: %v", err)
	}
	*now = now.Add(time.Second)
	if _, err := s.Login(ctx, "anna@example.com", "valid test passphrase"); err != nil {
		t.Fatalf("valid login at the window boundary: %v", err)
	}
}

func TestLoginThrottleBoundsRotatingEmails(t *testing.T) {
	t.Parallel()
	s, _, _ := throttledService(t)
	ctx := context.Background()
	for _, email := range []string{"protected@example.com", "healthy@example.com"} {
		if _, err := s.Register(ctx, email, "valid test passphrase"); err != nil {
			t.Fatal(err)
		}
	}
	for range 5 {
		_, _ = s.Login(ctx, "protected@example.com", "wrong passphrase")
	}
	for i := range maxLoginHistory + 64 {
		_, err := s.Login(ctx, fmt.Sprintf("unknown-%d@example.com", i), "wrong passphrase")
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if len(s.throttle.unknown) != maxLoginHistory {
		t.Fatalf("unbounded history: %d", len(s.throttle.unknown))
	}
	_, err := s.Login(ctx, "protected@example.com", "valid test passphrase")
	requireThrottle(t, err, time.Minute)
	if _, err := s.Login(ctx, "healthy@example.com", "valid test passphrase"); err != nil {
		t.Fatalf("unrelated account: %v", err)
	}
}

func TestLoginThrottleIsAtomic(t *testing.T) {
	t.Parallel()
	s, h, _ := throttledService(t)
	var checked, limited atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			_, err := s.Login(context.Background(), "same@example.com", "wrong passphrase")
			var throttle *domain.ThrottledError
			switch {
			case errors.Is(err, domain.ErrInvalidCredentials):
				checked.Add(1)
			case errors.As(err, &throttle):
				limited.Add(1)
			case errors.Is(err, domain.ErrBusy):
				limited.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if checked.Load() > 5 || checked.Load() < 1 || checked.Load()+limited.Load() != 40 || h.checks.Load() != checked.Load() {
		t.Fatalf("checked=%d limited=%d hashes=%d", checked.Load(), limited.Load(), h.checks.Load())
	}
}

func TestRegistrationThrottleAndLoginAreIndependent(t *testing.T) {
	t.Parallel()
	s, h, now := throttledService(t)
	ctx := context.Background()
	for i := range 10 {
		if _, err := s.Register(ctx, fmt.Sprintf("user-%d@example.com", i), "valid test passphrase"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Register(ctx, "next@example.com", "valid test passphrase")
	requireThrottle(t, err, time.Minute)
	if h.hashes.Load() != 11 { // Ten registrations and the dummy hash at startup.
		t.Fatalf("hash calls = %d, want 11", h.hashes.Load())
	}
	for range 5 {
		if _, err := s.Login(ctx, "user-0@example.com", "valid test passphrase"); err != nil {
			t.Fatalf("login while registration is limited: %v", err)
		}
	}
	_, err = s.Login(ctx, "user-0@example.com", "valid test passphrase")
	if err != nil {
		t.Fatalf("successful logins must not consume failures: %v", err)
	}
	*now = now.Add(time.Minute)
	if _, err := s.Register(ctx, "next@example.com", "valid test passphrase"); err != nil {
		t.Fatalf("registration after expiry: %v", err)
	}
}

func TestInvalidAndCanceledAuthDoesNotSpendAllowance(t *testing.T) {
	t.Parallel()
	s, h, _ := throttledService(t)
	ctx := context.Background()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for range 20 {
		_, err := s.Login(ctx, strings.Repeat("e", 255), "wrong passphrase")
		var validation *domain.ValidationError
		if !errors.As(err, &validation) || validation.Fields["email"] == "" {
			t.Fatalf("long email: %v", err)
		}
		if _, err := s.Login(canceled, "anna@example.com", "wrong passphrase"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled login: %v", err)
		}
		if _, err := s.Register(canceled, "anna@example.com", "valid test passphrase"); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled registration: %v", err)
		}
	}
	if h.checks.Load() != 0 || h.hashes.Load() != 1 {
		t.Fatal("invalid or canceled attempts reached the hasher")
	}
	if _, err := s.Register(ctx, "anna@example.com", "valid test passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "anna@example.com", "valid test passphrase"); err != nil {
		t.Fatal(err)
	}
}
