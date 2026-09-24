package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

func (q *queries) GetCompletedTransaction(ctx context.Context, id uuid.UUID) (domain.CompletedTransaction, error) {
	var t domain.CompletedTransaction
	err := q.db.QueryRow(ctx, `
SELECT id, farmer_id, reseller_id, completed_at FROM completed_transactions WHERE id = $1`, id).
		Scan(&t.ID, &t.FarmerID, &t.ResellerID, &t.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CompletedTransaction{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.CompletedTransaction{}, fmt.Errorf("get completed transaction: %w", err)
	}
	t.CompletedAt = utc(t.CompletedAt)
	return t, nil
}

func (q *queries) InsertCompletedTransaction(ctx context.Context, t domain.CompletedTransaction, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
INSERT INTO completed_transactions (id, farmer_id, reseller_id, completed_at, recorded_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING`, t.ID, t.FarmerID, t.ResellerID, t.CompletedAt, now)
	if err != nil {
		return false, fmt.Errorf("insert completed transaction: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *queries) AddDeal(ctx context.Context, ids ...uuid.UUID) error {
	if _, err := q.db.Exec(ctx, `UPDATE profiles SET deals_count = deals_count + 1 WHERE id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("count deal: %w", err)
	}
	return nil
}

const ratingCols = `id, transaction_id, rater_id, rated_user_id, request_id, score, comment, created_at`

func scanRating(row pgx.Row) (domain.Rating, error) {
	var r domain.Rating
	err := row.Scan(&r.ID, &r.TransactionID, &r.RaterID, &r.RatedUserID, &r.RequestID, &r.Score, &r.Comment, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Rating{}, domain.ErrNotFound
	}
	r.CreatedAt = utc(r.CreatedAt)
	return r, err
}

func (q *queries) RatingByRequest(ctx context.Context, raterID, requestID uuid.UUID) (domain.Rating, error) {
	return scanRating(q.db.QueryRow(ctx, `SELECT `+ratingCols+` FROM ratings WHERE rater_id = $1 AND request_id = $2`,
		raterID, requestID))
}

// InsertRating relies on the two unique constraints: (transaction_id,
// rater_id) is ALREADY_RATED, (rater_id, request_id) an idempotent retry.
func (q *queries) InsertRating(ctx context.Context, r domain.Rating) (bool, error) {
	tag, err := q.db.Exec(ctx, `
INSERT INTO ratings (`+ratingCols+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT DO NOTHING`, r.ID, r.TransactionID, r.RaterID, r.RatedUserID, r.RequestID, r.Score, r.Comment, r.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert rating: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *queries) AddRating(ctx context.Context, userID uuid.UUID, score int32, now time.Time) (domain.Profile, error) {
	p, err := scanProfile(q.db.QueryRow(ctx, `
UPDATE profiles AS p
SET rating_sum = p.rating_sum + $2, rating_count = p.rating_count + 1, version = p.version + 1, updated_at = $3
WHERE p.id = $1
RETURNING `+profileCols+`, false`, userID, score, now))
	if err != nil {
		return domain.Profile{}, fmt.Errorf("add rating: %w", err)
	}
	return p, nil
}

func (q *queries) ListRatings(ctx context.Context, userID uuid.UUID, after *domain.Cursor, limit int) ([]domain.Rating, error) {
	sql := `SELECT ` + ratingCols + ` FROM ratings WHERE rated_user_id = $1`
	args := []any{userID, limit}
	if after != nil {
		sql += ` AND (created_at, id) < ($3, $4)`
		args = append(args, after.At, after.ID)
	}
	sql += `
ORDER BY created_at DESC, id DESC
LIMIT $2`
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list ratings: %w", err)
	}
	ratings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Rating, error) { return scanRating(row) })
	if err != nil {
		return nil, fmt.Errorf("scan ratings: %w", err)
	}
	return ratings, nil
}
