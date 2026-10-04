package grpckit

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/requestid"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/validation"
)

// Status converts the errors all services share into a gRPC status:
//
//	*validation.Error         → INVALID_ARGUMENT + google.rpc.BadRequest
//	context.Canceled          → CANCELLED
//	context.DeadlineExceeded  → DEADLINE_EXCEEDED
//	anything else             → INTERNAL with a generic message
//
// The text of an internal error goes to log with the request ID and not to
// the caller: it may describe the database and is of no use to a client.
// Services check their own errors first and call Status for the rest.
func Status(ctx context.Context, log *slog.Logger, err error) error {
	var v *validation.Error
	switch {
	case errors.As(err, &v):
		return InvalidArgument(v)
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "deadline exceeded")
	default:
		log.ErrorContext(ctx, "internal error", "error", err, "request_id", requestid.From(ctx))
		return status.Error(codes.Internal, "internal error")
	}
}

// InvalidArgument reports invalid fields as INVALID_ARGUMENT with a
// google.rpc.BadRequest listing them in a stable order.
func InvalidArgument(v *validation.Error) error {
	br := &errdetails.BadRequest{}
	for _, f := range v.SortedFields() {
		br.FieldViolations = append(br.FieldViolations, &errdetails.BadRequest_FieldViolation{
			Field:       f,
			Description: v.Fields[f],
		})
	}
	return WithDetails(codes.InvalidArgument, v.Error(), br)
}

// WithDetails builds a status error carrying detail. Attaching a detail
// fails only when it cannot be marshaled, which no generated message can;
// the bare status is still correct then.
func WithDetails(code codes.Code, msg string, detail protoadapt.MessageV1) error {
	st := status.New(code, msg)
	if withDetail, err := st.WithDetails(detail); err == nil {
		st = withDetail
	}
	return st.Err()
}
