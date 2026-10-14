package usecase

import (
	"strings"
	"sync"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
)

const (
	authWindow         = time.Minute
	loginsPerEmail     = 5
	registersPerWindow = 10
	maxActiveAuth      = 3
	maxLoginHistory    = 4096
)

type loginHistory struct {
	failures int
	until    time.Time
}

// Unknown addresses must never evict the protection of an existing account.
// The account repository also bounds registration to MaxAccounts.
type authThrottle struct {
	mu            sync.Mutex
	now           func() time.Time
	active        int
	registering   bool
	registerUntil time.Time
	registers     int
	inFlight      map[string]bool
	known         map[string]*loginHistory
	unknown       map[string]*loginHistory
}

func (a *authThrottle) enter(register bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active >= maxActiveAuth || register && a.registering {
		return domain.ErrBusy
	}
	a.active++
	if register {
		a.registering = true
	}
	return nil
}

func (a *authThrottle) leave(register bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active--
	if register {
		a.registering = false
	}
}

func (a *authThrottle) register() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if !now.Before(a.registerUntil) {
		a.registerUntil = now.Add(authWindow)
		a.registers = 0
	}
	if a.registers >= registersPerWindow {
		return &domain.ThrottledError{RetryAfter: a.registerUntil.Sub(now)}
	}
	a.registers++
	return nil
}

// claim precedes storage and hashing. At most maxActiveAuth copied keys are
// retained, including direct gRPC calls.
func (a *authThrottle) claim(email string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight[email] {
		return &domain.ThrottledError{RetryAfter: time.Second}
	}
	if a.inFlight == nil {
		a.inFlight = make(map[string]bool)
	}
	a.inFlight[strings.Clone(email)] = true
	return nil
}
func (a *authThrottle) release(email string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.inFlight, email)
}

func (a *authThrottle) login(email string, known bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	history := a.unknown
	if known {
		history = a.known
	}
	if history == nil {
		history = make(map[string]*loginHistory)
		if known {
			a.known = history
		} else {
			a.unknown = history
		}
	}
	now := a.now()
	if h := history[email]; h != nil {
		if !now.Before(h.until) {
			h.failures = 0
			h.until = now.Add(authWindow)
		}
		if h.failures >= loginsPerEmail {
			return &domain.ThrottledError{RetryAfter: h.until.Sub(now)}
		}
		return nil
	}
	if len(history) >= maxLoginHistory {
		for key, h := range history {
			if !a.inFlight[key] && !now.Before(h.until) {
				delete(history, key)
			}
		}
	}
	if len(history) >= maxLoginHistory && !known {
		// These addresses have no password. Eviction cannot reset the counter
		// of a real account; known histories are never evicted.
		for key := range history {
			if !a.inFlight[key] {
				delete(history, key)
				break
			}
		}
	}
	if len(history) >= maxLoginHistory {
		return domain.ErrBusy
	}
	history[strings.Clone(email)] = &loginHistory{until: now.Add(authWindow)}
	return nil
}

// Count credential failures, not successes, canceled work or backend errors.
// The in-flight claim is held through this update.
func (a *authThrottle) finish(email string, known, failed, success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	history := a.unknown
	if known {
		history = a.known
	}
	h := history[email]
	if h == nil {
		return
	}
	if failed {
		h.failures++
	}
	if success || h.failures == 0 {
		delete(history, email)
	}
}
