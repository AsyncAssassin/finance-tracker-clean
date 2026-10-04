// Package repotest is the contract every account Repository must meet. The
// in-memory and the PostgreSQL repositories run the same scenarios.
package repotest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
)

// Run runs the contract against fresh repositories made by newRepo.
func Run(t *testing.T, newRepo func(t *testing.T) usecase.Repository) {
	t.Run("account quota is atomic and preserves duplicates", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()
		for i := range domain.MaxAccounts - 1 {
			if _, err := r.CreateUser(ctx, fmt.Sprintf("quota-%d@example.com", i), "hash"); err != nil {
				t.Fatal(err)
			}
		}
		results := make(chan error, 8)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Go(func() { _, err := r.CreateUser(ctx, fmt.Sprintf("last-%d@example.com", i), "hash"); results <- err })
		}
		wg.Wait()
		close(results)
		created := 0
		for err := range results {
			if err == nil {
				created++
			} else if !errors.Is(err, domain.ErrAccountQuota) {
				t.Fatal(err)
			}
		}
		if created != 1 {
			t.Fatalf("created %d accounts at quota boundary", created)
		}
		if _, err := r.CreateUser(ctx, "quota-0@example.com", "changed"); !errors.Is(err, domain.ErrEmailTaken) {
			t.Fatalf("duplicate at quota: %v", err)
		}
	})

	t.Run("create and find", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()
		u, err := r.CreateUser(ctx, "anna@example.com", "$argon2id$hash")
		if err != nil {
			t.Fatal(err)
		}
		if u.ID <= 0 || u.CreatedAt.IsZero() || u.Email != "anna@example.com" || u.PasswordHash != "$argon2id$hash" {
			t.Errorf("CreateUser = %+v", u)
		}
		got, err := r.UserByEmail(ctx, "anna@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != u.ID || got.Email != u.Email || got.PasswordHash != u.PasswordHash || !got.CreatedAt.Equal(u.CreatedAt) {
			t.Errorf("UserByEmail = %+v, want %+v", got, u)
		}
		other, err := r.CreateUser(ctx, "boris@example.com", "$argon2id$other")
		if err != nil || other.ID == u.ID {
			t.Errorf("second user = %+v, %v; want another ID", other, err)
		}
	})

	t.Run("unknown email", func(t *testing.T) {
		r := newRepo(t)
		if _, err := r.UserByEmail(context.Background(), "nobody@example.com"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("email taken", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()
		if _, err := r.CreateUser(ctx, "anna@example.com", "h1"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.CreateUser(ctx, "anna@example.com", "h2"); !errors.Is(err, domain.ErrEmailTaken) {
			t.Errorf("err = %v, want ErrEmailTaken", err)
		}
		if u, _ := r.UserByEmail(ctx, "anna@example.com"); u.PasswordHash != "h1" {
			t.Error("the second registration replaced the first account")
		}
	})

	t.Run("concurrent registrations of one email", func(t *testing.T) {
		r := newRepo(t)
		const n = 20
		var wg sync.WaitGroup
		results := make(chan error, n)
		for range n {
			wg.Go(func() {
				_, err := r.CreateUser(context.Background(), "race@example.com", "h")
				results <- err
			})
		}
		wg.Wait()
		close(results)
		created := 0
		for err := range results {
			switch {
			case err == nil:
				created++
			case !errors.Is(err, domain.ErrEmailTaken):
				t.Errorf("unexpected error: %v", err)
			}
		}
		if created != 1 {
			t.Errorf("%d accounts created, want exactly 1", created)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		r := newRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := r.CreateUser(ctx, "anna@example.com", "h"); err == nil {
			t.Error("CreateUser with a canceled context succeeded")
		}
	})
}
