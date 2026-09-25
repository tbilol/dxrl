// Package grpcserver exposes agrogram.catalog.v1.CatalogService over
// gRPC, with the standard health service and optional server reflection.
package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	catalogv1 "github.com/tbilol/dxrl/gen/go/catalog/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpchealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/proto"

	"github.com/tbilol/dxrl/services/catalog/internal/apperr"
	"github.com/tbilol/dxrl/services/catalog/internal/auth"
)

// Options configure the server.
type Options struct {
	Logger     *slog.Logger
	Validator  protovalidate.Validator
	Reflection bool // grpcurl-friendly; keep off in production
}

// New returns a gRPC server with CatalogService, health and (optionally)
// reflection registered. Unary interceptors, outermost first: recovery,
// request context (Principal), logging and error mapping, validation.
func New(svc Service, health *grpchealth.Server, opts Options) *grpc.Server {
	log := opts.Logger
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			recoveryInterceptor(log),
			principalInterceptor(log),
			loggingInterceptor(log),
			validationInterceptor(opts.Validator),
		),
		grpc.ChainStreamInterceptor(streamRecoveryInterceptor(log)),
	)
	catalogv1.RegisterCatalogServiceServer(srv, &handler{svc: svc})
	healthpb.RegisterHealthServer(srv, health)
	if opts.Reflection {
		reflection.Register(srv)
	}
	return srv
}

// ServiceName is the gRPC health service name of CatalogService.
var ServiceName = catalogv1.CatalogService_ServiceDesc.ServiceName

// GracefulStop stops accepting calls and waits for running ones until ctx
// ends, then stops hard.
func GracefulStop(ctx context.Context, srv *grpc.Server) {
	done := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		srv.Stop()
		<-done
	}
}

func recoveryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ctx, "panic in RPC handler", "method", info.FullMethod,
					"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = apperr.Internal(fmt.Errorf("panic: %v", r)).GRPCStatus().Err()
			}
		}()
		return next(ctx, req)
	}
}

func streamRecoveryInterceptor(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				log.ErrorContext(ss.Context(), "panic in stream handler", "method", info.FullMethod,
					"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = apperr.Internal(fmt.Errorf("panic: %v", r)).GRPCStatus().Err()
			}
		}()
		return next(srv, ss)
	}
}

// principalInterceptor decodes the x-agrogram-* metadata into a Principal and
// echoes the request id in the response header.
func principalInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		p, err := auth.FromMetadata(md)
		_ = grpc.SetHeader(ctx, metadata.Pairs(auth.HeaderRequestID, p.RequestID))
		if err != nil {
			ae := apperr.From(err)
			log.WarnContext(ctx, "rpc rejected: bad auth metadata", "method", info.FullMethod,
				"request_id", p.RequestID, "code", ae.Code.String(), "error", ae.Message)
			return nil, ae.GRPCStatus().Err()
		}
		return next(auth.NewContext(ctx, p), req)
	}
}

// loggingInterceptor logs one line per call and turns every error into a
// status carrying exactly one agrogram.common.v1.Error.
func loggingInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		p, _ := auth.FromContext(ctx)
		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("request_id", p.RequestID),
			slog.Duration("duration", time.Since(start)),
		}
		if p.Authenticated() {
			attrs = append(attrs, slog.String("user_id", p.UserID.String()))
		}
		if p.Caller != "" {
			attrs = append(attrs, slog.String("caller", p.Caller))
		}
		level := slog.LevelInfo
		if strings.HasPrefix(info.FullMethod, "/grpc.health.v1.Health/") {
			level = slog.LevelDebug
		}
		if err == nil {
			log.LogAttrs(ctx, level, "rpc", append(attrs, slog.String("code", codes.OK.String()))...)
			return resp, nil
		}
		ae := apperr.From(err)
		attrs = append(attrs, slog.String("code", ae.Code.String()), slog.String("reason", ae.Reason.String()))
		if ae.Cause != nil {
			attrs = append(attrs, slog.String("error", ae.Cause.Error()))
		}
		if serverFault(ae.Code) {
			level = slog.LevelError
		}
		log.LogAttrs(ctx, level, "rpc", attrs...)
		return nil, ae.GRPCStatus().Err()
	}
}

func serverFault(c codes.Code) bool {
	switch c {
	case codes.Internal, codes.Unknown, codes.DataLoss, codes.Unavailable, codes.DeadlineExceeded:
		return true
	}
	return false
}

// validationInterceptor enforces the protovalidate annotations of every
// request: INVALID_ARGUMENT with ERROR_REASON_VALIDATION_FAILED.
func validationInterceptor(v protovalidate.Validator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if msg, ok := req.(proto.Message); ok {
			if err := v.Validate(msg); err != nil {
				return nil, validationError(err)
			}
		}
		return next(ctx, req)
	}
}

func validationError(err error) error {
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) {
		return apperr.Internal(fmt.Errorf("validate request: %w", err))
	}
	violations := make([]*commonv1.FieldViolation, 0, len(ve.Violations))
	for _, v := range ve.Violations {
		violations = append(violations, &commonv1.FieldViolation{
			Field:       protovalidate.FieldPathString(v.Proto.GetField()),
			Description: v.Proto.GetMessage(),
			Rule:        v.Proto.GetRuleId(),
		})
	}
	return apperr.Validation(violations...)
}
