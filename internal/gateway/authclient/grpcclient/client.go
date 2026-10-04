// Package grpcclient implements authclient.Client over gRPC.
package grpcclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	authv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/auth/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/upstream"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
)

// Client talks to the auth service over one gRPC connection.
type Client struct {
	conn *grpc.ClientConn
	api  authv1.AuthServiceClient
}

var _ authclient.Client = (*Client)(nil)

// New returns a client for the auth service at target, such as
// "auth:9091". It does not connect yet; every call is limited to timeout.
// opts are added to the defaults of grpckit.Dial.
func New(target string, timeout time.Duration, opts ...grpc.DialOption) (*Client, error) {
	conn, err := grpckit.Dial(target, timeout, opts...)
	if err != nil {
		return nil, fmt.Errorf("auth client for %q: %w", target, err)
	}
	return &Client{conn: conn, api: authv1.NewAuthServiceClient(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) Register(ctx context.Context, email, password string) (authclient.Session, error) {
	resp, err := c.api.Register(ctx, &authv1.RegisterRequest{Email: email, Password: password})
	if err != nil {
		return authclient.Session{}, translate(err)
	}
	return toSession(resp.GetUser(), resp.GetToken())
}

func (c *Client) Login(ctx context.Context, email, password string) (authclient.Session, error) {
	resp, err := c.api.Login(ctx, &authv1.LoginRequest{Email: email, Password: password})
	if err != nil {
		return authclient.Session{}, translate(err)
	}
	return toSession(resp.GetUser(), resp.GetToken())
}

// toSession converts a response; a missing part is a bug of the auth
// service and becomes an internal error.
func toSession(u *authv1.User, t *authv1.Token) (authclient.Session, error) {
	if u == nil || t == nil || t.GetAccessToken() == "" || t.GetExpiresAt() == nil || u.GetCreatedAt() == nil {
		return authclient.Session{}, errors.New("auth sent an incomplete session")
	}
	return authclient.Session{
		User:        authclient.User{ID: u.GetId(), Email: u.GetEmail(), CreatedAt: u.GetCreatedAt().AsTime()},
		AccessToken: t.GetAccessToken(),
		ExpiresAt:   t.GetExpiresAt().AsTime(),
	}, nil
}

// translate converts a gRPC status from the auth service into an error:
//
//	ALREADY_EXISTS   → authclient.ErrEmailTaken          (HTTP 409)
//	UNAUTHENTICATED  → authclient.ErrInvalidCredentials  (HTTP 401)
//	the rest         → upstream.FromStatus
func translate(err error) error {
	switch status.Code(err) {
	case codes.PermissionDenied:
		return authclient.ErrRegistrationClosed
	case codes.ResourceExhausted:
		if info, ok := upstream.Detail[*errdetails.ErrorInfo](status.Convert(err)); ok && info.GetReason() == "ACCOUNT_QUOTA" && info.GetDomain() == "finance-tracker" {
			return authclient.ErrAccountQuota
		}
		if info, ok := upstream.Detail[*errdetails.ErrorInfo](status.Convert(err)); ok && info.GetReason() == "AUTH_BUSY" && info.GetDomain() == "finance-tracker" {
			return authclient.ErrBusy
		}
		// Only an explicit throttle with a valid delay becomes HTTP 429.
		// Other resource failures must not be mislabeled as login limits.
		if info, ok := upstream.Detail[*errdetails.RetryInfo](status.Convert(err)); ok {
			if delay := info.GetRetryDelay(); delay != nil && delay.CheckValid() == nil && delay.AsDuration() > 0 {
				return &authclient.ThrottledError{RetryAfter: delay.AsDuration()}
			}
		}
		return upstream.FromStatus("auth", err)
	case codes.AlreadyExists:
		return authclient.ErrEmailTaken
	case codes.Unauthenticated:
		return authclient.ErrInvalidCredentials
	default:
		return upstream.FromStatus("auth", err)
	}
}
