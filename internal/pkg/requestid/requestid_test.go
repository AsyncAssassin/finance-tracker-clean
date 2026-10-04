package requestid

import (
	"context"
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   string
		want bool
	}{
		{"req-42", true},
		{"b92b817b8b7d16aa", true},
		{strings.Repeat("a", MaxLen), true},
		{"", false},
		{strings.Repeat("a", MaxLen+1), false},
		{"with space", false},
		{"line\nbreak", false},
		{"кириллица", false},
		{"tab\t", false},
	}
	for _, tc := range tests {
		if got := Valid(tc.id); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestContext(t *testing.T) {
	t.Parallel()
	if id := From(context.Background()); id != "" {
		t.Errorf("From(empty context) = %q, want empty", id)
	}
	if id := From(With(context.Background(), "req-1")); id != "req-1" {
		t.Errorf("From(With(req-1)) = %q", id)
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	a, b := New(), New()
	if len(a) != 16 || !Valid(a) || a == b {
		t.Errorf("New() = %q, %q; want two different valid 16-digit ids", a, b)
	}
}
