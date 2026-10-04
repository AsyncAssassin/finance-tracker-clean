package grpckit

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// The transport fix cited by GHSA-2v4p-qf9q-27wj rejects a request missing
// both :authority and Host before it reaches an interceptor or handler.
func TestRejectsMissingAuthorityAndRemainsHealthy(t *testing.T) {
	t.Parallel()
	srv := NewServer("test", slog.New(slog.DiscardHandler))
	srv.RegisterService(&echoDesc, struct{}{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	var block bytes.Buffer
	enc := hpack.NewEncoder(&block)
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "POST"},
		{Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/grpc.health.v1.Health/Check"},
		{Name: "content-type", Value: "application/grpc"},
		{Name: "te", Value: "trailers"},
	} {
		if err := enc.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("no HTTP rejection: %v", err)
		}
		if settings, ok := frame.(*http2.SettingsFrame); ok && !settings.IsAck() {
			if err := fr.WriteSettingsAck(); err != nil {
				t.Fatal(err)
			}
		}
		if headers, ok := frame.(*http2.MetaHeadersFrame); ok && headers.StreamID == 1 {
			if got := headers.PseudoValue("status"); got != "400" {
				t.Fatalf("missing authority: HTTP status = %q, want 400", got)
			}
			break
		}
	}
	_ = conn.Close()
	if err := CheckHealth(ln.Addr().String(), echoDesc.ServiceName); err != nil {
		t.Fatalf("health after malformed HTTP/2 request: %v", err)
	}
}
