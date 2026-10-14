package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/hasher"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
)

const password = "зелёный трамвай едет"

var secret = []byte("0123456789abcdef0123456789abcdef")

// countingHasher counts the calls of a fast real hasher.
type countingHasher struct {
	*hasher.Argon2id
	hashes, verifies atomic.Int32
}

func (h *countingHasher) Hash(ctx context.Context, password string) (string, error) {
	h.hashes.Add(1)
	return h.Argon2id.Hash(ctx, password)
}

func (h *countingHasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	h.verifies.Add(1)
	return h.Argon2id.Verify(ctx, password, encoded)
}

type env struct {
	svc    *usecase.Service
	repo   *memory.Repo
	hasher *countingHasher
	tokens *jwtauth.Verifier
}

func newEnv(t *testing.T) env {
	t.Helper()
	issuer, err := jwtauth.NewIssuer(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := jwtauth.NewVerifier(secret)
	if err != nil {
		t.Fatal(err)
	}
	h := &countingHasher{Argon2id: hasher.New(hasher.Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}, 0)}
	repo := memory.New()
	return env{svc: usecase.New(repo, h, issuer), repo: repo, hasher: h, tokens: verifier}
}

func TestRegisterAndLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()

	reg, err := e.svc.Register(ctx, "  Anna@Example.COM ", password)
	if err != nil {
		t.Fatal(err)
	}
	if reg.User.ID <= 0 || reg.User.Email != "anna@example.com" || reg.ExpiresAt.IsZero() {
		t.Errorf("Register = %+v", reg)
	}
	if id, err := e.tokens.Verify(reg.Token); err != nil || id != reg.User.ID {
		t.Errorf("the registration token is for %d (%v), want %d", id, err, reg.User.ID)
	}
	stored, _ := e.repo.UserByEmail(ctx, "anna@example.com")
	if !strings.HasPrefix(stored.PasswordHash, "$argon2id$") || strings.Contains(stored.PasswordHash, password) {
		t.Errorf("stored hash = %q", stored.PasswordHash)
	}

	login, err := e.svc.Login(ctx, "ANNA@example.com", password)
	if err != nil {
		t.Fatal(err)
	}
	if login.User.ID != reg.User.ID {
		t.Errorf("Login user = %d, want %d", login.User.ID, reg.User.ID)
	}
	if id, err := e.tokens.Verify(login.Token); err != nil || id != reg.User.ID {
		t.Errorf("the login token is for %d (%v)", id, err)
	}
}

// The password is normalized the same way at registration and at login,
// so the form it is typed in does not matter, nor how many code points the
// typed form has.
func TestPasswordIsNormalized(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	composed := "мой йогурт и кофе утром"
	decomposed := strings.ReplaceAll(composed, "й", "и\u0306")
	tests := []struct {
		name, registered, typed string
	}{
		{"decomposed form of a composed registration", composed, decomposed},
		{"composed form of a decomposed registration", decomposed, composed},
		{"fullwidth letters and digits", "ＡＢＣ１２３ and the rest", "ABC123 and the rest"},
		{"ligatures", "ﬁnancial planning ﬂow", "financial planning flow"},
		{"Hangul jamo and syllables", "\u1112\u1161\u11ab\u1100\u1173\u11af password text", "한글 password text"},
		{"128 characters typed as 256 code points", strings.Repeat("и\u0306е\u0308", 64), strings.Repeat("йё", 64)},
	}
	for i, tc := range tests {
		email := fmt.Sprintf("user-%d@example.com", i)
		if _, err := e.svc.Register(ctx, email, tc.registered); err != nil {
			t.Errorf("%s: register: %v", tc.name, err)
			continue
		}
		if _, err := e.svc.Login(ctx, email, tc.typed); err != nil {
			t.Errorf("%s: login with the other form: %v", tc.name, err)
		}
	}
}

func TestRegisterErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Register(ctx, "anna@example.com", password); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Register(ctx, "Anna@Example.com", "another long passphrase"); !errors.Is(err, domain.ErrEmailTaken) {
		t.Errorf("same email in another case: %v, want ErrEmailTaken", err)
	}
	_, err := e.svc.Register(ctx, "not-an-email", "short")
	var v *domain.ValidationError
	if !errors.As(err, &v) || v.Fields["email"] == "" || v.Fields["password"] == "" {
		t.Errorf("bad email and password: %v", err)
	}
}

func TestLoginErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Register(ctx, "anna@example.com", password); err != nil {
		t.Fatal(err)
	}

	before := e.hasher.verifies.Load()
	_, wrong := e.svc.Login(ctx, "anna@example.com", "зелёный трамвай стоит")
	_, unknown := e.svc.Login(ctx, "boris@example.com", password)
	if !errors.Is(wrong, domain.ErrInvalidCredentials) || !errors.Is(unknown, domain.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v; unknown email: %v; want ErrInvalidCredentials for both", wrong, unknown)
	}
	if wrong.Error() != unknown.Error() {
		t.Error("the answers for a wrong password and an unknown email differ")
	}
	if got := e.hasher.verifies.Load() - before; got != 2 {
		t.Errorf("%d password checks for two failed logins, want 2: an unknown email must cost a check too", got)
	}

	_, err := e.svc.Login(ctx, "", "")
	var v *domain.ValidationError
	if !errors.As(err, &v) || v.Fields["email"] == "" || v.Fields["password"] == "" {
		t.Errorf("empty login: %v", err)
	}
}

// failingRepo fails every call with a storage error.
type failingRepo struct{}

var errDisk = errors.New("disk on fire")

func (failingRepo) CreateUser(context.Context, string, string) (domain.User, error) {
	return domain.User{}, errDisk
}

func (failingRepo) UserByEmail(context.Context, string) (domain.User, error) {
	return domain.User{}, errDisk
}

func TestStorageErrorsAreNotCredentialErrors(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	svc := usecase.New(failingRepo{}, e.hasher, nil)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "anna@example.com", password); !errors.Is(err, errDisk) || errors.Is(err, domain.ErrEmailTaken) {
		t.Errorf("Register = %v, want the storage error", err)
	}
	if _, err := svc.Login(ctx, "anna@example.com", password); !errors.Is(err, errDisk) || errors.Is(err, domain.ErrInvalidCredentials) {
		t.Errorf("Login = %v, want the storage error", err)
	}
}

func TestCorruptedHashIsAnError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.repo.CreateUser(ctx, "anna@example.com", "not a hash"); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Login(ctx, "anna@example.com", password)
	if err == nil || errors.Is(err, domain.ErrInvalidCredentials) || !errors.Is(err, hasher.ErrMalformed) {
		t.Errorf("Login with a corrupted hash = %v, want an internal error", err)
	}
}

// busyHasher never gets a free slot: Verify waits for the context to end.
type busyHasher struct{}

func (busyHasher) Hash(context.Context, string) (string, error) { return "$stub$", nil }

func (busyHasher) Verify(ctx context.Context, _, _ string) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func TestBusyHasherAnswersAlikeForKnownAndUnknownEmails(t *testing.T) {
	t.Parallel()
	repo := memory.New()
	if _, err := repo.CreateUser(context.Background(), "anna@example.com", "$stub$"); err != nil {
		t.Fatal(err)
	}
	svc := usecase.New(repo, busyHasher{}, nil)
	login := func(email string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := svc.Login(ctx, email, password)
		return err
	}
	known, unknown := login("anna@example.com"), login("boris@example.com")
	if !errors.Is(known, context.DeadlineExceeded) || !errors.Is(unknown, context.DeadlineExceeded) {
		t.Errorf("known email: %v; unknown email: %v; want the same timeout for both", known, unknown)
	}
}

// recordingRepo counts the calls of the repository.
type recordingRepo struct {
	usecase.Repository
	calls atomic.Int32
}

func (r *recordingRepo) CreateUser(ctx context.Context, email, hash string) (domain.User, error) {
	r.calls.Add(1)
	return r.Repository.CreateUser(ctx, email, hash)
}

func (r *recordingRepo) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	r.calls.Add(1)
	return r.Repository.UserByEmail(ctx, email)
}

// recordingIssuer counts the tokens issued.
type recordingIssuer struct {
	usecase.TokenIssuer
	calls atomic.Int32
}

func (i *recordingIssuer) Issue(userID int64) (string, time.Time, error) {
	i.calls.Add(1)
	return i.TokenIssuer.Issue(userID)
}

// recordingEnv is a service whose repository, hasher and token issuer count
// their calls.
type recordingEnv struct {
	svc    *usecase.Service
	repo   *recordingRepo
	hasher *countingHasher
	tokens *recordingIssuer
}

func newRecordingEnv(t *testing.T) recordingEnv {
	t.Helper()
	issuer, err := jwtauth.NewIssuer(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &countingHasher{Argon2id: hasher.New(hasher.Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}, 0)}
	repo := &recordingRepo{Repository: memory.New()}
	tokens := &recordingIssuer{TokenIssuer: issuer}
	return recordingEnv{svc: usecase.New(repo, h, tokens), repo: repo, hasher: h, tokens: tokens}
}

