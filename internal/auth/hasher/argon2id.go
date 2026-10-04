// Package hasher stores passwords as Argon2id hashes, the first choice of
// the OWASP Password Storage Cheat Sheet. Unlike bcrypt it has no 72-byte
// input limit, which a passphrase in Cyrillic (two bytes per letter) would
// hit at 36 characters.
//
// A hash is kept in the PHC string format with its parameters, for example
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>
//
// so the parameters can be raised later and old hashes still verify.
package hasher

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters.
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLen     uint32 // bytes
	KeyLen      uint32 // bytes
}

// DefaultParams are the OWASP minimum: 19 MiB of memory, 2 iterations,
// 1 thread. One hash takes tens of milliseconds.
var DefaultParams = Params{Memory: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLen: 16, KeyLen: 32}

// Upper bounds for parameters read from a stored hash, so a corrupted or
// planted hash cannot make one verification use gigabytes of memory.
const (
	maxMemory      = 256 * 1024 // KiB
	maxIterations  = 16
	maxParallelism = 16
	maxKeyLen      = 64
	minKeyLen      = 16
	minSaltLen     = 8
)

// ErrMalformed means a stored hash is not an Argon2id PHC string this
// package can verify.
var ErrMalformed = errors.New("malformed password hash")

var b64 = base64.RawStdEncoding

// Argon2id hashes and verifies passwords. At most a fixed number of hashes
// run at once: each takes Params.Memory, and a burst of logins must not
// exhaust the memory of a small server. Waiting callers give up when
// their context ends.
type Argon2id struct {
	params Params
	slots  chan struct{}
}

// New returns a hasher with p that runs up to concurrency hashes at once;
// 0 means the number of CPUs.
func New(p Params, concurrency int) *Argon2id {
	if concurrency <= 0 {
		concurrency = runtime.GOMAXPROCS(0)
	}
	return &Argon2id{params: p, slots: make(chan struct{}, concurrency)}
}

// Hash returns the PHC string of password with a new random salt.
func (h *Argon2id) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	key, err := h.derive(ctx, password, salt, h.params)
	if err != nil {
		return "", err
	}
	p := h.params
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify reports whether password matches the PHC string encoded. The
// comparison takes the same time wherever the keys differ.
func (h *Argon2id) Verify(ctx context.Context, password, encoded string) (bool, error) {
	p, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	got, err := h.derive(ctx, password, salt, p)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

func (h *Argon2id) derive(ctx context.Context, password string, salt []byte, p Params) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	wait := time.NewTimer(250 * time.Millisecond)
	defer wait.Stop()
	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-wait.C:
		return nil, domain.ErrBusy
	}
	defer func() { <-h.slots }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLen)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return key, nil
}

// decode parses $argon2id$v=19$m=..,t=..,p=..$salt$key.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrMalformed
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("%w: unsupported version %q", ErrMalformed, parts[2])
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: parameters %q", ErrMalformed, parts[3])
	}
	if p.Memory == 0 || p.Memory > maxMemory || p.Iterations == 0 || p.Iterations > maxIterations ||
		p.Parallelism == 0 || p.Parallelism > maxParallelism || p.Memory < 8*uint32(p.Parallelism) {
		return Params{}, nil, nil, fmt.Errorf("%w: parameters %q out of range", ErrMalformed, parts[3])
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < minSaltLen {
		return Params{}, nil, nil, fmt.Errorf("%w: salt", ErrMalformed)
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < minKeyLen || len(key) > maxKeyLen {
		return Params{}, nil, nil, fmt.Errorf("%w: key", ErrMalformed)
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}
