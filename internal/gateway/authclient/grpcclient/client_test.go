package grpcclient

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/auth/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/hasher"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
)

const password = "зелёный трамвай едет"

var secret = []byte("0123456789abcdef0123456789abcdef")

func dial(t *testing.T, ln *bufconn.Listener) *Client {
	t.Helper()
	c, err := New("passthrough:///bufnet", 5*time.Second, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return ln.DialContext(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newAuth starts the real auth server over an in-memory store.
func newAuth(t *testing.T) *Client {
	t.Helper()
	issuer, err := jwtauth.NewIssuer(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fast := hasher.New(hasher.Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}, 0)
	srv := grpcserver.New(usecase.New(memory.New(), fast, issuer), slog.New(slog.DiscardHandler))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	return dial(t, ln)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c := newAuth(t)
	ctx := context.Background()
	verifier, _ := jwtauth.NewVerifier(secret)

	reg, err := c.Register(ctx, "Anna@Example.com", password)
	if err != nil {
		t.Fatal(err)
	}
	if reg.User.ID <= 0 || reg.User.Email != "anna@example.com" || reg.User.CreatedAt.IsZero() || reg.ExpiresAt.IsZero() {
		t.Errorf("Register = %+v", reg)
	}
	if id, err := verifier.Verify(reg.AccessToken); err != nil || id != reg.User.ID {
		t.Errorf("token user = %d, %v", id, err)
	}
	login, err := c.Login(ctx, "anna@example.com", password)
	if err != nil || login.User.ID != reg.User.ID {
		t.Errorf("Login = %+v, %v", login, err)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	c := newAuth(t)
	ctx := context.Background()
	if _, err := c.Register(ctx, "anna@example.com", password); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(ctx, "anna@example.com", password); !errors.Is(err, authclient.ErrEmailTaken) {
		t.Errorf("taken email: %v", err)
	}
	if _, err := c.Login(ctx, "anna@example.com", "wrong password here"); !errors.Is(err, authclient.ErrInvalidCredentials) {
		t.Errorf("wrong password: %v", err)
	}
	_, err := c.Register(ctx, "anna", "short")
	var v *upstream.ValidationError
	if !errors.As(err, &v) || v.Fields["email"] == "" || v.Fields["password"] == "" {
		t.Errorf("policy violation: %v", err)
	}
}

func TestAuthDown(t *testing.T) {
	t.Parallel()
	ln := bufconn.Listen(1 << 10)
	_ = ln.Close()
	_, err := dial(t, ln).Login(context.Background(), "anna@example.com", password)
	var down *upstream.UnavailableError
	if !errors.As(err, &down) || down.Service != "auth" {
		t.Errorf("err = %v, want UnavailableError for auth", err)
	}
}

func TestThrottleCrossesGRPCBoundary(t *testing.T) {
	t.Parallel()
	c := newAuth(t)
	ctx := context.Background()
	for range 5 {
		if _, err := c.Login(ctx, "unknown@example.com", password); !errors.Is(err, authclient.ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	_, err := c.Login(ctx, "UNKNOWN@example.com", password)
	var throttled *authclient.ThrottledError
	if !errors.As(err, &throttled) || throttled.RetryAfter <= 0 || throttled.RetryAfter > time.Minute {
		t.Fatalf("throttle from Auth through gRPC = %v", err)
	}
}

func TestIncompleteSession(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		u *authv1.User
		t *authv1.Token
	}{
		"no user":         {nil, &authv1.Token{AccessToken: "x", ExpiresAt: timestamppb.Now()}},
		"no token":        {&authv1.User{Id: 1, CreatedAt: timestamppb.Now()}, nil},
		"empty token":     {&authv1.User{Id: 1, CreatedAt: timestamppb.Now()}, &authv1.Token{ExpiresAt: timestamppb.Now()}},
		"no expiry":       {&authv1.User{Id: 1, CreatedAt: timestamppb.Now()}, &authv1.Token{AccessToken: "x"}},
		"no created time": {&authv1.User{Id: 1}, &authv1.Token{AccessToken: "x", ExpiresAt: timestamppb.Now()}},
	} {
		if _, err := toSession(tc.u, tc.t); err == nil {
			t.Errorf("%s: toSession accepted an incomplete session", name)
		}
	}
}
