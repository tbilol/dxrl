package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// Page tokens name the list they belong to.
const (
	listFollowers = "followers"
	listFollowing = "following"
	listRatings   = "ratings"
)

// FollowUser makes the caller follow userID and returns that profile with
// its counters. Both profiles must be ACTIVE: the target (NOT_FOUND) and the
// caller's own (FAILED_PRECONDITION: create it first). Following a user
// already followed is a no-op. Race-safe: both rows are locked in id order,
// the edge is inserted with ON CONFLICT DO NOTHING, and the counters change
// only when a row was inserted, all in one transaction.
func (s *Service) FollowUser(ctx context.Context, p auth.Principal, userID string) (domain.Profile, error) {
	follower, target, err := s.followArgs(ctx, p, userID)
	if err != nil {
		return domain.Profile{}, err
	}
	var (
		prof  domain.Profile
		added bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		rows, err := q.LockProfiles(ctx, follower, target)
		if err != nil {
			return err
		}
		if t, ok := rows[target]; !ok || !t.Public() {
			return notFound("profile", target)
		}
		if f, ok := rows[follower]; !ok || !f.Public() {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_FAILED_PRECONDITION,
				"create your profile (CreateProfile) before following other users")
		}
		if added, err = q.InsertFollow(ctx, follower, target, s.clock()); err != nil {
			return err
		}
		prof, err = q.GetProfile(ctx, target, follower)
		return err
	})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("follow user: %w", err)
	}
	if added {
		s.log.DebugContext(ctx, "followed", "follower_id", follower, "followee_id", target)
	}
	return prof, nil
}

// UnfollowUser removes the caller's follow of userID and returns that
// profile. Unfollowing a user not followed is a no-op; a target without a
// public profile is NOT_FOUND.
func (s *Service) UnfollowUser(ctx context.Context, p auth.Principal, userID string) (domain.Profile, error) {
	follower, target, err := s.followArgs(ctx, p, userID)
	if err != nil {
		return domain.Profile{}, err
	}
	var prof domain.Profile
	err = s.store.InTx(ctx, func(q Queries) error {
		rows, err := q.LockProfiles(ctx, follower, target)
		if err != nil {
			return err
		}
		if t, ok := rows[target]; !ok || !t.Public() {
			return notFound("profile", target)
		}
		if _, err := q.DeleteFollow(ctx, follower, target); err != nil {
			return err
		}
		prof, err = q.GetProfile(ctx, target, follower)
		return err
	})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("unfollow user: %w", err)
	}
	return prof, nil
}

// followArgs checks the caller, the target and the rate limit.
func (s *Service) followArgs(ctx context.Context, p auth.Principal, userID string) (follower, target uuid.UUID, err error) {
	if follower, err = auth.RequireUser(p); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if target, err = parseID("user_id", userID); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if target == follower {
		return uuid.Nil, uuid.Nil, apperr.New(commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED,
			"users cannot follow or unfollow themselves")
	}
	if err := s.checkRateLimit(ctx, follower); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return follower, target, nil
}

// ListFollowers lists the ACTIVE profiles following userID, newest first.
func (s *Service) ListFollowers(ctx context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.ProfilePage, error) {
	return s.listFollows(ctx, p, userID, page, listFollowers)
}

// ListFollowing lists the ACTIVE profiles userID follows, newest first.
func (s *Service) ListFollowing(ctx context.Context, p auth.Principal, userID string, page domain.PageRequest) (domain.ProfilePage, error) {
	return s.listFollows(ctx, p, userID, page, listFollowing)
}

// listFollows serves both lists: public, anonymous callers allowed, the
// user's profile must be public, keyset pagination on (follow time, id) and
// total_size from the cached counter.
func (s *Service) listFollows(ctx context.Context, p auth.Principal, userID string, page domain.PageRequest, list string) (domain.ProfilePage, error) {
	id, err := parseID("user_id", userID)
	if err != nil {
		return domain.ProfilePage{}, err
	}
	after, err := decodeCursor(page.Token, list, id)
	if err != nil {
		return domain.ProfilePage{}, err
	}
	size := pageSize(page.Size)
	var (
		owner   domain.Profile
		entries []domain.FollowEntry
	)
	err = s.store.Run(ctx, func(q Queries) error {
		if owner, err = q.GetProfile(ctx, id, uuid.Nil); err != nil {
			return err
		}
		if !owner.Public() {
			return domain.ErrNotFound
		}
		if list == listFollowers {
			entries, err = q.ListFollowers(ctx, id, p.UserID, after, size+1)
		} else {
			entries, err = q.ListFollowing(ctx, id, p.UserID, after, size+1)
		}
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.ProfilePage{}, notFound("profile", id)
	case err != nil:
		return domain.ProfilePage{}, fmt.Errorf("list %s: %w", list, err)
	}
	out := domain.ProfilePage{TotalSize: owner.FollowerCount}
	if list == listFollowing {
		out.TotalSize = owner.FollowingCount
	}
	if len(entries) > size {
		entries = entries[:size]
		last := entries[size-1]
		out.NextPageToken = encodeCursor(list, id, domain.Cursor{At: last.FollowedAt, ID: last.Profile.ID})
	}
	out.Profiles = make([]domain.Profile, len(entries))
	for i, e := range entries {
		out.Profiles[i] = e.Profile
	}
	return out, nil
}
