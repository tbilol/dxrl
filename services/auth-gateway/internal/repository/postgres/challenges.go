package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	authgatewayv1 "github.com/tbilol/dxrl/gen/go/authgateway/v1"

	"github.com/tbilol/dxrl/services/auth-gateway/internal/domain"
)

const challengeSelect = `
SELECT id, phone_number, purpose, code_hash, attempts, max_attempts,
       created_at, expires_at, resend_available_at, consumed_at
FROM phone_challenges`

func scanChallenge(row pgx.Row) (domain.PhoneChallenge, error) {
	var (
		c                                       domain.PhoneChallenge
		purpose                                 string
		createdAt, expiresAt, resendAvailableAt time.Time
		consumedAt                              *time.Time
	)
	err := row.Scan(&c.ID, &c.PhoneNumber, &purpose, &c.CodeHash, &c.Attempts, &c.MaxAttempts,
		&createdAt, &expiresAt, &resendAvailableAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PhoneChallenge{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.PhoneChallenge{}, err
	}
	if c.Purpose, err = purposes.value(purpose); err != nil {
		return domain.PhoneChallenge{}, err
	}
	c.CreatedAt, c.ExpiresAt, c.ResendAvailableAt = utc(createdAt), utc(expiresAt), utc(resendAvailableAt)
	c.ConsumedAt = utcPtr(consumedAt)
	return c, nil
}

func (q *queries) InsertChallenge(ctx context.Context, c domain.PhoneChallenge) error {
	purpose, err := purposes.text(c.Purpose)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `
INSERT INTO phone_challenges (id, phone_number, purpose, code_hash, attempts, max_attempts,
                              created_at, expires_at, resend_available_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		c.ID, c.PhoneNumber, purpose, c.CodeHash, c.Attempts, c.MaxAttempts,
		c.CreatedAt, c.ExpiresAt, c.ResendAvailableAt)
	if err != nil {
		return fmt.Errorf("insert phone challenge: %w", err)
	}
	return nil
}

func (q *queries) GetChallenge(ctx context.Context, id uuid.UUID) (domain.PhoneChallenge, error) {
	return scanChallenge(q.db.QueryRow(ctx, challengeSelect+` WHERE id = $1`, id))
}

func (q *queries) LatestChallenge(ctx context.Context, phone string, purpose authgatewayv1.PhoneCodePurpose) (domain.PhoneChallenge, error) {
	name, err := purposes.text(purpose)
	if err != nil {
		return domain.PhoneChallenge{}, err
	}
	return scanChallenge(q.db.QueryRow(ctx, challengeSelect+`
WHERE phone_number = $1 AND purpose = $2
ORDER BY created_at DESC, id DESC
LIMIT 1`, phone, name))
}

func (q *queries) CountChallengeAttempt(ctx context.Context, id uuid.UUID) error {
	if _, err := q.db.Exec(ctx, `UPDATE phone_challenges SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("count phone challenge attempt: %w", err)
	}
	return nil
}

// ConsumeChallenge is the guarded update that makes a code single-use: only an
// unconsumed, unexpired challenge with attempts left can be spent.
func (q *queries) ConsumeChallenge(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
UPDATE phone_challenges SET consumed_at = $2
WHERE id = $1 AND consumed_at IS NULL AND expires_at > $2 AND attempts < max_attempts`, id, now)
	if err != nil {
		return false, fmt.Errorf("consume phone challenge: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
