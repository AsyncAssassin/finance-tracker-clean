package rest

import (
	"context"
	"fmt"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/authclient"
	"net"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSlowAuthBodyReturnsTimeoutAndReleasesSlot(t *testing.T) {
	t.Parallel()
	auth := &fakeAuth{login: func(context.Context, string, string) (authclient.Session, error) { return authclient.Session{}, nil }}
	h := newHandlerWithAuth(&fakeLedger{}, auth)
	s := httptest.NewServer(h)
	defer s.Close()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = fmt.Fprint(conn, "POST /api/auth/login HTTP/1.1\r\nHost: gateway\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	response, body, err := readAnswer(conn, authBodyTimeout+2*time.Second)
	if err != nil {
		t.Fatalf("timeout response was not delivered: %v", err)
	}
	decodeError(t, response, body, 408, codeTimeout)
	response, _ = do(t, h, "POST", "/api/auth/login", `{"email":"anna@example.com","password":"passphrase"}`)
	if response.StatusCode != 200 {
		t.Fatalf("slot not released: %d", response.StatusCode)
	}
}
