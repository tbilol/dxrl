package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

// accountSelect reads an account with its roles.
const accountSelect = `
SELECT a.id, a.status, a.version, a.created_at, a.updated_at,
       coalesce((SELECT array_agg(r.role) FROM account_roles r WHERE r.account_id = a.id), '{}'::text[])
FROM accounts a`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var (
		a                    domain.Account
		status               string
		roleNames            []string
		createdAt, updatedAt time.Time
	)
	err := row.Scan(&a.ID, &status, &a.Version, &createdAt, &updatedAt, &roleNames)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Account{}, err
	}
	if a.Status, err = accountStatuses.value(status); err != nil {
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
	a.CreatedAt, a.UpdatedAt = utc(createdAt), utc(updatedAt)
	return a, nil
}

func (q *queries) CreateAccount(ctx context.Context, a domain.Account, requestID uuid.UUID) (domain.Account, bool, error) {
	status, err := accountStatuses.text(a.Status)
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
INSERT INTO accounts (id, status, version, create_request_id, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (create_request_id) DO NOTHING
RETURNING id`, a.ID, status, a.Version, requestID, a.CreatedAt).Scan(&id)
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
