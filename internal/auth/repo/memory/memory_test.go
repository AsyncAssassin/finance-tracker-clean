package memory_test

import (
	"context"
	"testing"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/repotest"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
)

func TestContract(t *testing.T) {
	repotest.Run(t, func(*testing.T) usecase.Repository { return memory.New() })
}

func TestIDsAreNotReusedAfterRestart(t *testing.T) {
	ctx := context.Background()
	seen := make(map[int64]bool)
	// No sleep: restarting a store or moving the wall clock must not
	// assign a former identity to a new account.
	for range 100 {
		u, err := memory.New().CreateUser(ctx, "anna@example.com", "h")
		if err != nil {
			t.Fatal(err)
		}
		if u.ID <= 0 || u.ID > 1<<53-1 || seen[u.ID] {
			t.Fatalf("reused or non-JavaScript-safe account ID: %d", u.ID)
		}
		seen[u.ID] = true
	}
}
