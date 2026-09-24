package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
)

const sessionSelect = `
SELECT id, user_id, method, platform, device_id, device_name, app_version,
       created_at, last_seen_at, expires_at, revoked_at, coalesce(revoked_reason, '')
FROM sessions`

func scanSession(row pgx.Row) (domain.Session, error) {
	var (
		s                                domain.Session
		method, platform                 string
		createdAt, lastSeenAt, expiresAt time.Time
		revokedAt                        *time.Time
	)
	err := row.Scan(&s.ID, &s.UserID, &method, &platform, &s.Client.DeviceID, &s.Client.DeviceName,
		&s.Client.AppVersion, &createdAt, &lastSeenAt, &expiresAt, &revokedAt, &s.RevokedReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Session{}, err
	}
	if s.Method, err = methods.value(method); err != nil {
		return domain.Session{}, err
	}
	if s.Client.Platform, err = platforms.value(platform); err != nil {
		return domain.Session{}, err
	}
	s.CreatedAt, s.LastSeenAt, s.ExpiresAt = utc(createdAt), utc(lastSeenAt), utc(expiresAt)
	s.RevokedAt = utcPtr(revokedAt)
	return s, nil
}

func (q *queries) InsertSession(ctx context.Context, s domain.Session) error {
	method, err := methods.text(s.Method)
	if err != nil {
		return err
	}
	platform, err := platforms.text(s.Client.Platform)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `
INSERT INTO sessions (id, user_id, method, platform, device_id, device_name, app_version,
                      created_at, last_seen_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		s.ID, s.UserID, method, platform, s.Client.DeviceID, s.Client.DeviceName, s.Client.AppVersion,
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

func (q *queries) GetSession(ctx context.Context, id uuid.UUID) (domain.Session, error) {
	return scanSession(q.db.QueryRow(ctx, sessionSelect+` WHERE id = $1`, id))
}

func (q *queries) TouchSession(ctx context.Context, id uuid.UUID, now time.Time) error {
	if _, err := q.db.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, id, now); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// RevokeSession only ever ends a live session of that user, so one user can
// never sign another one out.
func (q *queries) RevokeSession(ctx context.Context, id, userID uuid.UUID, reason string, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
UPDATE sessions SET revoked_at = $3, revoked_reason = $4
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID, now, reason)
	if err != nil {
		return false, fmt.Errorf("revoke session: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *queries) RevokeUserSessions(ctx context.Context, userID uuid.UUID, reason string, now time.Time) (int64, error) {
	tag, err := q.db.Exec(ctx, `
UPDATE sessions SET revoked_at = $2, revoked_reason = $3
WHERE user_id = $1 AND revoked_at IS NULL`, userID, now, reason)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (q *queries) InsertRefreshToken(ctx context.Context, rt domain.RefreshToken) error {
	_, err := q.db.Exec(ctx, `
INSERT INTO refresh_tokens (id, session_id, token_hash, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5)`, rt.ID, rt.SessionID, rt.Hash, rt.IssuedAt, rt.ExpiresAt)
	if err != nil {
		return fmt.Errorf("insert refresh token: %w", err)
	}
	return nil
}

func (q *queries) RefreshTokenByHash(ctx context.Context, hash []byte) (domain.RefreshToken, error) {
	var (
		rt                  domain.RefreshToken
		issuedAt, expiresAt time.Time
		usedAt              *time.Time
	)
	err := q.db.QueryRow(ctx, `
SELECT id, session_id, token_hash, issued_at, expires_at, used_at, replaced_by
FROM refresh_tokens WHERE token_hash = $1`, hash).
		Scan(&rt.ID, &rt.SessionID, &rt.Hash, &issuedAt, &expiresAt, &usedAt, &rt.ReplacedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RefreshToken{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.RefreshToken{}, fmt.Errorf("find refresh token: %w", err)
	}
	rt.IssuedAt, rt.ExpiresAt, rt.UsedAt = utc(issuedAt), utc(expiresAt), utcPtr(usedAt)
	return rt, nil
}

// UseRefreshToken is the guarded update behind rotation: only an unused token
// can be spent, so two parallel refreshes cannot both succeed and the loser is
// treated as reuse.
func (q *queries) UseRefreshToken(ctx context.Context, id, replacedBy uuid.UUID, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
UPDATE refresh_tokens SET used_at = $2, replaced_by = $3
WHERE id = $1 AND used_at IS NULL`, id, now, replacedBy)
	if err != nil {
		return false, fmt.Errorf("use refresh token: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
