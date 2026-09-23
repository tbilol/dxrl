package eijara

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

func TestNormalizeContractNumber(t *testing.T) {
	for in, want := range map[string]string{" ab-12/3 ": "AB-12/3", "123": "123", "x9-": "X9-"} {
		if got, ok := NormalizeContractNumber(in); !ok || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "ab", "/ab", "a b", "ab_1", "ab.1", strings.Repeat("A", 65)} {
		if _, ok := NormalizeContractNumber(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStub(t *testing.T) {
	// 2026-09-21 20:30 UTC is already 2026-09-22 in Tashkent.
	now := func() time.Time { return time.Date(2026, 9, 21, 20, 30, 0, 0, time.UTC) }
	s := Stub{Now: now}

	res, err := s.Check(context.Background(), "AB-1")
	if err != nil || res.Status != domain.VerificationVerified || res.Lease == nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if want := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC); !res.Lease.ValidFrom.Equal(want) ||
		!res.Lease.ValidUntil.Equal(want.AddDate(1, 0, 0)) || res.Lease.ContractNumber != "AB-1" {
		t.Fatalf("lease = %+v", res.Lease)
	}

	res, err = s.Check(context.Background(), "INVALID-9")
	if err != nil || res.Status != domain.VerificationRejected || res.RejectionReason != domain.ReasonContractInvalid {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	slow := Stub{Delay: time.Hour, Now: now}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := slow.Check(ctx, "AB-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delayed stub err = %v", err)
	}
}
