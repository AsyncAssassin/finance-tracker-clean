package grpckit

import (
	"context"
	"net/url"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
)

// connectParams bound how long a call waits for a connection. When a
// service container stops, the channel still knows its old address, and a
// connection attempt to it would hang for the default 20 seconds, turning
// "the service is down" (502) into a timeout (504). Inside one network a
// connection takes milliseconds, so 2 seconds is generous. Retries back off
// to at most 5 seconds, so a restarted service is noticed quickly.
var connectParams = grpc.ConnectParams{
	Backoff:           backoff.Config{BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second},
	MinConnectTimeout: 2 * time.Second,
}

// MaxMsgSize is the largest message the services and the gateway accept:
// an import of the largest allowed CSV file, or its answer, in one message.
const MaxMsgSize = 16 << 20

// Dial returns a connection to the service at target, such as
// "ledger:9090", for the gateway. It does not connect yet: the connection
// is made on the first call, so the gateway starts while a service is down.
// Every unary call is limited to timeout; streams set their own deadline.
// The ID of the HTTP request travels in the metadata. opts are added to
// the defaults, tests use them to dial an in-memory listener.
//
// A target without a scheme uses the passthrough resolver: the host name
// is resolved on every connection attempt. gRPC's default DNS resolver
// backs off for up to two minutes after failed lookups, which is what
// happens while a service container is stopped, and the gateway would
// keep answering 502 long after the service is back.
func Dial(target string, timeout time.Duration, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts = append([]grpc.DialOption{
		// The services talk inside the compose network; TLS between them
		// is out of scope of the project.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(connectParams),
		// The answer to a large import can exceed the 4 MB default.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMsgSize)),
		grpc.WithChainUnaryInterceptor(unaryTimeout(timeout), unaryOutgoingRequestID),
		grpc.WithChainStreamInterceptor(streamOutgoingRequestID),
	}, opts...)
	return grpc.NewClient(withScheme(target), opts...)
}

// withScheme adds the passthrough scheme to a plain host:port target and
// keeps targets that name a registered resolver, such as dns:///ledger:9090
// or unix:/run/ledger.sock. It reads the target the way grpc.NewClient does.
func withScheme(target string) string {
	if u, err := url.Parse(target); err == nil && u.Scheme != "" && resolver.Get(u.Scheme) != nil {
		return target
	}
	return "passthrough:///" + target
}

// unaryTimeout limits every unary call to d, or to the timeout set with
// WithCallTimeout, so no method can forget it.
func unaryTimeout(d time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		timeout := d
		if own, ok := ctx.Value(callTimeoutKey{}).(time.Duration); ok && own > 0 {
			timeout = own
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

type callTimeoutKey struct{}

// WithCallTimeout makes the unary calls made with ctx use d instead of the
// client's timeout, for the few calls that take longer by design, such as
// an import of many rows.
func WithCallTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, callTimeoutKey{}, d)
}

// unaryOutgoingRequestID and streamOutgoingRequestID forward the ID of the
// HTTP request, so the logs of the gateway and the service share it.
func unaryOutgoingRequestID(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	return invoker(outgoingRequestID(ctx), method, req, reply, cc, opts...)
}

func streamOutgoingRequestID(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return streamer(outgoingRequestID(ctx), desc, cc, method, opts...)
}

func outgoingRequestID(ctx context.Context) context.Context {
	if id := requestid.From(ctx); requestid.Valid(id) {
		return metadata.AppendToOutgoingContext(ctx, requestid.MetadataKey, id)
	}
	return ctx
}
