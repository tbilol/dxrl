package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/events"
)

// LinkInput is a decoded LinkSignInMethodRequest: the method to add to the
// caller's account.
type LinkInput struct {
	Method authgatewayv1.SignInMethod
	// Token and Nonce are the Google or Apple credential.
	Token string
	Nonce string
	// Phone is the PHONE_PASSWORD setup (number, code, new password).
	Phone PhoneSetup
}

// ListSignInMethods lists the caller's linked methods.
func (s *Service) ListSignInMethods(ctx context.Context, p auth.Principal) ([]domain.Credential, error) {
	userID, err := auth.RequireUser(p)
	if err != nil {
		return nil, err
	}
	var creds []domain.Credential
	if err := s.store.Run(ctx, func(q Queries) error {
		creds, err = q.CredentialsByUser(ctx, userID)
		return err
	}); err != nil {
		return nil, wrapStore("list sign-in methods", err)
	}
	return creds, nil
}

// LinkSignInMethod adds a sign-in method to the caller's account. An account
// holds at most one credential per method, and a Google/Apple subject or a
// phone number belongs to at most one account.
func (s *Service) LinkSignInMethod(ctx context.Context, p auth.Principal, in LinkInput) ([]domain.Credential, error) {
	userID, err := auth.RequireUser(p)
	if err != nil {
		return nil, err
	}

	now := s.clock()
	cred := domain.Credential{UserID: userID, Method: in.Method, LinkedAt: now}
	challengeID := uuid.Nil

	switch in.Method {
	case domain.MethodGoogle, domain.MethodApple:
		ident, err := s.verifyProviderToken(ctx, in.Method, in.Token, in.Nonce)
		if err != nil {
			return nil, err
		}
		cred.Subject = ident.Subject
		cred.DisplayHint = domain.DisplayHint(in.Method, ident.Subject, ident.Email)
	case domain.MethodPhonePassword:
		if challengeID, err = s.verifyChallenge(ctx, in.Phone, domain.PurposeLinkPhone); err != nil {
			return nil, err
		}
		hash, err := s.hasher.Hash(in.Phone.Password)
		if err != nil {
			return nil, apperr.Internal(err)
		}
		cred.Subject, cred.PasswordHash = in.Phone.Phone, hash
		cred.DisplayHint = domain.MaskPhone(in.Phone.Phone)
	default:
		return nil, apperr.Validation(&commonv1.FieldViolation{
			Field: "method", Description: "exactly one method is required", Rule: "required",
		})
	}
	if cred.ID, err = newID(); err != nil {
		return nil, err
	}

	var creds []domain.Credential
	err = s.store.InTx(ctx, func(q Queries) error {
		if challengeID != uuid.Nil {
			consumed, err := q.ConsumeChallenge(ctx, challengeID, now)
			if err != nil {
				return err
			}
			if !consumed {
				return otpInvalid()
			}
		}
		switch err := q.InsertCredential(ctx, cred); {
		case errors.Is(err, domain.ErrMethodLinked):
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_PROVIDER_ALREADY_LINKED,
				"this account already has that sign-in method", "method", methodText(in.Method))
		case errors.Is(err, domain.ErrSubjectTaken):
			if in.Method == domain.MethodPhonePassword {
				return phoneTaken()
			}
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_PROVIDER_ALREADY_LINKED,
				"that provider account is already linked to another account", "method", methodText(in.Method))
		case err != nil:
			return err
		}
		if err := s.notify(ctx, q, p, userID, events.TemplateSignInMethodLinked,
			map[string]string{"method": methodText(in.Method)}, now); err != nil {
			return err
		}
		creds, err = q.CredentialsByUser(ctx, userID)
		return err
	})
	if err != nil {
		return nil, wrapStore("link sign-in method", err)
	}
	s.log.InfoContext(ctx, "sign-in method linked", "user_id", userID, "method", methodText(in.Method),
		"request_id", p.RequestID)
	return creds, nil
}

// UnlinkSignInMethod removes a method from the caller's account. The last
// remaining method cannot be removed: that would lock the account out.
//
// TODO(sessions): sessions opened with the removed method keep working until
// they expire. Whether unlinking should end them is a product decision the
// proto does not settle.
func (s *Service) UnlinkSignInMethod(ctx context.Context, p auth.Principal,
	method authgatewayv1.SignInMethod,
) ([]domain.Credential, error) {
	userID, err := auth.RequireUser(p)
	if err != nil {
		return nil, err
	}
	now := s.clock()

	var creds []domain.Credential
	err = s.store.InTx(ctx, func(q Queries) error {
		current, err := q.CredentialsByUser(ctx, userID)
		if err != nil {
			return err
		}
		found := false
		for _, c := range current {
			if c.Method == method {
				found = true
				break
			}
		}
		switch {
		case !found:
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_NOT_FOUND,
				"this account does not have that sign-in method", "method", methodText(method))
		case len(current) == 1:
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_LAST_SIGN_IN_METHOD,
				"the last sign-in method cannot be removed", "method", methodText(method))
		}
		if _, err := q.DeleteCredential(ctx, userID, method); err != nil {
			return err
		}
		if err := s.notify(ctx, q, p, userID, events.TemplateSignInMethodUnlinked,
			map[string]string{"method": methodText(method)}, now); err != nil {
			return err
		}
		creds, err = q.CredentialsByUser(ctx, userID)
		return err
	})
	if err != nil {
		return nil, wrapStore("unlink sign-in method", err)
	}
	s.log.InfoContext(ctx, "sign-in method unlinked", "user_id", userID, "method", methodText(method),
		"request_id", p.RequestID)
	return creds, nil
}

// notify writes a SECURITY notification request into the outbox, in the same
// transaction as the change it reports. params carry the method name at most:
// no codes, tokens, hashes or phone numbers.
func (s *Service) notify(ctx context.Context, q Queries, p auth.Principal, userID uuid.UUID,
	template string, params map[string]string, at time.Time,
) error {
	msg, err := events.NewNotificationRequest(events.Notification{
		RecipientID: userID,
		Category:    eventsv1.NotificationCategory_NOTIFICATION_CATEGORY_SECURITY,
		TemplateKey: template,
		Params:      params,
		Subject: &commonv1.ResourceRef{
			Type: commonv1.ResourceType_RESOURCE_TYPE_USER,
			Id:   userID.String(),
		},
		OccurredAt:    at,
		CorrelationID: p.RequestID,
		Traceparent:   p.Traceparent,
		Tracestate:    p.Tracestate,
		Actor:         p.Actor(),
	})
	if err != nil {
		return err
	}
	return q.EnqueueOutbox(ctx, msg)
}
