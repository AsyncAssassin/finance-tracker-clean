// Package jwtauth issues and verifies the access tokens of the project:
// JWTs signed with HS256 whose subject is the user ID. The auth service
// issues them, the gateway verifies them locally on every request.
//
// Verification follows RFC 8725: only HS256 is accepted, so a token cannot
// pick its own algorithm ("none" or a public-key one); the expiry, issue
// time, issuer and audience are required and checked; and the secret must
// be at least MinSecretLen random bytes rather than a memorable password.
package jwtauth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// MinSecretLen is the shortest accepted secret, 256 bits as the HS256
// key size. Generate one with: openssl rand -base64 32
const MinSecretLen = 32

// The fixed claims of every token. They tie a token to this project, so a
// token issued for something else with the same secret is rejected.
const (
	issuer   = "finance-tracker-auth"
	audience = "finance-tracker-api"
)

// ErrInvalidToken is wrapped by every error of Verify.
var ErrInvalidToken = errors.New("invalid token")

var errShortSecret = fmt.Errorf("the secret must be at least %d bytes", MinSecretLen)

// Issuer creates access tokens.
type Issuer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewIssuer returns an Issuer signing with secret tokens valid for ttl.
func NewIssuer(secret []byte, ttl time.Duration) (*Issuer, error) {
	if len(secret) < MinSecretLen {
		return nil, errShortSecret
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("the token lifetime must be positive, got %s", ttl)
	}
	return &Issuer{secret: secret, ttl: ttl, now: time.Now}, nil
}

// Issue returns a token for userID and the moment it expires.
func (i *Issuer) Issue(userID int64) (string, time.Time, error) {
	// JWT times are whole seconds; truncating keeps the returned expiry
	// equal to the one in the token.
	now := i.now().Truncate(time.Second)
	expires := now.Add(i.ttl)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   strconv.FormatInt(userID, 10),
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{audience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expires),
	})
	signed, err := token.SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, expires, nil
}

// Verifier checks access tokens.
type Verifier struct {
	secret []byte
	parser *jwt.Parser
}

// NewVerifier returns a Verifier for tokens signed with secret.
func NewVerifier(secret []byte) (*Verifier, error) {
	return newVerifier(secret, time.Now)
}

func newVerifier(secret []byte, now func() time.Time) (*Verifier, error) {
	if len(secret) < MinSecretLen {
		return nil, errShortSecret
	}
	return &Verifier{
		secret: secret,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithIssuer(issuer),
			jwt.WithAudience(audience),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithTimeFunc(now),
		),
	}, nil
}

// Verify checks the signature and the claims of token and returns the
// user ID it was issued for.
func (v *Verifier) Verify(token string) (int64, error) {
	var claims jwt.RegisteredClaims
	_, err := v.parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return v.secret, nil
	})
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if claims.IssuedAt == nil {
		return 0, fmt.Errorf("%w: no issue time", ErrInvalidToken)
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: subject %q is not a user ID", ErrInvalidToken, claims.Subject)
	}
	return id, nil
}