// work describes the calls that reached the repository, the hasher and the
// token issuer so far.
func (e recordingEnv) work() string {
	return fmt.Sprintf("repository %d, hashes %d, checks %d, tokens %d",
		e.repo.calls.Load(), e.hasher.hashes.Load(), e.hasher.verifies.Load(), e.tokens.calls.Load())
}

// allocatedPerCall returns the bytes one call of f allocates, averaged over
// a few sequential calls on one processor, so other goroutines add little.
func allocatedPerCall(f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f() // a first call may allocate what later calls reuse
	const runs = 5
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// oversizedPassword is far over the limit before normalization and 18
// times longer after it.
var oversizedPassword = strings.Repeat("\ufdfa", (256<<10)/3)

// An oversized password is a validation error that costs about as much as
// any other: no storage, hashing or token work, and no normalization of
// the whole input, which would take memory in proportion to its size.
func TestOversizedPasswordIsRejectedWithoutWork(t *testing.T) {
	// Not parallel: it measures the allocations of the whole process.
	const limit = 64 << 10 // bytes per call; the full NFKC form alone is 2.8 MiB
	e := newRecordingEnv(t)
	calls := map[string]func(context.Context, string, string) (usecase.Session, error){
		"Register": e.svc.Register,
		"Login":    e.svc.Login,
	}
	for name, call := range calls {
		before := e.work()
		var err error
		allocated := allocatedPerCall(func() { _, err = call(context.Background(), "anna@example.com", oversizedPassword) })
		t.Logf("%s: %d bytes allocated per call for a %d-byte password", name, allocated, len(oversizedPassword))
		var v *domain.ValidationError
		if !errors.As(err, &v) || !strings.Contains(v.Fields["password"], "at most 128") {
			t.Fatalf("%s with an oversized password: %v, want a validation error for the password", name, err)
		}
		if after := e.work(); after != before {
			t.Errorf("%s with an oversized password did work: before %s, after %s", name, before, after)
		}
		if allocated > limit {
			t.Errorf("%s allocated %d bytes per call for a %d-byte password, want at most %d: the whole password is normalized before the length check",
				name, allocated, len(oversizedPassword), limit)
		}
	}
}

// A request whose context is already canceled is answered before any
// password work, whatever the password.
func TestCanceledRequestDoesNoPasswordWork(t *testing.T) {
	// Not parallel: it measures the allocations of the whole process.
	const limit = 64 << 10
	e := newRecordingEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func(context.Context, string, string) (usecase.Session, error){
		"Register": e.svc.Register,
		"Login":    e.svc.Login,
	}
	for name, call := range calls {
		for _, pw := range []string{password, oversizedPassword} {
			before := e.work()
			var err error
			allocated := allocatedPerCall(func() { _, err = call(ctx, "anna@example.com", pw) })
			t.Logf("%s, canceled context: %d bytes allocated per call for a %d-byte password", name, allocated, len(pw))
			if !errors.Is(err, context.Canceled) {
				t.Errorf("%s with a canceled context and a %d-byte password: %v, want context.Canceled", name, len(pw), err)
			}
			if after := e.work(); after != before {
				t.Errorf("%s with a canceled context did work: before %s, after %s", name, before, after)
			}
			if allocated > limit {
				t.Errorf("%s with a canceled context allocated %d bytes per call for a %d-byte password, want at most %d",
					name, allocated, len(pw), limit)
			}
		}
	}
}

type cancelingHasher struct{ cancel context.CancelFunc }

func (*cancelingHasher) Hash(context.Context, string) (string, error) { return "test hash", nil }
func (h *cancelingHasher) Verify(context.Context, string, string) (bool, error) {
	h.cancel()
	return true, nil
}

type observingIssuer struct{ calls int }

func (i *observingIssuer) Issue(int64) (string, time.Time, error) {
	i.calls++
	return "synthetic session", time.Now().Add(time.Hour), nil
}

func TestCancellationDuringSuccessfulVerifyDoesNotIssueSession(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			repo := memory.New()
			if known {
				if _, err := repo.CreateUser(context.Background(), "anna@example.com", "test hash"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			issuer := &observingIssuer{}
			svc := usecase.New(repo, &cancelingHasher{cancel}, issuer)
			session, err := svc.Login(ctx, "anna@example.com", password)
			if !errors.Is(err, context.Canceled) || session.Token != "" || issuer.calls != 0 {
				t.Fatalf("canceled verification issued a session: error=%v, calls=%d", err, issuer.calls)
			}
		})
	}
}
