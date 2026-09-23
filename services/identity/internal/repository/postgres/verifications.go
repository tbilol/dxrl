package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/identity/internal/domain"
)

const verificationColumns = `id, user_id, request_id, lease_contract_number, status, rejection_reason,
       lease_valid_from, lease_valid_until, attempts, next_attempt_at, submitted_at, decided_at`

func scanVerification(row pgx.Row) (domain.Verification, error) {
	var (
		v           domain.Verification
		status      string
		reason      *string
		from, until *time.Time
		decidedAt   *time.Time
	)
	err := row.Scan(&v.ID, &v.UserID, &v.RequestID, &v.ContractNumber, &status, &reason,
		&from, &until, &v.Attempts, &v.NextAttemptAt, &v.SubmittedAt, &decidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Verification{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Verification{}, err
	}
	if v.Status, err = verificationStatuses.value(status); err != nil {
		return domain.Verification{}, err
	}
	if reason != nil {
		if v.RejectionReason, err = rejectionReasons.value(*reason); err != nil {
			return domain.Verification{}, err
		}
	}
	if from != nil && until != nil {
		v.Lease = &domain.Lease{ContractNumber: v.ContractNumber, ValidFrom: civil(*from), ValidUntil: civil(*until)}
	}
	v.NextAttemptAt, v.SubmittedAt, v.DecidedAt = utc(v.NextAttemptAt), utc(v.SubmittedAt), utcPtr(decidedAt)
	return v, nil
}

func rowToVerification(row pgx.CollectableRow) (domain.Verification, error) {
	return scanVerification(row)
}

// civil normalizes a scanned `date` to 00:00 UTC.
func civil(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (q *queries) GetVerification(ctx context.Context, id uuid.UUID) (domain.Verification, error) {
	return scanVerification(q.db.QueryRow(ctx,
		`SELECT `+verificationColumns+` FROM farmer_verifications WHERE id = $1`, id))
}

func (q *queries) VerificationByRequest(ctx context.Context, userID, requestID uuid.UUID) (domain.Verification, error) {
	return scanVerification(q.db.QueryRow(ctx,
		`SELECT `+verificationColumns+` FROM farmer_verifications WHERE user_id = $1 AND request_id = $2`,
		userID, requestID))
}

func (q *queries) LatestVerification(ctx context.Context, userID uuid.UUID) (domain.Verification, error) {
	return scanVerification(q.db.QueryRow(ctx, `SELECT `+verificationColumns+` FROM farmer_verifications
WHERE user_id = $1 ORDER BY submitted_at DESC, id DESC LIMIT 1`, userID))
}

func (q *queries) ContractClaimed(ctx context.Context, contractNumber string, userID uuid.UUID) (bool, error) {
	var claimed bool
	err := q.db.QueryRow(ctx, `SELECT EXISTS (
    SELECT 1 FROM farmer_verifications
    WHERE lease_contract_number = $1 AND status = 'VERIFIED' AND user_id <> $2)`, contractNumber, userID).Scan(&claimed)
	return claimed, err
}

func (q *queries) InsertVerification(ctx context.Context, v domain.Verification) (domain.Verification, bool, error) {
	stored, err := scanVerification(q.db.QueryRow(ctx, `
INSERT INTO farmer_verifications
    (id, user_id, request_id, lease_contract_number, status, attempts, next_attempt_at, submitted_at, updated_at)
VALUES ($1, $2, $3, $4, 'PENDING', 0, $5, $6, $6)
ON CONFLICT (user_id, request_id) DO NOTHING
RETURNING `+verificationColumns, v.ID, v.UserID, v.RequestID, v.ContractNumber, v.NextAttemptAt, v.SubmittedAt))
	if errors.Is(err, domain.ErrNotFound) { // same (user_id, request_id): return the stored one
		stored, err = q.VerificationByRequest(ctx, v.UserID, v.RequestID)
		return stored, false, err
	}
	if err != nil {
		return domain.Verification{}, false, fmt.Errorf("insert verification: %w", err)
	}
	return stored, true, nil
}

func (q *queries) DecideVerification(ctx context.Context, d domain.Decision) (bool, error) {
	status, err := verificationStatuses.text(d.Status)
	if err != nil {
		return false, err
	}
	var reason *string
	if d.Status == domain.VerificationRejected {
		r, err := rejectionReasons.text(d.RejectionReason)
		if err != nil {
			return false, err
		}
		reason = &r
	}
	var from, until *time.Time
	if d.Lease != nil {
		from, until = &d.Lease.ValidFrom, &d.Lease.ValidUntil
	}
	tag, err := q.db.Exec(ctx, `
UPDATE farmer_verifications
SET status = $2, rejection_reason = $3, lease_valid_from = $4, lease_valid_until = $5,
    decided_at = $6, updated_at = $6, last_error = NULL
WHERE id = $1 AND status = 'PENDING'`, d.VerificationID, status, reason, from, until, d.DecidedAt)
	if isUniqueViolation(err, verifiedContractIndex) {
		return false, domain.ErrContractClaimed
	}
	if err != nil {
		return false, fmt.Errorf("decide verification: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *queries) ClaimPendingVerifications(ctx context.Context, now time.Time, limit int, backoff, maxBackoff time.Duration) ([]domain.Verification, error) {
	rows, err := q.db.Query(ctx, `
UPDATE farmer_verifications
SET attempts = attempts + 1,
    next_attempt_at = $1::timestamptz + least(
        make_interval(secs => $3::float8 * power(2, least(attempts, 20))),
        make_interval(secs => $4::float8)),
    updated_at = $1::timestamptz
WHERE id IN (
    SELECT id FROM farmer_verifications
    WHERE status = 'PENDING' AND next_attempt_at <= $1::timestamptz
    ORDER BY next_attempt_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED)
RETURNING `+verificationColumns, now, limit, backoff.Seconds(), maxBackoff.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim pending verifications: %w", err)
	}
	return pgx.CollectRows(rows, rowToVerification)
}

func (q *queries) RecordVerificationError(ctx context.Context, id uuid.UUID, msg string) error {
	_, err := q.db.Exec(ctx, `UPDATE farmer_verifications SET last_error = left($2, 1000) WHERE id = $1`, id, msg)
	return err
}

func (q *queries) ExpiryCandidates(ctx context.Context, today time.Time, limit int) ([]domain.Verification, error) {
	rows, err := q.db.Query(ctx, `SELECT `+verificationColumns+` FROM farmer_verifications
WHERE status = 'VERIFIED' AND lease_valid_until < $1::date
ORDER BY lease_valid_until, id
LIMIT $2`, today, limit)
	if err != nil {
		return nil, fmt.Errorf("list expiry candidates: %w", err)
	}
	return pgx.CollectRows(rows, rowToVerification)
}

func (q *queries) ExpireVerification(ctx context.Context, id uuid.UUID, today, now time.Time) (domain.Verification, bool, error) {
	v, err := scanVerification(q.db.QueryRow(ctx, `
UPDATE farmer_verifications
SET status = 'EXPIRED', expired_at = $3, updated_at = $3
WHERE id = $1 AND status = 'VERIFIED' AND lease_valid_until < $2::date
RETURNING `+verificationColumns, id, today, now))
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Verification{}, false, nil
	}
	if err != nil {
		return domain.Verification{}, false, fmt.Errorf("expire verification: %w", err)
	}
	return v, true, nil
}
