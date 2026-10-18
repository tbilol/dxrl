// Package apperr is profile-social's error model. Every failure that leaves the
// service is a gRPC status whose details hold exactly one
// agrogram.common.v1.Error; the status code follows the ErrorReason table in
// proto/common/v1/error.proto.
package apperr

import (
	"context"
	"errors"
	"fmt"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Domain is the Error.domain of every error profile-social produces.
const Domain = "profile-social"

// Error is an API error. Cause is for logs only and never leaves the service.
type Error struct {
	Code       codes.Code
	Reason     commonv1.ErrorReason
	Message    string // English, for developers
	Metadata   map[string]string
	Violations []*commonv1.FieldViolation
	Cause      error
}

// New returns an error for reason with the code the ErrorReason table assigns.
// kv are metadata key/value pairs, e.g. "retry_after_seconds", "60".
func New(reason commonv1.ErrorReason, msg string, kv ...string) *Error {
	e := &Error{Code: CodeOf(reason), Reason: reason, Message: msg}
	if len(kv) > 0 {
		e.Metadata = make(map[string]string, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			e.Metadata[kv[i]] = kv[i+1]
		}
	}
	return e
}

// Validation returns an INVALID_ARGUMENT / VALIDATION_FAILED error.
func Validation(violations ...*commonv1.FieldViolation) *Error {
	e := New(commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED, "request failed validation")
	e.Violations = violations
	return e
}

// Internal wraps an unexpected error. The client sees a generic message.
func Internal(cause error) *Error {
	e := New(commonv1.ErrorReason_ERROR_REASON_INTERNAL, "internal error")
	e.Cause = cause
	return e
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s (%s): %s", e.Reason, e.Code, e.Message)
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Cause }

// Proto returns the agrogram.common.v1.Error carried in the status details.
func (e *Error) Proto() *commonv1.Error {
	return &commonv1.Error{
		Reason:          e.Reason,
		Message:         e.Message,
		Domain:          Domain,
		Metadata:        e.Metadata,
		FieldViolations: e.Violations,
	}
}

// GRPCStatus implements the interface grpc-go uses to turn an error into a
// status, so an *Error can be returned from a handler as is.
func (e *Error) GRPCStatus() *status.Status {
	st := status.New(e.Code, e.Message)
	if withDetails, err := st.WithDetails(e.Proto()); err == nil {
		return withDetails
	}
	return st
}

// From converts any error into an *Error: an *Error in the chain is returned
// as is, context errors keep their gRPC codes, gRPC statuses keep code and
// message, and anything else becomes INTERNAL (the cause stays in logs).
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	switch {
	case errors.Is(err, context.Canceled):
		return &Error{Code: codes.Canceled, Message: "request canceled", Cause: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: codes.DeadlineExceeded, Message: "deadline exceeded", Cause: err}
	}
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown && st.Code() != codes.OK {
		return &Error{Code: st.Code(), Reason: reasonForCode[st.Code()], Message: st.Message(), Cause: err}
	}
	return Internal(err)
}

// CodeOf returns the gRPC code the ErrorReason table assigns to r
// (codes.Unknown for ERROR_REASON_UNSPECIFIED or values added later).
func CodeOf(r commonv1.ErrorReason) codes.Code {
	if c, ok := codeByReason[r]; ok {
		return c
	}
	return codes.Unknown
}

// codeByReason mirrors the comments on agrogram.common.v1.ErrorReason.
var codeByReason = map[commonv1.ErrorReason]codes.Code{
	commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED:        codes.InvalidArgument,
	commonv1.ErrorReason_ERROR_REASON_NOT_FOUND:                codes.NotFound,
	commonv1.ErrorReason_ERROR_REASON_ALREADY_EXISTS:           codes.AlreadyExists,
	commonv1.ErrorReason_ERROR_REASON_CONFLICT:                 codes.Aborted,
	commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED:          codes.Unauthenticated,
	commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED:        codes.PermissionDenied,
	commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED:             codes.ResourceExhausted,
	commonv1.ErrorReason_ERROR_REASON_NOT_IMPLEMENTED:          codes.Unimplemented,
	commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE:   codes.Unavailable,
	commonv1.ErrorReason_ERROR_REASON_INTERNAL:                 codes.Internal,
	commonv1.ErrorReason_ERROR_REASON_FAILED_PRECONDITION:      codes.FailedPrecondition,
	commonv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS:      codes.Unauthenticated,
	commonv1.ErrorReason_ERROR_REASON_TOKEN_EXPIRED:            codes.Unauthenticated,
	commonv1.ErrorReason_ERROR_REASON_TOKEN_INVALID:            codes.Unauthenticated,
	commonv1.ErrorReason_ERROR_REASON_TOKEN_REVOKED:            codes.Unauthenticated,
	commonv1.ErrorReason_ERROR_REASON_PHONE_ALREADY_REGISTERED: codes.AlreadyExists,
	commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED:         codes.PermissionDenied,
	commonv1.ErrorReason_ERROR_REASON_PROVIDER_ALREADY_LINKED:  codes.AlreadyExists,
	commonv1.ErrorReason_ERROR_REASON_LAST_SIGN_IN_METHOD:      codes.FailedPrecondition,
	commonv1.ErrorReason_ERROR_REASON_OTP_INVALID:              codes.InvalidArgument,
	commonv1.ErrorReason_ERROR_REASON_LISTING_LIMIT_REACHED:    codes.ResourceExhausted,
	commonv1.ErrorReason_ERROR_REASON_LISTING_NOT_ACTIVE:       codes.FailedPrecondition,
	commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED:  codes.InvalidArgument,
	commonv1.ErrorReason_ERROR_REASON_UNSUPPORTED_MEDIA_TYPE:   codes.InvalidArgument,
	commonv1.ErrorReason_ERROR_REASON_MEDIA_TOO_LARGE:          codes.InvalidArgument,
	commonv1.ErrorReason_ERROR_REASON_MEDIA_PROCESSING_FAILED:  codes.FailedPrecondition,
}

// reasonForCode picks the generic reason for statuses created outside apperr
// (for example UNIMPLEMENTED from the generated server stubs).
var reasonForCode = map[codes.Code]commonv1.ErrorReason{
	codes.InvalidArgument:    commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED,
	codes.NotFound:           commonv1.ErrorReason_ERROR_REASON_NOT_FOUND,
	codes.AlreadyExists:      commonv1.ErrorReason_ERROR_REASON_ALREADY_EXISTS,
	codes.Aborted:            commonv1.ErrorReason_ERROR_REASON_CONFLICT,
	codes.Unauthenticated:    commonv1.ErrorReason_ERROR_REASON_UNAUTHENTICATED,
	codes.PermissionDenied:   commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
	codes.ResourceExhausted:  commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED,
	codes.Unimplemented:      commonv1.ErrorReason_ERROR_REASON_NOT_IMPLEMENTED,
	codes.Unavailable:        commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE,
	codes.Internal:           commonv1.ErrorReason_ERROR_REASON_INTERNAL,
	codes.FailedPrecondition: commonv1.ErrorReason_ERROR_REASON_FAILED_PRECONDITION,
}
