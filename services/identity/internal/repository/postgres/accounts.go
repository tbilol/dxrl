package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// accountSelect reads an account with its roles and the status of its latest
// farmer verification.
const accountSelect = `
SELECT a.id, a.status, a.plan, a.premium_until, a.version, a.created_at, a.updated_at,
       coalesce((SELECT array_agg(r.role) FROM account_roles r WHERE r.account_id = a.id), '{}'::text[]),
       coalesce((SELECT v.status FROM farmer_verifications v WHERE v.user_id = a.id
                 ORDER BY v.submitted_at DESC, v.id DESC LIMIT 1), '')
FROM accounts a`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var (
		a                      domain.Account
		status, plan, fvStatus string
		roleNames              []string
		premiumUntil           *time.Time
		createdAt, updatedAt   time.Time
	)
	err := row.Scan(&a.ID, &status, &plan, &premiumUntil, &a.Version, &createdAt, &updatedAt, &roleNames, &fvStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Account{}, err
	}
	if a.Status, err = accountStatuses.value(status); err != nil {
		return domain.Account{}, err
	}
	if a.Plan, err = plans.value(plan); err != nil {
		return domain.Account{}, err
	}
	for _, name := range roleNames {
		r, err := roles.value(name)
		if err != nil {
			return domain.Account{}, err
		}
		a.Roles = append(a.Roles, r)
	}
	slices.Sort(a.Roles)
	if fvStatus != "" {
		if a.FarmerVerificationStatus, err = verificationStatuses.value(fvStatus); err != nil {
			return domain.Account{}, err
		}
	}
	a.PremiumUntil, a.CreatedAt, a.UpdatedAt = utcPtr(premiumUntil), utc(createdAt), utc(updatedAt)
	return a, nil
}

func (q *queries) CreateAccount(ctx context.Context, a domain.Account, requestID uuid.UUID) (domain.Account, bool, error) {
	status, err := accountStatuses.text(a.Status)
	if err != nil {
		return domain.Account{}, false, err
	}
	plan, err := plans.text(a.Plan)
	if err != nil {
		return domain.Account{}, false, err
	}
	roleNames := make([]string, 0, len(a.Roles))
	for _, r := range a.Roles {
		name, err := roles.text(r)
		if err != nil {
			return domain.Account{}, false, err
		}
		roleNames = append(roleNames, name)
	}

	var id uuid.UUID
	err = q.db.QueryRow(ctx, `
INSERT INTO accounts (id, status, plan, premium_until, version, create_request_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
ON CONFLICT (create_request_id) DO NOTHING
RETURNING id`, a.ID, status, plan, a.PremiumUntil, a.Version, requestID, a.CreatedAt).Scan(&id)
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) { // same request id: return the existing account
		err = q.db.QueryRow(ctx, `SELECT id FROM accounts WHERE create_request_id = $1`, requestID).Scan(&id)
	}
	if err != nil {
		return domain.Account{}, false, fmt.Errorf("insert account: %w", err)
	}
	if created && len(roleNames) > 0 {
		if _, err := q.db.Exec(ctx, `
INSERT INTO account_roles (account_id, role, granted_at)
SELECT $1, unnest($2::text[]), $3`, id, roleNames, a.CreatedAt); err != nil {
			return domain.Account{}, false, fmt.Errorf("insert account roles: %w", err)
		}
	}
	acct, err := q.GetAccount(ctx, id)
	return acct, created, err
}

func (q *queries) GetAccount(ctx context.Context, id uuid.UUID) (domain.Account, error) {
	return scanAccount(q.db.QueryRow(ctx, accountSelect+` WHERE a.id = $1`, id))
}

func (q *queries) LockAccount(ctx context.Context, id uuid.UUID) error {
	var one int
	err := q.db.QueryRow(ctx, `SELECT 1 FROM accounts WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

func (q *queries) GrantRole(ctx context.Context, id uuid.UUID, role commonv1.Role, now time.Time) (bool, error) {
	name, err := roles.text(role)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `
WITH granted AS (
    INSERT INTO account_roles (account_id, role, granted_at) VALUES ($1, $2, $3)
    ON CONFLICT (account_id, role) DO NOTHING
    RETURNING account_id)
UPDATE accounts SET version = version + 1, updated_at = $3
WHERE id IN (SELECT account_id FROM granted)`, id, name, now)
	if err != nil {
		return false, fmt.Errorf("grant role %s: %w", name, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *queries) RevokeFarmerRoleUnlessVerified(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
WITH revoked AS (
    DELETE FROM account_roles
    WHERE account_id = $1 AND role = 'FARMER'
      AND NOT EXISTS (SELECT 1 FROM farmer_verifications WHERE user_id = $1 AND status = 'VERIFIED')
    RETURNING account_id)
UPDATE accounts SET version = version + 1, updated_at = $2
WHERE id IN (SELECT account_id FROM revoked)`, id, now)
	if err != nil {
		return false, fmt.Errorf("revoke farmer role: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
