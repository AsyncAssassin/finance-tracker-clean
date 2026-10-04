package usecase

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
)

func TestRegistrationAllowlist(t *testing.T) {
	t.Parallel()
	s, h, _ := throttledService(t)
	WithRegistrationEmails(nil)(s)
	if _, err := s.Register(context.Background(), "anna@example.com", "valid test passphrase"); !errors.Is(err, domain.ErrRegistrationClosed) {
		t.Fatal(err)
	}
	if h.hashes.Load() != 1 {
		t.Fatal("closed registration reached password work")
	}
	WithRegistrationEmails([]string{" Anna@Example.com "})(s)
	if _, err := s.Register(context.Background(), "ANNA@example.com", "valid test passphrase"); err != nil {
		t.Fatal(err)
	}
	WithRegistrationEmails(nil)(s)
	if _, err := s.Login(context.Background(), "anna@example.com", "valid test passphrase"); err != nil {
		t.Fatalf("closing registration revoked existing login: %v", err)
	}
}

type waitingHasher struct{ started chan struct{} }

func (h waitingHasher) Hash(context.Context, string) (string, error) { return "hash", nil }
func (h waitingHasher) Verify(ctx context.Context, _, _ string) (bool, error) {
	h.started <- struct{}{}
	<-ctx.Done()
	return false, ctx.Err()
}

func TestAuthAdmissionAndCancellation(t *testing.T) {
	t.Parallel()
	s, _, _ := throttledService(t)
	h := waitingHasher{make(chan struct{}, 3)}
	s.hasher = h
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 3)
	for i := range 3 {
		go func() {
			_, err := s.Login(ctx, fmt.Sprintf("waiting-%d@example.com", i), "wrong passphrase")
			done <- err
		}()
		select {
		case <-h.started:
		case <-time.After(time.Second):
			t.Fatal("login did not reach hasher")
		}
		if i == 0 {
			_, err := s.Login(ctx, "waiting-0@example.com", "wrong passphrase")
			requireThrottle(t, err, time.Second)
		}
	}
	if _, err := s.Login(ctx, "fourth@example.com", "wrong passphrase"); !errors.Is(err, domain.ErrBusy) {
		t.Fatalf("capacity: %v", err)
	}
	cancel()
	for range 3 {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if s.throttle.active != 0 || len(s.throttle.inFlight) != 0 || len(s.throttle.unknown) != 0 {
		t.Fatal("canceled attempts retained work or failure history")
	}
}

func TestThrottleKeysDoNotRetainPaddedInput(t *testing.T) {
	s, _, _ := throttledService(t)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range 64 {
		padded := strings.Repeat(" ", 1<<20) + fmt.Sprintf("memory-%d@example.com", i)
		_, err := s.Login(context.Background(), padded, "wrong passphrase")
		if !errors.Is(err, domain.ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)
	if after.HeapAlloc > before.HeapAlloc+8<<20 {
		t.Fatalf("retained padded input: %d bytes", after.HeapAlloc-before.HeapAlloc)
	}
}
