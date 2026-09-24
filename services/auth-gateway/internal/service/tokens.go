package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/auth"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/identityclient"
	"github.com/tbilol/dxrl/services/auth-gateway/internal/token"
)

// errTokenReuse rolls the rotation transaction back when the refresh token
// was already spent by a parallel refresh. It never leaves the service.
var errTokenReuse = errors.New("refresh token reuse")

// TokenPair is a signed access token and the opaque refresh token that
// renews it. The refresh token exists in plaintext only here and in the
// response; the database holds its SHA-256 digest.
type TokenPair struct {
	AccessToken           string
	AccessTokenExpiresAt  time.Time
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
}

// SignInResult is the outcome of a sign-in or a sign-up.
type SignInResult struct {
	UserID       uuid.UUID
	SessionID    uuid.UUID
	Tokens       TokenPair
	IsNewAccount bool
}

// Verified is what VerifyToken tells the api-gateway.
type Verified struct {
	UserID    uuid.UUID
	SessionID uuid.UUID
	Roles     []commonv1.Role
	Premium   bool
	ExpiresAt time.Time
}

// openSession stores a new session with its first refresh token and signs the
// first access token. It runs inside the caller's transaction, so a sign-in
// either produces a complete session or nothing.
func (s *Service) openSession(ctx context.Context, q Queries, acct identityclient.Account,
	method authgatewayv1.SignInMethod, client domain.ClientInfo, now time.Time,
) (domain.Session, TokenPair, error) {
	id, err := newID()
	if err != nil {
		return domain.Session{}, TokenPair{}, err
	}
	session := domain.Session{
		ID:         id,
		UserID:     acct.ID,
		Method:     method,
		Client:     client,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.cfg.RefreshTokenTTL),
	}
	if err := q.InsertSession(ctx, session); err != nil {
		return domain.Session{}, TokenPair{}, fmt.Errorf("insert session: %w", err)
	}
	pair, err := s.issueTokens(ctx, q, acct, session.ID, now)
	if err != nil {
		return domain.Session{}, TokenPair{}, err
	}
	return session, pair, nil
}

// issueTokens signs an access token and stores a fresh refresh token for the
// session.
func (s *Service) issueTokens(ctx context.Context, q Queries, acct identityclient.Account,
	sessionID uuid.UUID, now time.Time,
) (TokenPair, error) {
	access, claims, err := s.issuer.Issue(token.Claims{
		UserID:    acct.ID,
		SessionID: sessionID,
		Roles:     acct.RoleNames(),
		Premium:   acct.IsPremium,
		IssuedAt:  now,
	})
	if err != nil {
		return TokenPair{}, err
	}
	secret, rt, err := s.newRefreshToken(sessionID, now)
	if err != nil {
		return TokenPair{}, err
	}
	if err := q.InsertRefreshToken(ctx, rt); err != nil {
		return TokenPair{}, fmt.Errorf("insert refresh token: %w", err)
	}
	return TokenPair{
		AccessToken:           access,
		AccessTokenExpiresAt:  claims.ExpiresAt,
		RefreshToken:          secret,
		RefreshTokenExpiresAt: rt.ExpiresAt,
	}, nil
}

func (s *Service) newRefreshToken(sessionID uuid.UUID, now time.Time) (string, domain.RefreshToken, error) {
	id, err := newID()
	if err != nil {
		return "", domain.RefreshToken{}, err
	}
	secret, hash, err := token.NewRefreshToken()
	if err != nil {
		return "", domain.RefreshToken{}, err
	}
	return secret, domain.RefreshToken{
		ID:        id,
		SessionID: sessionID,
		Hash:      hash,
		IssuedAt:  now,
		ExpiresAt: now.Add(s.cfg.RefreshTokenTTL),
	}, nil
}

