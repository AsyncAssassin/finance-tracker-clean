package jwtauth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	secret = []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	t0     = time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func newPair(t *testing.T, issuedAt, verifiedAt time.Time) (*Issuer, *Verifier) {
	t.Helper()
	iss, err := NewIssuer(secret, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	iss.now = fixedClock(issuedAt)
	ver, err := newVerifier(secret, fixedClock(verifiedAt))
	if err != nil {
		t.Fatal(err)
	}
	return iss, ver
}

// sign builds a token with arbitrary claims and method, for the attacks.
func sign(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "42", "iss": issuer, "aud": audience,
		"iat": t0.Unix(), "exp": t0.Add(time.Hour).Unix(),
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	iss, ver := newPair(t, t0.Add(500*time.Millisecond), t0.Add(time.Hour))
	token, expires, err := iss.Issue(42)
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(t0.Add(24 * time.Hour)) {
		t.Errorf("expires = %s, want %s (whole seconds)", expires, t0.Add(24*time.Hour))
	}
	id, err := ver.Verify(token)
	if err != nil || id != 42 {
		t.Errorf("Verify = %d, %v; want 42", id, err)
	}
	if got := strings.Count(token, "."); got != 2 {
		t.Errorf("token has %d dots, want a compact JWS", got)
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()
	iss, ver := newPair(t, t0, t0.Add(24*time.Hour+time.Second))
	token, _, err := iss.Issue(42)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(token); !errors.Is(err, ErrInvalidToken) || !errors.Is(err, jwt.ErrTokenExpired) {
		t.Errorf("expired token: err = %v", err)
	}
}

func TestRejects(t *testing.T) {
	t.Parallel()
	_, ver := newPair(t, t0, t0.Add(time.Minute))
	without := func(key string) jwt.MapClaims {
		c := validClaims()
		delete(c, key)
		return c
	}
	with := func(key string, value any) jwt.MapClaims {
		c := validClaims()
		c[key] = value
		return c
	}
	tests := map[string]string{
		"empty":            "",
		"garbage":          "not.a.token",
		"wrong secret":     sign(t, jwt.SigningMethodHS256, []byte("another-secret-another-secret-123"), validClaims()),
		"alg none":         sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, validClaims()),
		"other hmac alg":   sign(t, jwt.SigningMethodHS512, secret, validClaims()),
		"foreign issuer":   sign(t, jwt.SigningMethodHS256, secret, with("iss", "someone-else")),
		"no issuer":        sign(t, jwt.SigningMethodHS256, secret, without("iss")),
		"foreign audience": sign(t, jwt.SigningMethodHS256, secret, with("aud", "another-api")),
		"no audience":      sign(t, jwt.SigningMethodHS256, secret, without("aud")),
		"no expiry":        sign(t, jwt.SigningMethodHS256, secret, without("exp")),
		"expiry boundary":  sign(t, jwt.SigningMethodHS256, secret, with("exp", t0.Add(time.Minute).Unix())),
		"not yet valid":    sign(t, jwt.SigningMethodHS256, secret, with("nbf", t0.Add(time.Hour).Unix())),
		"no issue time":    sign(t, jwt.SigningMethodHS256, secret, without("iat")),
		"issued in future": sign(t, jwt.SigningMethodHS256, secret, with("iat", t0.Add(time.Hour).Unix())),
		"no subject":       sign(t, jwt.SigningMethodHS256, secret, without("sub")),
		"text subject":     sign(t, jwt.SigningMethodHS256, secret, with("sub", "admin")),
		"zero subject":     sign(t, jwt.SigningMethodHS256, secret, with("sub", "0")),
		"negative subject": sign(t, jwt.SigningMethodHS256, secret, with("sub", "-5")),
		"overflow subject": sign(t, jwt.SigningMethodHS256, secret, with("sub", "9223372036854775808")),
		"numeric subject":  sign(t, jwt.SigningMethodHS256, secret, with("sub", 42)),
	}
	for name, token := range tests {
		if id, err := ver.Verify(token); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: Verify = %d, %v; want ErrInvalidToken", name, id, err)
		}
	}
	// The hand-made valid token passes, so the cases above fail for their
	// own reason and not because of the helper.
	if id, err := ver.Verify(sign(t, jwt.SigningMethodHS256, secret, validClaims())); err != nil || id != 42 {
		t.Errorf("hand-made valid token: %d, %v", id, err)
	}
}

func TestConstructorsCheckSettings(t *testing.T) {
	t.Parallel()
	short := secret[:MinSecretLen-1]
	if _, err := NewIssuer(short, time.Hour); err == nil {
		t.Error("NewIssuer accepted a short secret")
	}
	if _, err := NewIssuer(secret, 0); err == nil {
		t.Error("NewIssuer accepted a zero lifetime")
	}
	if _, err := NewVerifier(short); err == nil {
		t.Error("NewVerifier accepted a short secret")
	}
	if _, err := NewVerifier(secret); err != nil {
		t.Errorf("NewVerifier: %v", err)
	}
}
