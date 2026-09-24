package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/apperr"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/identityclient"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/password"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/provider"
)

// SignInInput is a decoded SignInRequest: one credential plus the device.
type SignInInput struct {
	Method authgatewayv1.SignInMethod
	// Token is the Google ID token or the Apple identity token.
	Token string
	Nonce string
	// Phone and Password are the PHONE_PASSWORD credential.
	Phone    string
	Password string
	Client   domain.ClientInfo
}

// SignIn opens a session for a Google account, an Apple account or a phone
// number and password. The first Google or Apple sign-in creates the account
// in identity (is_new_account); phone accounts come from SignUpWithPhone.
func (s *Service) SignIn(ctx context.Context, p auth.Principal, in SignInInput) (SignInResult, error) {
	switch in.Method {
	case domain.MethodGoogle, domain.MethodApple:
		return s.signInWithProvider(ctx, p, in)
	case domain.MethodPhonePassword:
		return s.signInWithPassword(ctx, p, in)
	default:
		return SignInResult{}, apperr.Validation(&commonv1.FieldViolation{
			Field: "credential", Description: "exactly one credential is required", Rule: "required",
		})
	}
}

// signInWithProvider signs in with Google or Apple. A subject that has no
// credential yet is a first sign-in: identity creates the account with a
// request id derived from the sign-in identity, so a retry (or a parallel
// second attempt) reuses it instead of creating a second account.
func (s *Service) signInWithProvider(ctx context.Context, p auth.Principal, in SignInInput) (SignInResult, error) {
	ident, err := s.verifyProviderToken(ctx, in.Method, in.Token, in.Nonce)
	if err != nil {
		return SignInResult{}, err
	}
	key := signInKey(in.Method, ident.Subject)
	if err := s.exceeded(ctx, key, "too many failed sign-in attempts; retry later"); err != nil {
		return SignInResult{}, err
	}

	var cred domain.Credential
	err = s.store.Run(ctx, func(q Queries) error {
		cred, err = q.CredentialBySubject(ctx, in.Method, ident.Subject)
		return err
	})
	linked := err == nil
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return SignInResult{}, wrapStore("sign in", err)
	}

	var acct identityclient.Account
	if linked {
		if acct, err = s.accounts.GetAccount(ctx, p, cred.UserID); err != nil {
			return SignInResult{}, err
		}
	} else if acct, err = s.accounts.CreateAccount(ctx, p, accountRequestID(in.Method, ident.Subject)); err != nil {
		return SignInResult{}, err
	}
	if !acct.Active() {
		return SignInResult{}, accountDisabled()
	}

	now := s.clock()
	isNew := !linked
	var (
		session domain.Session
		pair    TokenPair
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		if !linked {
			id, err := newID()
			if err != nil {
				return err
			}
			cred = domain.Credential{
				ID:          id,
				UserID:      acct.ID,
				Method:      in.Method,
				Subject:     ident.Subject,
				DisplayHint: domain.DisplayHint(in.Method, ident.Subject, ident.Email),
				LinkedAt:    now,
			}
			switch err := q.InsertCredential(ctx, cred); {
			case errors.Is(err, domain.ErrSubjectTaken), errors.Is(err, domain.ErrMethodLinked):
				// A parallel first sign-in stored it first; both derived the
				// same account, so continue with the stored credential.
				if cred, err = q.CredentialBySubject(ctx, in.Method, ident.Subject); err != nil {
					return err
				}
				if cred.UserID != acct.ID {
					return apperr.New(commonv1.ErrorReason_ERROR_REASON_CONFLICT,
						"the sign-in identity belongs to another account")
				}
				isNew = false
			case err != nil:
				return err
			}
		}
		if err := q.TouchCredential(ctx, cred.ID, now); err != nil {
			return err
		}
		session, pair, err = s.openSession(ctx, q, acct, in.Method, in.Client, now)
		return err
	})
	if err != nil {
		return SignInResult{}, wrapStore("sign in", err)
	}
	s.log.InfoContext(ctx, "signed in", "user_id", acct.ID, "session_id", session.ID,
		"method", methodText(in.Method), "new_account", isNew, "request_id", p.RequestID)
	return SignInResult{UserID: acct.ID, SessionID: session.ID, Tokens: pair, IsNewAccount: isNew}, nil
}

