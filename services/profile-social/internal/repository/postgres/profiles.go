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

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// profileCols are the columns scanProfile reads, in order, from `profiles p`.
const profileCols = `p.id, p.status, p.display_name, p.avatar_media_id, p.bio, p.region, p.district,
       p.create_request_id, p.version, p.created_at, p.updated_at, p.deleted_at,
       p.follower_count, p.following_count, p.roles, p.account_version`

// followedByViewer is the is_followed_by_me column for the viewer in $2.
const followedByViewer = `EXISTS (SELECT 1 FROM follows m WHERE m.follower_id = $2 AND m.followee_id = p.id)`

// scanProfile reads profileCols plus is_followed_by_me, then extra columns.
func scanProfile(row pgx.Row, extra ...any) (domain.Profile, error) {
	var (
		p                   domain.Profile
		status, bio         string
		displayName, region *string
		avatar, createReq   *uuid.UUID
		createdAt           *time.Time
		updatedAt           time.Time
		deletedAt           *time.Time
		roleNames           []string
	)
	dest := append([]any{
		&p.ID, &status, &displayName, &avatar, &bio, &region, &p.District,
		&createReq, &p.Version, &createdAt, &updatedAt, &deletedAt,
		&p.FollowerCount, &p.FollowingCount, &roleNames, &p.Account.Version,
		&p.IsFollowedByMe,
	}, extra...)
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Profile{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Profile{}, err
	}
	if p.Status, err = profileStatuses.value(status); err != nil {
		return domain.Profile{}, err
	}
	if region != nil {
		if p.Region, err = regions.value(*region); err != nil {
			return domain.Profile{}, err
		}
	}
	for _, name := range roleNames {
		r, err := roles.value(name)
		if err != nil {
			return domain.Profile{}, err
		}
		p.Account.Roles = append(p.Account.Roles, r)
	}
	slices.Sort(p.Account.Roles)
	p.Bio = bio
	if displayName != nil {
		p.DisplayName = *displayName
	}
	if avatar != nil {
		p.AvatarMediaID = *avatar
	}
	if createReq != nil {
		p.CreateRequestID = *createReq
	}
	if createdAt != nil {
		p.CreatedAt = utc(*createdAt)
	}
	p.UpdatedAt, p.DeletedAt = utc(updatedAt), utcPtr(deletedAt)
	return p, nil
}

func (q *queries) GetProfile(ctx context.Context, id, viewer uuid.UUID) (domain.Profile, error) {
	return scanProfile(q.db.QueryRow(ctx, `SELECT `+profileCols+`, `+followedByViewer+`
FROM profiles p WHERE p.id = $1`, id, viewer))
}

func (q *queries) EnsureProfile(ctx context.Context, id uuid.UUID, now time.Time) error {
	if _, err := q.db.Exec(ctx, `
INSERT INTO profiles (id, status, updated_at) VALUES ($1, 'PENDING', $2)
ON CONFLICT (id) DO NOTHING`, id, now); err != nil {
		return fmt.Errorf("ensure profile: %w", err)
	}
	return nil
}

func (q *queries) LockProfiles(ctx context.Context, ids ...uuid.UUID) (map[uuid.UUID]domain.Profile, error) {
	rows, err := q.db.Query(ctx, `SELECT `+profileCols+`, false
FROM profiles p WHERE p.id = ANY($1)
ORDER BY p.id
FOR NO KEY UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock profiles: %w", err)
	}
	return collectProfiles(rows)
}

func (q *queries) LockFollowGraph(ctx context.Context, id uuid.UUID) (domain.Profile, error) {
	rows, err := q.db.Query(ctx, `SELECT `+profileCols+`, false
FROM profiles p
WHERE p.id = $1
   OR p.id IN (SELECT followee_id FROM follows WHERE follower_id = $1)
   OR p.id IN (SELECT follower_id FROM follows WHERE followee_id = $1)
ORDER BY p.id
FOR NO KEY UPDATE`, id)
	if err != nil {
		return domain.Profile{}, fmt.Errorf("lock follow graph: %w", err)
	}
	locked, err := collectProfiles(rows)
	if err != nil {
		return domain.Profile{}, err
	}
	p, ok := locked[id]
	if !ok {
		return domain.Profile{}, domain.ErrNotFound
	}
	return p, nil
}

func collectProfiles(rows pgx.Rows) (map[uuid.UUID]domain.Profile, error) {
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Profile, error) { return scanProfile(row) })
	if err != nil {
		return nil, fmt.Errorf("scan profiles: %w", err)
	}
	out := make(map[uuid.UUID]domain.Profile, len(list))
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

func (q *queries) SaveProfile(ctx context.Context, p domain.Profile) error {
	status, err := profileStatuses.text(p.Status)
	if err != nil {
		return err
	}
	var region *string
	if p.Region != commonv1.Region_REGION_UNSPECIFIED {
		name, err := regions.text(p.Region)
		if err != nil {
			return err
		}
		region = &name
	}
	tag, err := q.db.Exec(ctx, `
UPDATE profiles
SET status = $2, display_name = $3, avatar_media_id = $4, bio = $5, region = $6, district = $7,
    create_request_id = $8, version = $9, created_at = $10, updated_at = $11, deleted_at = $12
WHERE id = $1`, p.ID, status, nullString(p.DisplayName), nullUUID(p.AvatarMediaID), p.Bio, region, p.District,
		nullUUID(p.CreateRequestID), p.Version, nullTime(p.CreatedAt), p.UpdatedAt, p.DeletedAt)
	if err != nil {
		return fmt.Errorf("save profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (q *queries) MarkProcessed(ctx context.Context, e domain.ProcessedEvent) (bool, error) {
	tag, err := q.db.Exec(ctx, `
INSERT INTO processed_events (event_id, topic, event_type, aggregate_id, aggregate_version, processed_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (event_id) DO NOTHING`, e.EventID, e.Topic, e.EventType, e.AggregateID, e.AggregateVersion, e.ProcessedAt)
	if err != nil {
		return false, fmt.Errorf("mark event processed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ApplyAccount upserts the projection; the ON CONFLICT ... WHERE makes the
// version check and the write one atomic statement.
func (q *queries) ApplyAccount(ctx context.Context, userID uuid.UUID, a domain.Account, now time.Time) (bool, error) {
	roleNames := make([]string, 0, len(a.Roles))
	for _, r := range a.Roles {
		name, err := roles.text(r)
		if err != nil {
			return false, err
		}
		roleNames = append(roleNames, name)
	}
	tag, err := q.db.Exec(ctx, `
INSERT INTO profiles (id, status, updated_at, roles, account_version, account_synced_at)
VALUES ($1, 'PENDING', $4, $2, $3, $4)
ON CONFLICT (id) DO UPDATE
SET roles = EXCLUDED.roles, account_version = EXCLUDED.account_version,
    account_synced_at = EXCLUDED.account_synced_at
WHERE profiles.account_version < EXCLUDED.account_version`,
		userID, roleNames, a.Version, now)
	if err != nil {
		return false, fmt.Errorf("apply account snapshot: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
