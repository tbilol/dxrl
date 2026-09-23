package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/identity/internal/apperr"
	"github.com/tbilol/dxrl/services/identity/internal/auth"
	"github.com/tbilol/dxrl/services/identity/internal/domain"
	"github.com/tbilol/dxrl/services/identity/internal/eijara"
	"github.com/tbilol/dxrl/services/identity/internal/events"
)

// errNotPending: the verification was decided by someone else meanwhile.
var errNotPending = errors.New("verification is no longer pending")

// SubmitFarmerVerification verifies the caller as a farmer through e-ijara.
//
// Order: account must exist and be ACTIVE; a retried request id returns the
// original verification; a verified farmer gets ALREADY_EXISTS; then the rate
// limit, the contract number format and the "claimed by another account"
// check; then the verification is stored PENDING and e-ijara is asked inline
// with a short timeout. Its answer is applied in one transaction (status,
// ROLE_FARMER, outbox). Without an answer the verification is returned
// PENDING and the worker retries it.
func (s *Service) SubmitFarmerVerification(ctx context.Context, p auth.Principal, requestID, contractNumber string) (domain.Verification, error) {
	userID, err := auth.RequireUser(p)
	if err != nil {
		return domain.Verification{}, err
	}
	reqID, err := parseRequestID(requestID)
	if err != nil {
		return domain.Verification{}, err
	}

	var (
		acct  domain.Account
		prior domain.Verification
		retry bool
	)
	err = s.store.Run(ctx, func(q Queries) error {
		var err error
		if acct, err = q.GetAccount(ctx, userID); err != nil {
			return err
		}
		prior, err = q.VerificationByRequest(ctx, userID, reqID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		retry = err == nil
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.Verification{}, notFound("account", userID)
	case err != nil:
		return domain.Verification{}, fmt.Errorf("submit farmer verification: %w", err)
	}
	if acct.Status != domain.AccountActive {
		return domain.Verification{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_ACCOUNT_DISABLED,
			"the account is not active", "status", acct.Status.String())
	}
	if retry {
		return prior, nil // idempotent retry: the original verification, as it stands now
	}
	if acct.HasRole(domain.RoleFarmer) {
		return domain.Verification{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_ALREADY_EXISTS,
			"the account is already a verified farmer")
	}
	if err := s.checkRateLimit(ctx, userID); err != nil {
		return domain.Verification{}, err
	}
	contract, ok := eijara.NormalizeContractNumber(contractNumber)
	if !ok {
		return domain.Verification{}, apperr.Validation(&commonv1.FieldViolation{
			Field:       "lease_contract_number",
			Description: "must be 3-64 letters, digits, '/' or '-', starting with a letter or digit",
			Rule:        "identity.lease_contract_number.format",
		})
	}

	id, err := newID()
	if err != nil {
		return domain.Verification{}, err
	}
	now := s.clock()
	v := domain.Verification{
		ID:             id,
		UserID:         userID,
		RequestID:      reqID,
		ContractNumber: contract,
		Status:         domain.VerificationPending,
		SubmittedAt:    now,
		// Keeps the worker away while the inline e-ijara call is in flight.
		NextAttemptAt: now.Add(max(s.cfg.RetryBackoff, 2*s.cfg.EIjaraTimeout)),
	}
	var claimed, inserted bool
	err = s.store.InTx(ctx, func(q Queries) error {
		var err error
		if claimed, err = q.ContractClaimed(ctx, contract, userID); err != nil || claimed {
			return err
		}
		v, inserted, err = q.InsertVerification(ctx, v)
		return err
	})
	switch {
	case err != nil:
		return domain.Verification{}, fmt.Errorf("store farmer verification: %w", err)
	case claimed:
		return domain.Verification{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED,
			"the lease contract is already bound to another account")
	case !inserted:
		return v, nil // a concurrent request with the same request id won
	}
	s.log.InfoContext(ctx, "farmer verification submitted", "verification_id", v.ID, "user_id", userID)

	res, err := s.check(ctx, contract)
	if err != nil {
		s.log.WarnContext(ctx, "e-ijara gave no answer; verification stays PENDING for the worker",
			"verification_id", v.ID, "error", err)
		return v, nil
	}
	return s.decide(ctx, v, res, trigger{
		actor:         p.Actor(),
		correlationID: p.RequestID,
		traceparent:   p.Traceparent,
		tracestate:    p.Tracestate,
	})
}

// GetFarmerVerification returns a user's latest verification. Empty userID
// means the caller.
func (s *Service) GetFarmerVerification(ctx context.Context, p auth.Principal, userID string) (domain.Verification, error) {
	target, err := auth.ReadTarget(p, userID)
	if err != nil {
		return domain.Verification{}, err
	}
	var v domain.Verification
	err = s.store.Run(ctx, func(q Queries) error {
		v, err = q.LatestVerification(ctx, target)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.Verification{}, notFound("farmer verification", target)
	case err != nil:
		return domain.Verification{}, fmt.Errorf("get farmer verification: %w", err)
	}
	return v, nil
}

// checkRateLimit enforces the per-user submission limit. It fails open: when
// Redis is unavailable the submission is allowed and a warning logged.
func (s *Service) checkRateLimit(ctx context.Context, userID uuid.UUID) error {
	if s.limiter == nil {
		return nil
	}
	allowed, retryAfter, err := s.limiter.Allow(ctx, "farmer-verification:"+userID.String())
	if err != nil {
		s.log.WarnContext(ctx, "rate limiter unavailable; allowing the submission (fail open)", "error", err)
		return nil
	}
	if allowed {
		return nil
	}
	secs := max(int64(math.Ceil(retryAfter.Seconds())), 1)
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_RATE_LIMITED,
		"too many farmer verification submissions; retry later",
		"retry_after_seconds", strconv.FormatInt(secs, 10))
}

// check asks e-ijara with the configured timeout and normalizes the answer.
func (s *Service) check(ctx context.Context, contract string) (eijara.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.EIjaraTimeout)
	defer cancel()
	res, err := s.checker.Check(ctx, contract)
	if err != nil {
		return eijara.Result{}, fmt.Errorf("e-ijara: %w", err)
	}
	switch res.Status {
	case domain.VerificationVerified:
		if res.Lease == nil {
			return eijara.Result{}, errors.New("e-ijara: VERIFIED without a lease")
		}
		if res.Lease.ValidUntil.Before(domain.TashkentDate(s.clock())) {
			// The contract exists, but the lease has already ended.
			return eijara.Result{Status: domain.VerificationRejected, RejectionReason: domain.ReasonContractInvalid}, nil
		}
		res.RejectionReason = commonv1.ErrorReason_ERROR_REASON_UNSPECIFIED
	case domain.VerificationRejected:
		if res.RejectionReason != domain.ReasonContractClaimed {
			res.RejectionReason = domain.ReasonContractInvalid
		}
		res.Lease = nil
	default:
		return eijara.Result{}, fmt.Errorf("e-ijara: unexpected status %s", res.Status)
	}
	return res, nil
}

// trigger is who caused a decision, for the event metadata.
type trigger struct {
	actor         *commonv1.Actor
	correlationID string
	traceparent   string
	tracestate    string
}

// workerTrigger is identity acting on its own, with a fresh correlation id.
func workerTrigger() trigger {
	id, err := newID()
	if err != nil {
		id = uuid.New()
	}
	return trigger{actor: auth.ServiceActor(), correlationID: id.String()}
}

// decide applies e-ijara's answer to a PENDING verification and returns the
// result. If the partial unique index says another account's verification of
// the same contract was VERIFIED meanwhile, the verification is REJECTED with
// LEASE_CONTRACT_ALREADY_CLAIMED instead.
func (s *Service) decide(ctx context.Context, v domain.Verification, res eijara.Result, tr trigger) (domain.Verification, error) {
	// A decided answer must not be lost because the client went away.
	ctx = context.WithoutCancel(ctx)
	d := domain.Decision{
		VerificationID:  v.ID,
		Status:          res.Status,
		RejectionReason: res.RejectionReason,
		Lease:           res.Lease,
		DecidedAt:       s.clock(),
	}
	apply := func(q Queries) error { return s.applyDecision(ctx, q, v, d, tr) }
	err := s.store.InTx(ctx, apply)
	if errors.Is(err, domain.ErrContractClaimed) {
		d = domain.Decision{
			VerificationID:  v.ID,
			Status:          domain.VerificationRejected,
			RejectionReason: domain.ReasonContractClaimed,
			DecidedAt:       d.DecidedAt,
		}
		err = s.store.InTx(ctx, apply)
	}
	switch {
	case errors.Is(err, errNotPending):
		var cur domain.Verification
		err = s.store.Run(ctx, func(q Queries) error {
			var err error
			cur, err = q.GetVerification(ctx, v.ID)
			return err
		})
		return cur, err
	case err != nil:
		return domain.Verification{}, fmt.Errorf("decide farmer verification %s: %w", v.ID, err)
	}
	attrs := []any{"verification_id", v.ID, "user_id", v.UserID, "status", d.Status.String()}
	if d.Status == domain.VerificationRejected {
		attrs = append(attrs, "rejection_reason", d.RejectionReason.String())
	}
	s.log.InfoContext(ctx, "farmer verification decided", attrs...)
	return v.Apply(d), nil
}

// applyDecision runs inside one transaction: verification status, ROLE_FARMER
// and the notification request.
func (s *Service) applyDecision(ctx context.Context, q Queries, v domain.Verification, d domain.Decision, tr trigger) error {
	if err := q.LockAccount(ctx, v.UserID); err != nil {
		return err
	}
	ok, err := q.DecideVerification(ctx, d)
	if err != nil {
		return err
	}
	if !ok {
		return errNotPending
	}
	// TODO(events): publish AccountEvent (farmer verification decided) on
	// agrogram.identity.account.v1 once events/v1/account.proto exists.
	template := events.TemplateFarmerVerificationRejected
	params := map[string]string{"lease_contract_number": v.ContractNumber}
	if d.Status == domain.VerificationVerified {
		template = events.TemplateFarmerVerified
		granted, err := q.GrantRole(ctx, v.UserID, domain.RoleFarmer, d.DecidedAt)
		if err != nil {
			return err
		}
		if granted {
			// TODO(events): publish AccountEvent (roles changed, ROLE_FARMER
			// granted) on agrogram.identity.account.v1 once events/v1/account.proto exists.
			s.log.InfoContext(ctx, "granting ROLE_FARMER", "user_id", v.UserID, "verification_id", v.ID)
		}
	} else {
		params["rejection_reason"] = d.RejectionReason.String()
	}
	return s.enqueueNotification(ctx, q, v, template, params, d.DecidedAt, tr)
}

func (s *Service) enqueueNotification(ctx context.Context, q Queries, v domain.Verification, template string,
	params map[string]string, at time.Time, tr trigger,
) error {
	msg, err := events.NewNotificationRequest(events.Notification{
		RecipientID: v.UserID,
		Category:    eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_ACCOUNT,
		TemplateKey: template,
		Params:      params,
		Subject: &commonv1.ResourceRef{
			Type: commonv1.ResourceType_RESOURCE_TYPE_FARMER_VERIFICATION,
			Id:   v.ID.String(),
		},
		OccurredAt:    at,
		CorrelationID: tr.correlationID,
		Traceparent:   tr.traceparent,
		Tracestate:    tr.tracestate,
		Actor:         tr.actor,
	})
	if err != nil {
		return err
	}
	return q.EnqueueOutbox(ctx, msg)
}
