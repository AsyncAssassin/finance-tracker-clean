package grpckit

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
)

// The interceptors run in the order request ID → logging → recovery, so
// the log line of a call has its ID and shows a recovered panic as INTERNAL.

// withRequestID takes the request ID the gateway sent in the metadata, or
// generates one for direct calls such as grpcurl.
func withRequestID(ctx context.Context) context.Context {
	id := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(requestid.MetadataKey); len(v) > 0 {
			id = v[0]
		}
	}
	if !requestid.Valid(id) {
		id = requestid.New()
	}
	return requestid.With(ctx, id)
}

func unaryRequestID(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	return next(withRequestID(ctx), req)
}

func streamRequestID(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	return next(srv, &contextStream{ServerStream: ss, ctx: withRequestID(ss.Context())})
}

// contextStream replaces the context of a server stream.
type contextStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *contextStream) Context() context.Context { return s.ctx }

func unaryLogging(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		logCall(ctx, log, info.FullMethod, start, err)
		return resp, err
	}
}

func streamLogging(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		start := time.Now()
		err := next(srv, ss)
		logCall(ss.Context(), log, info.FullMethod, start, err)
		return err
	}
}

// logCall writes one line per call. Codes that point at a fault of the
// ledger are errors; the rest are the normal answers to bad requests.
func logCall(ctx context.Context, log *slog.Logger, method string, start time.Time, err error) {
	code := status.Code(err)
	level := slog.LevelInfo
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unimplemented, codes.Unavailable:
		level = slog.LevelError
	}
	log.Log(ctx, level, "rpc",
		"method", method,
		"code", code.String(),
		"duration_ms", time.Since(start).Milliseconds(),
		"request_id", requestid.From(ctx),
	)
}

func unaryRecovery(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
		defer recoverPanic(ctx, log, info.FullMethod, &err)
		return next(ctx, req)
	}
}

func streamRecovery(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) (err error) {
		defer recoverPanic(ss.Context(), log, info.FullMethod, &err)
		return next(srv, ss)
	}
}

// recoverPanic turns a panic in a handler into an INTERNAL status and a log
// line with the stack, so one bad call cannot take the ledger down.
func recoverPanic(ctx context.Context, log *slog.Logger, method string, err *error) {
	p := recover()
	if p == nil {
		return
	}
	log.ErrorContext(ctx, "panic in rpc handler",
		"method", method, "panic", p, "stack", string(debug.Stack()), "request_id", requestid.From(ctx))
	*err = status.Error(codes.Internal, "internal error")
}
