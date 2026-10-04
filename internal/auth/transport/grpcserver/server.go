// Package grpcserver exposes registration and login as the gRPC service
// auth.v1.AuthService. The server itself comes from grpckit; nothing here
// logs request bodies, so passwords never reach the logs.
package grpcserver

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/AsyncAssassin/finance-tracker-clean/api/proto/auth/v1"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/domain"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/auth/usecase"
	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/grpckit"
)

// New returns the gRPC server of the auth service for svc.
func New(svc *usecase.Service, log *slog.Logger) *grpckit.Server {
	if log == nil {
		log = slog.Default()
	}
	srv := grpckit.NewServer("auth", log)
	authv1.RegisterAuthServiceServer(srv, &handler{svc: svc, log: log})
	return srv
}

type handler struct {
	authv1.UnimplementedAuthServiceServer
	svc *usecase.Service
	log *slog.Logger
}

func (h *handler) Register(ctx context.Context, req *authv1.RegisterRequest) (*authv1.RegisterResponse, error) {
	s, err := h.svc.Register(ctx, req.GetEmail(), req.GetPassword())
	if err != nil {
		return nil, h.status(ctx, err)
	}
	return &authv1.RegisterResponse{User: toUser(s.User), Token: toToken(s)}, nil
}

func (h *handler) Login(ctx context.Context, req *authv1.LoginRequest) (*authv1.LoginResponse, error) {
	s, err := h.svc.Login(ctx, req.GetEmail(), req.GetPassword())
	if err != nil {
		return nil, h.status(ctx, err)
	}
	return &authv1.LoginResponse{User: toUser(s.User), Token: toToken(s)}, nil
}

// status converts an error of the use cases into a gRPC status:
//
//	domain.ErrEmailTaken          → ALREADY_EXISTS
//	domain.ErrInvalidCredentials  → UNAUTHENTICATED
//	*domain.ThrottledError         -> RESOURCE_EXHAUSTED + RetryInfo
//	the rest                      → grpckit.Status
func (h *handler) status(ctx context.Context, err error) error {
	var throttled *domain.ThrottledError
	switch {
	case errors.Is(err, domain.ErrRegistrationClosed):
		return status.Error(codes.PermissionDenied, domain.ErrRegistrationClosed.Error())
	case errors.Is(err, domain.ErrAccountQuota):
		return grpckit.WithDetails(codes.ResourceExhausted, domain.ErrAccountQuota.Error(),
			&errdetails.ErrorInfo{Reason: "ACCOUNT_QUOTA", Domain: "finance-tracker"})
	case errors.Is(err, domain.ErrBusy):
		return grpckit.WithDetails(codes.ResourceExhausted, domain.ErrBusy.Error(),
			&errdetails.ErrorInfo{Reason: "AUTH_BUSY", Domain: "finance-tracker"})
	case errors.As(err, &throttled):
		return grpckit.WithDetails(codes.ResourceExhausted, throttled.Error(),
			&errdetails.RetryInfo{RetryDelay: durationpb.New(throttled.RetryAfter)})
	case errors.Is(err, domain.ErrEmailTaken):
		return status.Error(codes.AlreadyExists, domain.ErrEmailTaken.Error())
	case errors.Is(err, domain.ErrInvalidCredentials):
		return status.Error(codes.Unauthenticated, domain.ErrInvalidCredentials.Error())
	default:
		return grpckit.Status(ctx, h.log, err)
	}
}

func toUser(u domain.User) *authv1.User {
	return &authv1.User{Id: u.ID, Email: u.Email, CreatedAt: timestamppb.New(u.CreatedAt)}
}

func toToken(s usecase.Session) *authv1.Token {
	return &authv1.Token{AccessToken: s.Token, ExpiresAt: timestamppb.New(s.ExpiresAt)}
}