// signInWithPassword signs in with a phone number and password. Every failure
// answers INVALID_CREDENTIALS and counts against the per-number limit, so the
// response never says whether the number exists.
func (s *Service) signInWithPassword(ctx context.Context, p auth.Principal, in SignInInput) (SignInResult, error) {
	key := signInKey(domain.MethodPhonePassword, in.Phone)
	if err := s.exceeded(ctx, key, "too many failed sign-in attempts; retry later"); err != nil {
		return SignInResult{}, err
	}

	var cred domain.Credential
	err := s.store.Run(ctx, func(q Queries) error {
		var err error
		cred, err = q.CredentialBySubject(ctx, domain.MethodPhonePassword, in.Phone)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// The number has no account. Verifying nothing is measurably faster
		// than verifying a hash, but RequestPhoneCode already tells anyone who
		// asks whether a number is registered, so nothing is leaked here.
		s.countFailure(ctx, key)
		return SignInResult{}, invalidCredentials()
	case err != nil:
		return SignInResult{}, wrapStore("sign in", err)
	}
	switch err := password.Verify(cred.PasswordHash, in.Password); {
	case errors.Is(err, password.ErrMismatch):
		s.countFailure(ctx, key)
		s.log.InfoContext(ctx, "sign-in refused: wrong password", "user_id", cred.UserID,
			"request_id", p.RequestID)
		return SignInResult{}, invalidCredentials()
	case err != nil:
		return SignInResult{}, apperr.Internal(fmt.Errorf("verify password: %w", err))
	}

	acct, err := s.accounts.GetAccount(ctx, p, cred.UserID)
	if err != nil {
		return SignInResult{}, err
	}
	if !acct.Active() {
		return SignInResult{}, accountDisabled()
	}

	now := s.clock()
	var (
		session domain.Session
		pair    TokenPair
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		if err := q.TouchCredential(ctx, cred.ID, now); err != nil {
			return err
		}
		session, pair, err = s.openSession(ctx, q, acct, domain.MethodPhonePassword, in.Client, now)
		return err
	})
	if err != nil {
		return SignInResult{}, wrapStore("sign in", err)
	}
	s.log.InfoContext(ctx, "signed in", "user_id", acct.ID, "session_id", session.ID,
		"method", methodText(domain.MethodPhonePassword), "new_account", false, "request_id", p.RequestID)
	return SignInResult{UserID: acct.ID, SessionID: session.ID, Tokens: pair}, nil
}

// verifyProviderToken checks a Google or Apple credential with a timeout, so
// a slow provider can never hold a sign-in open. Any rejection is
// INVALID_CREDENTIALS: the caller learns nothing about why.
func (s *Service) verifyProviderToken(ctx context.Context, method authgatewayv1.SignInMethod,
	idToken, nonce string,
) (provider.Identity, error) {
	verifier := s.google
	if method == domain.MethodApple {
		verifier = s.apple
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ProviderTimeout)
	defer cancel()
	ident, err := verifier.Verify(ctx, idToken, nonce)
	switch {
	case errors.Is(err, provider.ErrInvalidToken):
		s.log.InfoContext(ctx, "sign-in refused: provider token rejected",
			"method", methodText(method), "error", err)
		return provider.Identity{}, invalidCredentials()
	case err != nil:
		// The provider itself failed (network, JWKS): that is a dependency
		// problem, not a bad credential.
		e := apperr.New(commonv1.ErrorReason_ERROR_REASON_DEPENDENCY_UNAVAILABLE,
			"the sign-in provider is unavailable", "dependency", methodText(method))
		e.Cause = err
		return provider.Identity{}, e
	case ident.Subject == "":
		return provider.Identity{}, invalidCredentials()
	}
	return ident, nil
}

// accountRequestID is identity's CreateAccount idempotency key, derived from
// the sign-in identity (UUIDv5 over "GOOGLE:<sub>").
func accountRequestID(method authgatewayv1.SignInMethod, subject string) uuid.UUID {
	return uuid.NewSHA1(accountNamespace, []byte(methodText(method)+":"+subject))
}
