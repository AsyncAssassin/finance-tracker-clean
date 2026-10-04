// Package upstream holds the errors the gateway gets from the services
// behind it, whatever the service, and the translation of their gRPC
// statuses into these errors. The HTTP layer maps them to status codes.
// Errors that only one service produces, such as an exceeded budget, live
// in that service's client package.
package upstream

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/validation"
)

// Errors a service may return. ErrInvalidArgument and ErrConflict cover
// rejections that come without the details of a typed error.
var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrConflict        = errors.New("conflict")
	ErrNotFound        = errors.New("not found")
	ErrUnavailable     = errors.New("service is unavailable")
	ErrQuotaExceeded   = errors.New("account storage quota exceeded")
)

// ValidationError reports invalid request fields with a reason for each.
// It is the same type the services use, rebuilt from the field
// violations of an INVALID_ARGUMENT status.
type ValidationError = validation.Error

// UnavailableError reports a service the gateway cannot reach.
// errors.Is(err, ErrUnavailable) matches it.
type UnavailableError struct {
	Service string // such as "ledger"
	Reason  string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s is unavailable: %s", e.Service, e.Reason)
}

// Is makes errors.Is(err, ErrUnavailable) report true.
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// FromStatus converts a gRPC status from service into an error of this
// package; the HTTP layer then picks the status code:
//
//	gRPC code            status detail          error                     HTTP
//	INVALID_ARGUMENT     google.rpc.BadRequest  *ValidationError          400
//	INVALID_ARGUMENT     none                   ErrInvalidArgument        400
//	FAILED_PRECONDITION,
//	ALREADY_EXISTS,
//	ABORTED              none                   ErrConflict               409
//	NOT_FOUND            none                   ErrNotFound               404
//	UNAVAILABLE          none                   *UnavailableError         502
//	DEADLINE_EXCEEDED    none                   context.DeadlineExceeded  504
//	CANCELLED            none                   context.Canceled          499
//	any other code       none                   plain error               500
//
// Clients check the details their service defines before calling it. An
// error that is not a status is returned unchanged.
func FromStatus(service string, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	msg := st.Message()
	switch st.Code() {
	case codes.ResourceExhausted:
		if info, ok := Detail[*errdetails.ErrorInfo](st); ok && info.GetDomain() == "finance-tracker" && info.GetReason() == "STORAGE_QUOTA" {
			return ErrQuotaExceeded
		}
		return fmt.Errorf("%s: %s: %s", service, st.Code(), msg)
	case codes.InvalidArgument:
		if br, ok := Detail[*errdetails.BadRequest](st); ok && len(br.GetFieldViolations()) > 0 {
			fields := make(map[string]string, len(br.GetFieldViolations()))
			for _, v := range br.GetFieldViolations() {
				fields[v.GetField()] = v.GetDescription()
			}
			return &ValidationError{Fields: fields}
		}
		return fmt.Errorf("%w: %s", ErrInvalidArgument, msg)
	case codes.FailedPrecondition, codes.AlreadyExists, codes.Aborted:
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, msg)
	case codes.Unavailable:
		return &UnavailableError{Service: service, Reason: msg}
	case codes.DeadlineExceeded:
		return fmt.Errorf("%s: %w", service, context.DeadlineExceeded)
	case codes.Canceled:
		return fmt.Errorf("%s: %w", service, context.Canceled)
	default:
		return fmt.Errorf("%s: %s: %s", service, st.Code(), msg)
	}
}

// Detail returns the first detail of st with type T.
func Detail[T any](st *status.Status) (T, bool) {
	for _, d := range st.Details() {
		if v, ok := d.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}
