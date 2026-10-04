// Package memory is an in-memory Repository of accounts: the store of the
// auth service without a database and the fake behind its tests.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
)

// Repo keeps accounts in a map; it is safe for concurrent use.
type Repo struct {
	mu      sync.Mutex
	byEmail map[string]domain.User
	ids     map[int64]struct{}
}

// New returns an empty Repo. Random IDs avoid predictable identity reuse
// across restarts and wall-clock changes: an old token must not identify a
// new account. IDs use 53 bits so JSON clients including Apps Script can
// represent them exactly. Collisions within this store are retried.
func New() *Repo {
	return &Repo{byEmail: map[string]domain.User{}, ids: map[int64]struct{}{}}
}

var _ usecase.Repository = (*Repo)(nil)

func (r *Repo) CreateUser(ctx context.Context, email, passwordHash string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byEmail[email]; taken {
		return domain.User{}, domain.ErrEmailTaken
	}
	if len(r.byEmail) >= domain.MaxAccounts {
		return domain.User{}, domain.ErrAccountQuota
	}
	id, err := r.newID()
	if err != nil {
		return domain.User{}, err
	}
	u := domain.User{ID: id, Email: email, PasswordHash: passwordHash, CreatedAt: time.Now().UTC()}
	r.byEmail[email] = u
	return u, nil
}

// newID runs under mu. The random source is cryptographic and independent
// of the clock. Without persistent state, cross-restart uniqueness is
// probabilistic; production accounts belong in PostgreSQL.
func (r *Repo) newID() (int64, error) {
	for {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, fmt.Errorf("generate account ID: %w", err)
		}
		id := int64(binary.BigEndian.Uint64(buf[:]) & (1<<53 - 1))
		if _, exists := r.ids[id]; id != 0 && !exists {
			r.ids[id] = struct{}{}
			return id, nil
		}
	}
}

func (r *Repo) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byEmail[email]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}
