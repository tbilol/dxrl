package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// InsertFollow expects both rows locked (service.Queries.LockProfiles), so
// the counter update cannot deadlock with a concurrent follow.
func (q *queries) InsertFollow(ctx context.Context, follower, followee uuid.UUID, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `
INSERT INTO follows (follower_id, followee_id, created_at) VALUES ($1, $2, $3)
ON CONFLICT (follower_id, followee_id) DO NOTHING`, follower, followee, now)
	if err != nil {
		return false, fmt.Errorf("insert follow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := q.db.Exec(ctx, `
UPDATE profiles
SET follower_count = follower_count + (id = $2)::int,
    following_count = following_count + (id = $1)::int
WHERE id IN ($1, $2)`, follower, followee); err != nil {
		return false, fmt.Errorf("count follow: %w", err)
	}
	return true, nil
}

func (q *queries) DeleteFollow(ctx context.Context, follower, followee uuid.UUID) (bool, error) {
	tag, err := q.db.Exec(ctx, `DELETE FROM follows WHERE follower_id = $1 AND followee_id = $2`, follower, followee)
	if err != nil {
		return false, fmt.Errorf("delete follow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := q.db.Exec(ctx, `
UPDATE profiles
SET follower_count = greatest(follower_count - (id = $2)::int, 0),
    following_count = greatest(following_count - (id = $1)::int, 0)
WHERE id IN ($1, $2)`, follower, followee); err != nil {
		return false, fmt.Errorf("uncount follow: %w", err)
	}
	return true, nil
}

// RemoveFollows expects the follow graph locked (LockFollowGraph). Everyone
// id followed loses a follower, everyone who followed id loses a following.
func (q *queries) RemoveFollows(ctx context.Context, id uuid.UUID) error {
	if _, err := q.db.Exec(ctx, `
WITH gone AS (
    DELETE FROM follows WHERE follower_id = $1 OR followee_id = $1
    RETURNING follower_id, followee_id),
lost AS (
    SELECT CASE WHEN follower_id = $1 THEN followee_id ELSE follower_id END AS id,
           count(*) FILTER (WHERE follower_id = $1) AS followers,
           count(*) FILTER (WHERE followee_id = $1) AS following
    FROM gone
    GROUP BY 1)
UPDATE profiles p
SET follower_count = greatest(p.follower_count - l.followers, 0),
    following_count = greatest(p.following_count - l.following, 0)
FROM lost l
WHERE p.id = l.id`, id); err != nil {
		return fmt.Errorf("remove follows: %w", err)
	}
	if _, err := q.db.Exec(ctx, `UPDATE profiles SET follower_count = 0, following_count = 0 WHERE id = $1`, id); err != nil {
		return fmt.Errorf("reset follow counters: %w", err)
	}
	return nil
}

func (q *queries) ListFollowers(ctx context.Context, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error) {
	return q.listFollows(ctx, "follower_id", "followee_id", id, viewer, after, limit)
}

func (q *queries) ListFollowing(ctx context.Context, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error) {
	return q.listFollows(ctx, "followee_id", "follower_id", id, viewer, after, limit)
}

// listFollows lists the profiles on the `listed` side of id's edges (id is on
// the `owner` side), newest follow first. Both column names are constants.
func (q *queries) listFollows(ctx context.Context, listed, owner string, id, viewer uuid.UUID, after *domain.Cursor, limit int) ([]domain.FollowEntry, error) {
	sql := `SELECT ` + profileCols + `, ` + followedByViewer + `, f.created_at
FROM follows f JOIN profiles p ON p.id = f.` + listed + `
WHERE f.` + owner + ` = $1 AND p.status = 'ACTIVE'`
	args := []any{id, viewer, limit}
	if after != nil {
		sql += ` AND (f.created_at, f.` + listed + `) < ($4, $5)`
		args = append(args, after.At, after.ID)
	}
	sql += `
ORDER BY f.created_at DESC, f.` + listed + ` DESC
LIMIT $3`
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list follows: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.FollowEntry, error) {
		var at time.Time
		p, err := scanProfile(row, &at)
		return domain.FollowEntry{Profile: p, FollowedAt: utc(at)}, err
	})
	if err != nil {
		return nil, fmt.Errorf("scan follows: %w", err)
	}
	return entries, nil
}
