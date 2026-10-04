// Package authclient defines how the gateway talks to the auth service.
// The gateway depends only on this interface and these types; the
// grpcclient subpackage implements it over gRPC.
package authclient

import (
	"context"
	"errors"
	"time"
)

// Client is the auth service as seen by the gateway.
type Client interface {
	// Register creates an account and returns a session for it.
	Register(ctx context.Context, email, password string) (Session, error)
	// Login checks the credentials and returns a new session.
	Login(ctx context.Context, email, password string) (Session, error)
}

// User is an account. The JSON tags define the HTTP API form.
type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is an account with a fresh access token.
type Session struct {
	User        User
	AccessToken string
	ExpiresAt   time.Time
}

// ThrottledError carries the retry delay from Auth to the HTTP response.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (*ThrottledError) Error() string { return "too many authentication attempts, try again later" }

// Errors only the auth service produces. Generic errors, such as invalid
// fields or an unreachable service, come from the upstream package.
var (
	ErrEmailTaken         = errors.New("email is already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrBusy               = errors.New("authentication is busy, retry later")
	ErrRegistrationClosed = errors.New("registration is not available for this address")
	ErrAccountQuota       = errors.New("the account quota for this installation is reached")
)
