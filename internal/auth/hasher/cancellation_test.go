package hasher

import (
	"context"
	"errors"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"testing"
	"time"
)

func TestAlreadyCanceledHashDoesNotCompute(t *testing.T) {
	t.Parallel()
	h := New(DefaultParams, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	for range 128 {
		if _, err := h.Hash(ctx, "a long enough passphrase"); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled hashing performed expensive work")
	}
	if len(h.slots) != 0 {
		t.Fatal("canceled hash leaked a slot")
	}
}
func TestHasherWaitIsBounded(t *testing.T) {
	t.Parallel()
	h := New(fast, 1)
	h.slots <- struct{}{}
	start := time.Now()
	if _, err := h.Hash(context.Background(), "a long enough passphrase"); !errors.Is(err, domain.ErrBusy) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("hash queue did not expire")
	}
	if len(h.slots) != 1 {
		t.Fatal("waiting caller released another caller's slot")
	}
	<-h.slots
	if _, err := h.Hash(context.Background(), "a long enough passphrase"); err != nil {
		t.Fatal(err)
	}
}
