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

const credentialSelect = `
SELECT id, user_id, method, subject, coalesce(password_hash, ''), display_hint, linked_at, last_used_at
FROM sign_in_methods`

func scanCredential(row pgx.Row) (domain.Credential, error) {
	var (
		c          domain.Credential
		method     string
		linkedAt   time.Time
		lastUsedAt *time.Time
	)
	err := row.Scan(&c.ID, &c.UserID, &method, &c.Subject, &c.PasswordHash, &c.DisplayHint, &linkedAt, &lastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Credential{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Credential{}, err
	}
	if c.Method, err = methods.value(method); err != nil {
		return domain.Credential{}, err
	}
	c.LinkedAt, c.LastUsedAt = utc(linkedAt), utcPtr(lastUsedAt)
	return c, nil
}

func (q *queries) CredentialBySubject(ctx context.Context, method authgatewayv1.SignInMethod, subject string) (domain.Credential, error) {
	name, err := methods.text(method)
	if err != nil {
		return domain.Credential{}, err
	}
	return scanCredential(q.db.QueryRow(ctx, credentialSelect+` WHERE method = $1 AND subject = $2`, name, subject))
}

func (q *queries) CredentialsByUser(ctx context.Context, userID uuid.UUID) ([]domain.Credential, error) {
	rows, err := q.db.Query(ctx, credentialSelect+` WHERE user_id = $1 ORDER BY linked_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list sign-in methods: %w", err)
	}
	creds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Credential, error) {
		return scanCredential(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list sign-in methods: %w", err)
	}
	return creds, nil
}

// InsertCredential translates the two unique constraints into the domain
// errors the service maps to PROVIDER_ALREADY_LINKED and
// PHONE_ALREADY_REGISTERED.
func (q *queries) InsertCredential(ctx context.Context, c domain.Credential) error {
	name, err := methods.text(c.Method)
	if err != nil {
		return err
	}
	var hash *string
	if c.PasswordHash != "" {
		hash = &c.PasswordHash
	}
	_, err = q.db.Exec(ctx, `
INSERT INTO sign_in_methods (id, user_id, method, subject, password_hash, display_hint, linked_at, last_used_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		c.ID, c.UserID, name, c.Subject, hash, c.DisplayHint, c.LinkedAt, c.LastUsedAt)
	switch {
	case isUniqueViolation(err, userMethodKey):
		return domain.ErrMethodLinked
	case isUniqueViolation(err, methodSubjectKey):
		return domain.ErrSubjectTaken
	case err != nil:
		return fmt.Errorf("insert sign-in method: %w", err)
	}
	return nil
}

func (q *queries) UpdatePasswordHash(ctx context.Context, credentialID uuid.UUID, hash string) error {
	tag, err := q.db.Exec(ctx, `UPDATE sign_in_methods SET password_hash = $2 WHERE id = $1`, credentialID, hash)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (q *queries) TouchCredential(ctx context.Context, credentialID uuid.UUID, now time.Time) error {
	if _, err := q.db.Exec(ctx, `UPDATE sign_in_methods SET last_used_at = $2 WHERE id = $1`, credentialID, now); err != nil {
		return fmt.Errorf("touch sign-in method: %w", err)
	}
	return nil
}

func (q *queries) DeleteCredential(ctx context.Context, userID uuid.UUID, method authgatewayv1.SignInMethod) (bool, error) {
	name, err := methods.text(method)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `DELETE FROM sign_in_methods WHERE user_id = $1 AND method = $2`, userID, name)
	if err != nil {
		return false, fmt.Errorf("delete sign-in method: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