// RefreshToken exchanges a refresh token for a new pair. The old token stops
// working at once (rotation), and presenting it again revokes the whole
// session: a stolen token can be used once at most, and the theft is visible.
// Roles and premium are re-read from identity, so the new access token carries
// current values.
func (s *Service) RefreshToken(ctx context.Context, p auth.Principal, secret string) (TokenPair, error) {
	now := s.clock()
	hash := token.HashRefreshToken(secret)

	var (
		rt      domain.RefreshToken
		session domain.Session
	)
	err := s.store.Run(ctx, func(q Queries) error {
		var err error
		if rt, err = q.RefreshTokenByHash(ctx, hash); err != nil {
			return err
		}
		session, err = q.GetSession(ctx, rt.SessionID)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return TokenPair{}, tokenInvalid()
	case err != nil:
		return TokenPair{}, wrapStore("refresh token", err)
	}

	// Reuse of an already rotated token: the session is compromised.
	if rt.UsedAt != nil {
		s.revokeForReuse(ctx, session, now)
		return TokenPair{}, tokenRevoked()
	}
	switch {
	case session.RevokedAt != nil:
		return TokenPair{}, tokenRevoked()
	case !now.Before(rt.ExpiresAt), !now.Before(session.ExpiresAt):
		return TokenPair{}, tokenExpired()
	}

	acct, err := s.accounts.GetAccount(ctx, p, session.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	if !acct.Active() {
		return TokenPair{}, accountDisabled()
	}

	next, rotated, err := s.newRefreshToken(session.ID, now)
	if err != nil {
		return TokenPair{}, err
	}
	access, claims, err := s.issuer.Issue(token.Claims{
		UserID:    acct.ID,
		SessionID: session.ID,
		Roles:     acct.RoleNames(),
		Premium:   acct.IsPremium,
		IssuedAt:  now,
	})
	if err != nil {
		return TokenPair{}, err
	}

	// The new token is stored before the old one points at it (refresh_tokens
	// .replaced_by is a foreign key). A lost race rolls the whole transaction
	// back, so the token that was never handed out disappears with it.
	err = s.store.InTx(ctx, func(q Queries) error {
		if err := q.InsertRefreshToken(ctx, rotated); err != nil {
			return err
		}
		used, err := q.UseRefreshToken(ctx, rt.ID, rotated.ID, now)
		if err != nil {
			return err
		}
		if !used { // a concurrent refresh won: treat it as reuse
			return errTokenReuse
		}
		return q.TouchSession(ctx, session.ID, now)
	})
	switch {
	case errors.Is(err, errTokenReuse):
		s.revokeForReuse(ctx, session, now)
		return TokenPair{}, tokenRevoked()
	case err != nil:
		return TokenPair{}, wrapStore("rotate refresh token", err)
	}
	s.log.InfoContext(ctx, "tokens refreshed", "user_id", session.UserID, "session_id", session.ID,
		"request_id", p.RequestID)
	return TokenPair{
		AccessToken:           access,
		AccessTokenExpiresAt:  claims.ExpiresAt,
		RefreshToken:          next,
		RefreshTokenExpiresAt: rotated.ExpiresAt,
	}, nil
}

// revokeForReuse ends a session whose refresh token was presented twice. The
// error is logged, not returned: the caller is refused either way.
//
// TODO(events): a "refresh token reused" security event belongs on a session
// stream, but events/v1 has no such schema; only NotificationRequested exists.
func (s *Service) revokeForReuse(ctx context.Context, session domain.Session, now time.Time) {
	if session.RevokedAt != nil {
		return
	}
	err := s.store.InTx(ctx, func(q Queries) error {
		_, err := q.RevokeSession(ctx, session.ID, session.UserID, domain.RevokedTokenReuse, now)
		return err
	})
	if err != nil {
		s.log.ErrorContext(ctx, "could not revoke a session after refresh token reuse",
			"session_id", session.ID, "user_id", session.UserID, "error", err)
		return
	}
	s.log.WarnContext(ctx, "refresh token reused; session revoked",
		"session_id", session.ID, "user_id", session.UserID)
}

// SignOut ends the caller's current session or every session of the account.
// Access tokens already issued keep working until they expire (minutes);
// their refresh tokens stop at once.
func (s *Service) SignOut(ctx context.Context, p auth.Principal, scope authgatewayv1.SignOutScope) error {
	now := s.clock()
	if scope == authgatewayv1.SignOutScope_SIGN_OUT_SCOPE_ALL_SESSIONS {
		userID, err := auth.RequireUser(p)
		if err != nil {
			return err
		}
		var n int64
		if err := s.store.InTx(ctx, func(q Queries) error {
			n, err = q.RevokeUserSessions(ctx, userID, domain.RevokedSignOutAll, now)
			return err
		}); err != nil {
			return wrapStore("sign out", err)
		}
		s.log.InfoContext(ctx, "signed out of every session", "user_id", userID, "sessions", n,
			"request_id", p.RequestID)
		return nil
	}

	userID, sessionID, err := auth.RequireSession(p)
	if err != nil {
		return err
	}
	var revoked bool
	if err := s.store.InTx(ctx, func(q Queries) error {
		revoked, err = q.RevokeSession(ctx, sessionID, userID, domain.RevokedSignOut, now)
		return err
	}); err != nil {
		return wrapStore("sign out", err)
	}
	// An already-revoked or unknown session is not an error: signing out twice
	// leaves the caller signed out either way.
	s.log.InfoContext(ctx, "signed out", "user_id", userID, "session_id", sessionID,
		"revoked", revoked, "request_id", p.RequestID)
	return nil
}

// VerifyToken checks an access token for the api-gateway: signature, issuer,
// audience and expiry locally, then that the session is still live. Roles and
// premium come from the token (a snapshot from when it was issued), which is
// what the `x-agrogram-*` headers are defined to carry.
func (s *Service) VerifyToken(ctx context.Context, p auth.Principal, accessToken string) (Verified, error) {
	if err := auth.RequireCaller(p, s.cfg.VerifyTokenCaller); err != nil {
		return Verified{}, err
	}
	claims, err := s.issuer.Verify(accessToken)
	switch {
	case errors.Is(err, token.ErrExpired):
		return Verified{}, tokenExpired()
	case err != nil:
		s.log.DebugContext(ctx, "access token rejected", "error", err, "request_id", p.RequestID)
		return Verified{}, tokenInvalid()
	}

	var session domain.Session
	err = s.store.Run(ctx, func(q Queries) error {
		session, err = q.GetSession(ctx, claims.SessionID)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return Verified{}, tokenInvalid()
	case err != nil:
		return Verified{}, wrapStore("verify token", err)
	case session.UserID != claims.UserID:
		return Verified{}, tokenInvalid()
	case session.RevokedAt != nil:
		return Verified{}, tokenRevoked()
	case !s.clock().Before(session.ExpiresAt):
		return Verified{}, tokenExpired()
	}
	return Verified{
		UserID:    claims.UserID,
		SessionID: claims.SessionID,
		Roles:     rolesFromNames(claims.Roles),
		Premium:   claims.Premium,
		ExpiresAt: claims.ExpiresAt,
	}, nil
}

// rolesFromNames turns `roles` claim names ("RESELLER") back into
// common.v1.Role values, ignoring names this build does not know.
func rolesFromNames(names []string) []commonv1.Role {
	roles := make([]commonv1.Role, 0, len(names))
	for _, n := range names {
		if v, ok := commonv1.Role_value["ROLE_"+n]; ok && v != 0 {
			roles = append(roles, commonv1.Role(v))
		}
	}
	return roles
}
