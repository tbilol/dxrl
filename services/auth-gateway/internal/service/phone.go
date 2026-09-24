package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/sms"
)

// Challenge is what RequestPhoneCode tells the app: which challenge to answer,
// how long the code lives and when another one may be asked for. The code
// itself is never returned.
type Challenge struct {
	ID                uuid.UUID
	ExpiresAt         time.Time
	ResendAvailableAt time.Time
}

// PhoneSetup is a verified phone number plus the password to set
// (PhonePasswordSetup in the proto).
type PhoneSetup struct {
	Phone       string
	ChallengeID string
	Code        string
	Password    string
}

// RequestPhoneCode creates an SMS code challenge for a number and a purpose.
// The purpose decides what the number must be: free for SIGN_UP and
// LINK_PHONE, registered for RESET_PASSWORD. A code only ever works for the
// purpose it was asked for.
func (s *Service) RequestPhoneCode(ctx context.Context, p auth.Principal, phone string,
	purpose authgatewayv1.PhoneCodePurpose,
) (Challenge, error) {
	if err := s.allow(ctx, phoneCodeKey(phone), "too many code requests for this number; retry later"); err != nil {
		return Challenge{}, err
	}
	now := s.clock()

	var (
		registered bool
		latest     domain.PhoneChallenge
		haveLatest bool
	)
	err := s.store.Run(ctx, func(q Queries) error {
		_, err := q.CredentialBySubject(ctx, domain.MethodPhonePassword, phone)
		switch {
		case err == nil:
			registered = true
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}
		latest, err = q.LatestChallenge(ctx, phone, purpose)
		switch {
		case err == nil:
			haveLatest = true
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}
		return nil
	})
	if err != nil {
		return Challenge{}, wrapStore("request phone code", err)
	}

	switch purpose {
	case domain.PurposeSignUp, domain.PurposeLinkPhone:
		if registered {
			return Challenge{}, phoneTaken()
		}
	case domain.PurposeResetPassword:
		if !registered {
			// The number is enumerable through sign-up anyway (that path must
			// answer PHONE_ALREADY_REGISTERED), so this stays honest instead
			// of pretending a code was sent.
			return Challenge{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND,
				"no account uses this phone number")
		}
	default:
		return Challenge{}, apperr.Validation(&commonv1.FieldViolation{
			Field: "purpose", Description: "value must be a defined enum value", Rule: "enum.defined_only",
		})
	}
	// The resend window: one code per number, purpose and minute.
	if haveLatest && now.Before(latest.ResendAvailableAt) {
		return Challenge{}, rateLimited("a code was already sent to this number; wait before asking for another",
			latest.ResendAvailableAt.Sub(now))
	}

	id, err := newID()
	if err != nil {
		return Challenge{}, err
	}
	code := s.cfg.DevPhoneCode // STUB: fixed development code when configured
	if code == "" {
		if code, err = sms.NewCode(s.cfg.PhoneCodeLength); err != nil {
			return Challenge{}, err
		}
	}
	challenge := domain.PhoneChallenge{
		ID:                id,
		PhoneNumber:       phone,
		Purpose:           purpose,
		CodeHash:          sms.HashCode(id, code),
		MaxAttempts:       s.cfg.PhoneCodeMaxAttempts,
		CreatedAt:         now,
		ExpiresAt:         now.Add(s.cfg.PhoneCodeTTL),
		ResendAvailableAt: now.Add(s.cfg.PhoneCodeResend),
	}
	if err := s.store.Run(ctx, func(q Queries) error {
		return q.InsertChallenge(ctx, challenge)
	}); err != nil {
		return Challenge{}, wrapStore("request phone code", err)
	}
	// STUB: sends nothing; see internal/sms. The code never goes to Kafka, to
	// the outbox, or into a log outside development.
	if err := s.sender.Send(ctx, phone, code, purpose); err != nil {
		s.log.ErrorContext(ctx, "could not deliver the phone code", "challenge_id", id, "error", err)
		e := apperr.New(commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE,
			"the SMS provider is unavailable", "dependency", "sms")
		e.Cause = err
		return Challenge{}, e
	}
	s.log.InfoContext(ctx, "phone code requested", "challenge_id", id, "purpose", purpose.String(),
		"request_id", p.RequestID)
	return Challenge{ID: id, ExpiresAt: challenge.ExpiresAt, ResendAvailableAt: challenge.ResendAvailableAt}, nil
}

