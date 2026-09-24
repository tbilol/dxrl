package apperr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errorDetail returns the single common.v1.Error in a status.
func errorDetail(t *testing.T, err error) (*status.Status, *commonv1.Error) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status: %v", err)
	}
	details := st.Details()
	if len(details) != 1 {
		t.Fatalf("want exactly one detail, got %d", len(details))
	}
	e, ok := details[0].(*commonv1.Error)
	if !ok {
		t.Fatalf("detail is %T", details[0])
	}
	return st, e
}

func TestStatusCarriesOneError(t *testing.T) {
	err := error(New(commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED, "slow down", "retry_after_seconds", "60"))
	st, e := errorDetail(t, From(fmt.Errorf("wrapped: %w", err)).GRPCStatus().Err())
	if st.Code() != codes.ResourceExhausted || st.Message() != "slow down" {
		t.Fatalf("status = %s %q", st.Code(), st.Message())
	}
	if e.GetReason() != commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED || e.GetDomain() != Domain ||
		e.GetMessage() != "slow down" || e.GetMetadata()["retry_after_seconds"] != "60" {
		t.Fatalf("error = %v", e)
	}

	v := Validation(&commonv1.FieldViolation{Field: "request_id", Rule: "string.uuid"})
	st, e = errorDetail(t, v.GRPCStatus().Err())
	if st.Code() != codes.InvalidArgument || e.GetReason() != commonv1.ErrorReason_ERROR_REASON_VALIDATION_FAILED ||
		len(e.GetFieldViolations()) != 1 || e.GetFieldViolations()[0].GetField() != "request_id" {
		t.Fatalf("validation = %s %v", st.Code(), e)
	}
}

func TestFrom(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		code   codes.Code
		reason commonv1.ErrorReason
	}{
		{"plain error hides its text", errors.New("pq: password leaked"), codes.Internal, commonv1.ErrorReason_ERROR_REASON_INTERNAL},
		{"canceled", fmt.Errorf("x: %w", context.Canceled), codes.Canceled, commonv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded, commonv1.ErrorReason_ERROR_REASON_UNSPECIFIED},
		{"foreign status", status.Error(codes.Unimplemented, "nope"), codes.Unimplemented, commonv1.ErrorReason_ERROR_REASON_NOT_IMPLEMENTED},
		{"wrapped app error", fmt.Errorf("ctx: %w", New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, "gone")), codes.NotFound, commonv1.ErrorReason_ERROR_REASON_NOT_FOUND},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ae := From(tt.err)
			st, e := errorDetail(t, ae.GRPCStatus().Err())
			if st.Code() != tt.code || e.GetReason() != tt.reason {
				t.Fatalf("got %s/%s", st.Code(), e.GetReason())
			}
			if tt.code == codes.Internal && st.Message() != "internal error" {
				t.Fatalf("internal message leaked: %q", st.Message())
			}
		})
	}
	if From(nil) != nil {
		t.Fatal("From(nil) != nil")
	}
}

func TestEveryReasonHasACode(t *testing.T) {
	for v, name := range commonv1.ErrorReason_name {
		r := commonv1.ErrorReason(v)
		if r == commonv1.ErrorReason_ERROR_REASON_UNSPECIFIED {
			continue
		}
		if c := CodeOf(r); c == codes.Unknown || c == codes.OK {
			t.Errorf("%s has no gRPC code", name)
		}
	}
	if CodeOf(commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED) != codes.AlreadyExists ||
		CodeOf(commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED) != codes.PermissionDenied {
		t.Fatal("table drifted from error.proto")
	}
}
