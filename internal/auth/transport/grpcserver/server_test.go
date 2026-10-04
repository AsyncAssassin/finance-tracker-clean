package grpcserver_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	authv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/auth/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/hasher"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/repo/memory"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/transport/grpcserver"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/jwtauth"
)

const password = "зелёный трамвай едет"

var secret = []byte("0123456789abcdef0123456789abcdef")

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// brokenRepo fails every call, to check how internal errors look.
type brokenRepo struct{}

func (brokenRepo) CreateUser(context.Context, string, string) (domain.User, error) {
	return domain.User{}, errors.New("connection to 10.0.0.5:5432 refused")
}

func (brokenRepo) UserByEmail(context.Context, string) (domain.User, error) {
	return domain.User{}, errors.New("connection to 10.0.0.5:5432 refused")
}

func start(t *testing.T, repo usecase.Repository) (authv1.AuthServiceClient, *syncBuffer) {
	t.Helper()
	issuer, err := jwtauth.NewIssuer(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	fast := hasher.New(hasher.Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}, 0)
	srv := grpcserver.New(usecase.New(repo, fast, issuer), slog.New(slog.NewJSONHandler(logs, nil)))
	ln := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return authv1.NewAuthServiceClient(conn), logs
}

func TestRegisterAndLogin(t *testing.T) {
	t.Parallel()
	c, logs := start(t, memory.New())
	ctx := context.Background()
	verifier, _ := jwtauth.NewVerifier(secret)

	reg, err := c.Register(ctx, &authv1.RegisterRequest{Email: "Anna@Example.com", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	u := reg.GetUser()
	if u.GetId() <= 0 || u.GetEmail() != "anna@example.com" || u.GetCreatedAt() == nil || reg.GetToken().GetExpiresAt() == nil {
		t.Errorf("Register = %v", reg)
	}
	if id, err := verifier.Verify(reg.GetToken().GetAccessToken()); err != nil || id != u.GetId() {
		t.Errorf("registration token: %d, %v", id, err)
	}

	login, err := c.Login(ctx, &authv1.LoginRequest{Email: "anna@example.com", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	if id, err := verifier.Verify(login.GetToken().GetAccessToken()); err != nil || id != u.GetId() {
		t.Errorf("login token: %d, %v", id, err)
	}
	if strings.Contains(logs.String(), password) || strings.Contains(logs.String(), "трамвай") {
		t.Error("the password reached the logs")
	}
}

func TestStatuses(t *testing.T) {
	t.Parallel()
	c, _ := start(t, memory.New())
	ctx := context.Background()
	if _, err := c.Register(ctx, &authv1.RegisterRequest{Email: "anna@example.com", Password: password}); err != nil {
		t.Fatal(err)
	}

	_, err := c.Register(ctx, &authv1.RegisterRequest{Email: "ANNA@example.com", Password: "another long passphrase"})
	if st := status.Convert(err); st.Code() != codes.AlreadyExists {
		t.Errorf("taken email: %v, want ALREADY_EXISTS", st)
	}

	_, err = c.Register(ctx, &authv1.RegisterRequest{Email: "anna", Password: "short"})
	st := status.Convert(err)
	fields := map[string]bool{}
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				fields[v.GetField()] = true
			}
		}
	}
	if st.Code() != codes.InvalidArgument || !fields["email"] || !fields["password"] {
		t.Errorf("bad input: %v with fields %v, want INVALID_ARGUMENT for email and password", st, fields)
	}

	_, wrong := c.Login(ctx, &authv1.LoginRequest{Email: "anna@example.com", Password: "зелёный трамвай стоит"})
	_, unknown := c.Login(ctx, &authv1.LoginRequest{Email: "boris@example.com", Password: password})
	ws, us := status.Convert(wrong), status.Convert(unknown)
	if ws.Code() != codes.Unauthenticated || us.Code() != codes.Unauthenticated || ws.Message() != us.Message() {
		t.Errorf("wrong password: %v; unknown email: %v; want the same UNAUTHENTICATED", ws, us)
	}
}

func TestInternalErrorIsHidden(t *testing.T) {
	t.Parallel()
	c, logs := start(t, brokenRepo{})
	_, err := c.Login(context.Background(), &authv1.LoginRequest{Email: "anna@example.com", Password: password})
	st := status.Convert(err)
	if st.Code() != codes.Internal || strings.Contains(st.Message(), "10.0.0.5") {
		t.Errorf("status = %v, want INTERNAL without the cause", st)
	}
	if !strings.Contains(logs.String(), "10.0.0.5") {
		t.Error("the cause is not in the log")
	}
}

// A caller inside the network can send a password far over the gateway's
// body limit; it is rejected as too long like any other and never reaches
// the logs.
func TestOversizedPasswordIsInvalidArgument(t *testing.T) {
	t.Parallel()
	c, logs := start(t, memory.New())
	ctx := context.Background()
	huge := strings.Repeat("\ufdfa", (2<<20)/3) // 2 MiB: over the gateway's body limit, under the gRPC default
	calls := map[string]func() error{
		"Register": func() error {
			_, err := c.Register(ctx, &authv1.RegisterRequest{Email: "anna@example.com", Password: huge})
			return err
		},
		"Login": func() error {
			_, err := c.Login(ctx, &authv1.LoginRequest{Email: "anna@example.com", Password: huge})
			return err
		},
	}
	for name, call := range calls {
		st := status.Convert(call())
		var problem string
		for _, d := range st.Details() {
			if br, ok := d.(*errdetails.BadRequest); ok {
				for _, v := range br.GetFieldViolations() {
					if v.GetField() == "password" {
						problem = v.GetDescription()
					}
				}
			}
		}
		if st.Code() != codes.InvalidArgument || !strings.Contains(problem, "at most 128") {
			t.Errorf("%s: %v with password problem %q, want INVALID_ARGUMENT and at most 128 characters", name, st, problem)
		}
	}
	if strings.Contains(logs.String(), "\ufdfa") {
		t.Error("the password reached the logs")
	}
}
