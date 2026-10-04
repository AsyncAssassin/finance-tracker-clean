package hasher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fast keeps the tests quick; the format and logic do not depend on cost.
var fast = Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	h := New(fast, 0)
	ctx := context.Background()
	const password = "зелёный трамвай едет"
	encoded, err := h.Hash(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=64,t=1,p=1$") || strings.Contains(encoded, password) {
		t.Errorf("hash = %q", encoded)
	}
	if ok, err := h.Verify(ctx, password, encoded); err != nil || !ok {
		t.Errorf("Verify(right password) = %v, %v", ok, err)
	}
	if ok, err := h.Verify(ctx, password+" ", encoded); err != nil || ok {
		t.Errorf("Verify(wrong password) = %v, %v", ok, err)
	}
	again, _ := h.Hash(ctx, password)
	if again == encoded {
		t.Error("two hashes of one password are equal: the salt is not random")
	}
}

func TestDefaultParams(t *testing.T) {
	t.Parallel()
	h := New(DefaultParams, 1)
	ctx := context.Background()
	encoded, err := h.Hash(ctx, "a long enough passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %q, want the OWASP parameters", encoded)
	}
	// A hash made with other parameters still verifies: they are read
	// from the stored string.
	old, _ := New(fast, 1).Hash(ctx, "a long enough passphrase")
	if ok, err := h.Verify(ctx, "a long enough passphrase", old); err != nil || !ok {
		t.Errorf("Verify of a hash with other parameters = %v, %v", ok, err)
	}
}

func TestLongCyrillicPassphrase(t *testing.T) {
	t.Parallel()
	h := New(fast, 0)
	ctx := context.Background()
	long := strings.Repeat("щука", 32) // 128 characters, 256 bytes: bcrypt would refuse it
	base, _ := h.Hash(ctx, long)
	if ok, _ := h.Verify(ctx, long, base); !ok {
		t.Fatal("the long passphrase does not verify")
	}
	changed := long[:len(long)-len("а")] + "о" // the last letter differs
	if ok, _ := h.Verify(ctx, changed, base); ok {
		t.Error("a passphrase differing in its last letter verifies: the input is truncated")
	}
}

func TestMalformedHashes(t *testing.T) {
	t.Parallel()
	h := New(fast, 0)
	good, _ := h.Hash(context.Background(), "a long enough passphrase")
	parts := strings.Split(good, "$")
	replace := func(i int, v string) string {
		p := append([]string(nil), parts...)
		p[i] = v
		return strings.Join(p, "$")
	}
	for name, encoded := range map[string]string{
		"empty":              "",
		"bcrypt":             "$2a$10$abcdefghijklmnopqrstuuSomeBcryptHashValueGoesHere123",
		"argon2i":            replace(1, "argon2i"),
		"other version":      replace(2, "v=16"),
		"bad parameters":     replace(3, "m=x,t=1,p=1"),
		"huge memory":        replace(3, "m=4194304,t=1,p=1"),
		"zero iterations":    replace(3, "m=64,t=0,p=1"),
		"too many threads":   replace(3, "m=1024,t=1,p=255"),
		"bad salt":           replace(4, "!!!"),
		"short salt":         replace(4, "YWJj"),
		"bad key":            replace(5, "!!!"),
		"short key":          replace(5, "YWJjZGVm"),
		"extra field":        good + "$x",
		"missing leading $":  strings.TrimPrefix(good, "$"),
		"memory below p*8KB": replace(3, "m=8,t=1,p=2"),
	} {
		if ok, err := h.Verify(context.Background(), "a long enough passphrase", encoded); ok || !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: Verify = %v, %v; want ErrMalformed", name, ok, err)
		}
	}
}

func TestConcurrencyLimit(t *testing.T) {
	t.Parallel()
	h := New(fast, 1)
	h.slots <- struct{}{} // the only slot is busy
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "a long enough passphrase"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Hash with no free slot = %v, want the context error", err)
	}
	<-h.slots

	// Many callers share the slots without a race.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := h.Hash(context.Background(), "a long enough passphrase"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
