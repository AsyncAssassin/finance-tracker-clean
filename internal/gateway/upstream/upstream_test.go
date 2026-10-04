package upstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

func TestFromStatus(t *testing.T) {
	t.Parallel()
	with := func(code codes.Code, msg string, details ...protoadapt.MessageV1) error {
		st, err := status.New(code, msg).WithDetails(details...)
		if err != nil {
			t.Fatal(err)
		}
		return st.Err()
	}
	tests := []struct {
		name  string
		in    error
		check func(error) bool
	}{
		{"bad request", with(codes.InvalidArgument, "bad", &errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: "amount", Description: "must be positive"}}}),
			func(err error) bool {
				var v *ValidationError
				return errors.As(err, &v) && v.Fields["amount"] == "must be positive"
			}},
		{"invalid argument without fields", with(codes.InvalidArgument, "bad"),
			func(err error) bool { return errors.Is(err, ErrInvalidArgument) }},
		{"empty bad request", with(codes.InvalidArgument, "bad", &errdetails.BadRequest{}),
			func(err error) bool { return errors.Is(err, ErrInvalidArgument) }},
		{"failed precondition", with(codes.FailedPrecondition, "no"),
			func(err error) bool { return errors.Is(err, ErrConflict) }},
		{"already exists", with(codes.AlreadyExists, "dup"),
			func(err error) bool { return errors.Is(err, ErrConflict) }},
		{"aborted", with(codes.Aborted, "retry"),
			func(err error) bool { return errors.Is(err, ErrConflict) }},
		{"not found", with(codes.NotFound, "nothing"),
			func(err error) bool { return errors.Is(err, ErrNotFound) }},
		{"unavailable", with(codes.Unavailable, "connection refused"),
			func(err error) bool {
				var e *UnavailableError
				return errors.As(err, &e) && e.Service == "svc" && errors.Is(err, ErrUnavailable) &&
					e.Error() == "svc is unavailable: connection refused"
			}},
		{"deadline", with(codes.DeadlineExceeded, "slow"),
			func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }},
		{"canceled", with(codes.Canceled, "gone"),
			func(err error) bool { return errors.Is(err, context.Canceled) }},
		{"internal", with(codes.Internal, "internal error"),
			func(err error) bool {
				return err != nil && !errors.Is(err, ErrUnavailable) && strings.Contains(err.Error(), "svc: Internal")
			}},
		{"not a status", io.ErrUnexpectedEOF,
			func(err error) bool { return errors.Is(err, io.ErrUnexpectedEOF) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FromStatus("svc", tc.in); !tc.check(got) {
				t.Errorf("FromStatus(%v) = %#v", tc.in, got)
			}
		})
	}
}

func TestValidationErrorMessage(t *testing.T) {
	t.Parallel()
	err := &ValidationError{Fields: map[string]string{"to": "is required", "from": "is required"}}
	if want := "validation failed: from: is required; to: is required"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}
