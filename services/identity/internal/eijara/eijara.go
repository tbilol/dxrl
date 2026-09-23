// Package eijara stands in for e-ijara, the government land-lease registry
// that confirms a farmer's lease contract.
//
// STUB: the e-ijara API does not exist yet. Everything here that decides an
// outcome is a placeholder; replace Stub with a real client implementing
// service.EIjaraChecker once the API is published.
package eijara

import (
	"context"
	"regexp"
	"strings"
	"time"

	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	identityv1 "github.com/tbilol/dxrl/gen/go/identity/v1"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// Result is e-ijara's answer for one contract number.
type Result struct {
	Status          identityv1.FarmerVerificationStatus // VERIFIED or REJECTED
	RejectionReason commonv1.ErrorReason                // set when REJECTED
	Lease           *domain.Lease                       // set when VERIFIED
}

// STUB: the real e-ijara contract number format is unknown. Until it is
// published we accept 3-64 letters, digits, '/' and '-' starting with a letter
// or digit, and compare numbers case-insensitively (stored upper case).
var contractNumberPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9/-]{2,63}$`)

// NormalizeContractNumber trims and upper-cases a contract number and reports
// whether it has an acceptable format.
func NormalizeContractNumber(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !contractNumberPattern.MatchString(s) {
		return "", false
	}
	return strings.ToUpper(s), true
}

// InvalidPrefix is the STUB test hook: contract numbers starting with it are
// REJECTED as LEASE_CONTRACT_INVALID, so the rejection path can be exercised.
const InvalidPrefix = "INVALID"

// Stub answers without calling anything.
type Stub struct {
	// Delay simulates e-ijara latency (IDENTITY_EIJARA_STUB_DELAY, default 0).
	Delay time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Check implements service.EIjaraChecker.
func (s Stub) Check(ctx context.Context, contractNumber string) (Result, error) {
	// STUB: simulated latency, so the timeout -> PENDING -> worker path can be
	// tried by hand (set IDENTITY_EIJARA_STUB_DELAY above IDENTITY_EIJARA_TIMEOUT).
	if s.Delay > 0 {
		t := time.NewTimer(s.Delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-t.C:
		}
	}
	// STUB: test hook. Numbers starting with "INVALID" are rejected.
	if strings.HasPrefix(strings.ToUpper(contractNumber), InvalidPrefix) {
		return Result{
			Status:          domain.VerificationRejected,
			RejectionReason: domain.ReasonContractInvalid,
		}, nil
	}
	// STUB: every other number is an active lease from today (Asia/Tashkent)
	// for one year. Location and area are left unset rather than invented.
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	today := domain.TashkentDate(now())
	return Result{
		Status: domain.VerificationVerified,
		Lease: &domain.Lease{
			ContractNumber: contractNumber,
			ValidFrom:      today,
			ValidUntil:     today.AddDate(1, 0, 0),
		},
	}, nil
}