// SignUpWithPhone creates a phone + password account from a verified code and
// signs it in. request_id is the caller's idempotency key and is passed
// straight to identity's CreateAccount, so a retry returns the same account.
func (s *Service) SignUpWithPhone(ctx context.Context, p auth.Principal, requestID string,
	setup PhoneSetup, client domain.ClientInfo,
) (SignInResult, error) {
	reqID, err := parseRequestID(requestID)
	if err != nil {
		return SignInResult{}, err
	}
	challengeID, err := s.verifyChallenge(ctx, setup, domain.PurposeSignUp)
	if err != nil {
		return SignInResult{}, err
	}

	var registered bool
	if err := s.store.Run(ctx, func(q Queries) error {
		_, err := q.CredentialBySubject(ctx, domain.MethodPhonePassword, setup.Phone)
		switch {
		case err == nil:
			registered = true
			return nil
		case errors.Is(err, domain.ErrNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		return SignInResult{}, wrapStore("sign up", err)
	}
	if registered {
		return SignInResult{}, phoneTaken()
	}

	hash, err := s.hasher.Hash(setup.Password)
	if err != nil {
		return SignInResult{}, apperr.Internal(err)
	}
	// The account is created before the challenge is spent: if the write below
	// fails, the (unusable) account is reused by a retry with the same
	// request_id rather than duplicated.
	acct, err := s.accounts.CreateAccount(ctx, p, reqID)
	if err != nil {
		return SignInResult{}, err
	}
	if !acct.Active() {
		return SignInResult{}, accountDisabled()
	}

	now := s.clock()
	credID, err := newID()
	if err != nil {
		return SignInResult{}, err
	}
	var (
		session domain.Session
		pair    TokenPair
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		consumed, err := q.ConsumeChallenge(ctx, challengeID, now)
		if err != nil {
			return err
		}
		if !consumed {
			return otpInvalid()
		}
		err = q.InsertCredential(ctx, domain.Credential{
			ID:           credID,
			UserID:       acct.ID,
			Method:       domain.MethodPhonePassword,
			Subject:      setup.Phone,
			PasswordHash: hash,
			DisplayHint:  domain.MaskPhone(setup.Phone),
			LinkedAt:     now,
			LastUsedAt:   &now,
		})
		switch {
		case errors.Is(err, domain.ErrSubjectTaken), errors.Is(err, domain.ErrMethodLinked):
			return phoneTaken()
		case err != nil:
			return err
		}
		session, pair, err = s.openSession(ctx, q, acct, domain.MethodPhonePassword, client, now)
		return err
	})
	if err != nil {
		return SignInResult{}, wrapStore("sign up", err)
	}
	s.log.InfoContext(ctx, "signed up with phone", "user_id", acct.ID, "session_id", session.ID,
		"request_id", p.RequestID)
	return SignInResult{UserID: acct.ID, SessionID: session.ID, Tokens: pair, IsNewAccount: true}, nil
}

// ResetPassword sets a new password from a verified code and ends every
// session of the account, so a stolen session cannot survive the reset.
func (s *Service) ResetPassword(ctx context.Context, p auth.Principal, setup PhoneSetup) error {
	challengeID, err := s.verifyChallenge(ctx, setup, domain.PurposeResetPassword)
	if err != nil {
		return err
	}

	var cred domain.Credential
	err = s.store.Run(ctx, func(q Queries) error {
		cred, err = q.CredentialBySubject(ctx, domain.MethodPhonePassword, setup.Phone)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND, "no account uses this phone number")
	case err != nil:
		return wrapStore("reset password", err)
	}

	hash, err := s.hasher.Hash(setup.Password)
	if err != nil {
		return apperr.Internal(err)
	}
	now := s.clock()
	var sessions int64
	err = s.store.InTx(ctx, func(q Queries) error {
		consumed, err := q.ConsumeChallenge(ctx, challengeID, now)
		if err != nil {
			return err
		}
		if !consumed {
			return otpInvalid()
		}
		if err := q.UpdatePasswordHash(ctx, cred.ID, hash); err != nil {
			return err
		}
		if sessions, err = q.RevokeUserSessions(ctx, cred.UserID, domain.RevokedPasswordReset, now); err != nil {
			return err
		}
		return s.notify(ctx, q, p, cred.UserID, events.TemplatePasswordChanged,
			map[string]string{"method": methodText(domain.MethodPhonePassword)}, now)
	})
	if err != nil {
		return wrapStore("reset password", err)
	}
	s.log.InfoContext(ctx, "password reset", "user_id", cred.UserID, "sessions_ended", sessions,
		"request_id", p.RequestID)
	return nil
}

// verifyChallenge checks a code against its challenge and returns the
// challenge id to spend. A wrong code is counted (and eventually locks the
// challenge out); the challenge is only marked consumed inside the caller's
// transaction, so the code survives a failure further down the flow only when
// that failure is the caller's own fault.
func (s *Service) verifyChallenge(ctx context.Context, setup PhoneSetup,
	purpose authgatewayv1.PhoneCodePurpose,
) (uuid.UUID, error) {
	id, err := parseChallengeID(setup.ChallengeID)
	if err != nil {
		return uuid.Nil, err
	}
	if !sms.ValidCode(setup.Code) {
		return uuid.Nil, otpInvalid()
	}
	now := s.clock()

	var challenge domain.PhoneChallenge
	err = s.store.Run(ctx, func(q Queries) error {
		challenge, err = q.GetChallenge(ctx, id)
		if err != nil {
			return err
		}
		// Wrong number, wrong purpose, expired, consumed or out of attempts:
		// all the same answer.
		if challenge.PhoneNumber != setup.Phone || challenge.Purpose != purpose || !challenge.Usable(now) {
			return otpInvalid()
		}
		if !sms.EqualCode(id, challenge.CodeHash, setup.Code) {
			if err := q.CountChallengeAttempt(ctx, id); err != nil {
				return fmt.Errorf("count phone code attempt: %w", err)
			}
			return otpInvalid()
		}
		return nil
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return uuid.Nil, otpInvalid()
	case err != nil:
		return uuid.Nil, wrapStore("verify phone code", err)
	}
	return id, nil
}
