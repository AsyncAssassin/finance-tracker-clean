package rest

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
)

// credentialsRequest is the body of POST /api/auth/register and
// POST /api/auth/login.
type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

const maxAuthBodyBytes = 16 << 10
const authBodyTimeout = 5 * time.Second

// sessionResponse is the answer to a registration or a login, in the
// shape of an OAuth 2.0 token response (RFC 6749, section 5.1).
type sessionResponse struct {
	User        authclient.User `json:"user"`
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"` // always "Bearer"
	ExpiresAt   time.Time       `json:"expires_at"`
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	s.authenticate(w, r, http.StatusCreated, s.auth.Register)
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	s.authenticate(w, r, http.StatusOK, s.auth.Login)
}

// authenticate reads the credentials, calls the auth service with call and
// answers with the session. The gateway checks only that both fields are
// present; the password policy belongs to the auth service.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request, status int,
	call func(ctx context.Context, email, password string) (authclient.Session, error)) {
	var req credentialsRequest
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(authBodyTimeout))
	if !decodeJSONLimit(w, r, &req, maxAuthBodyBytes) {
		return
	}
	// Clear only after consuming the body. On a read failure net/http must
	// not drain an unfinished body without a deadline before sending the error.
	_ = rc.SetReadDeadline(time.Time{})
	errs := fieldErrors{}
	if strings.TrimSpace(req.Email) == "" {
		errs.add("email", "is required")
	}
	if req.Password == "" {
		errs.add("password", "is required")
	}
	if errs.respond(w) {
		return
	}
	session, err := call(r.Context(), req.Email, req.Password)
	if err != nil {
		s.writeServiceError(w, r, err)
		return
	}
	// A token must not be kept by caches (RFC 6749, section 5.1).
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, sessionResponse{
		User:        session.User,
		AccessToken: session.AccessToken,
		TokenType:   "Bearer",
		ExpiresAt:   session.ExpiresAt,
	})
}
