package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// allFields are the user-owned fields CreateProfile sets.
var allFields = []string{
	domain.FieldDisplayName, domain.FieldAvatar, domain.FieldBio, domain.FieldRegion, domain.FieldDistrict,
}

// CreateProfile creates the caller's profile (the id is the caller's).
//
// The row may already exist as PENDING: identity's account stream (or a
// received rating) put it there, with the account projection. Creating fills
// in the public fields, makes it ACTIVE and keeps the projection, counters
// and rating aggregate. When no account event has arrived yet the row is
// inserted with account version 0 and the first event fills in the badges.
// A DELETED profile is created anew. A retry with the same request id
// returns the profile; an ACTIVE profile created with another request id is
// ALREADY_EXISTS.
func (s *Service) CreateProfile(ctx context.Context, p auth.Principal, requestID string, f domain.ProfileFields) (domain.Profile, error) {
	userID, err := auth.RequireUser(p)
	if err != nil {
		return domain.Profile{}, err
	}
	reqID, err := parseID("request_id", requestID)
	if err != nil {
		return domain.Profile{}, err
	}
	if f, err = normalize(f, allFields); err != nil {
		return domain.Profile{}, err
	}
	var (
		prof    domain.Profile
		created bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		now := s.clock()
		if err := q.EnsureProfile(ctx, userID, now); err != nil {
			return err
		}
		rows, err := q.LockProfiles(ctx, userID)
		if err != nil {
			return err
		}
		cur, ok := rows[userID]
		if !ok {
			return fmt.Errorf("profile %s vanished after insert", userID)
		}
		if cur.Status == domain.ProfileActive {
			if cur.CreateRequestID == reqID { // a retry
				prof, err = q.GetProfile(ctx, userID, uuid.Nil)
				return err
			}
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_ALREADY_EXISTS,
				"the caller already has a profile", "user_id", userID.String())
		}
		cur.ProfileFields = f
		cur.Status = domain.ProfileActive
		cur.CreateRequestID = reqID
		cur.Version++
		cur.CreatedAt, cur.UpdatedAt, cur.DeletedAt = now, now, nil
		if err := q.SaveProfile(ctx, cur); err != nil {
			return err
		}
		created = true
		prof, err = q.GetProfile(ctx, userID, uuid.Nil)
		return err
	})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("create profile: %w", err)
	}
	if created {
		s.log.InfoContext(ctx, "profile created", "user_id", userID, "request_id", p.RequestID,
			"account_version", prof.Account.Version)
	}
	// TODO(profile-stream): publish ProfileCreated/ProfileUpdated once
	// profile.proto has such a case (catalog's owner cards need names and
	// avatars); today the stream carries RatingUpdated only.
	return s.present(prof), nil
}

// GetProfile returns a public profile. PENDING (not created by the user yet)
// and DELETED profiles are NOT_FOUND, to everyone including their owner.
// Anonymous callers are allowed; is_followed_by_me is set for signed-in
// callers.
func (s *Service) GetProfile(ctx context.Context, p auth.Principal, userID string) (domain.Profile, error) {
	id, err := parseID("user_id", userID)
	if err != nil {
		return domain.Profile{}, err
	}
	var prof domain.Profile
	err = s.store.Run(ctx, func(q Queries) error {
		prof, err = q.GetProfile(ctx, id, p.UserID)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound) || (err == nil && !prof.Public()):
		return domain.Profile{}, notFound("profile", id)
	case err != nil:
		return domain.Profile{}, fmt.Errorf("get profile: %w", err)
	}
	return s.present(prof), nil
}

// UpdateProfile changes the fields named in mask on the caller's own ACTIVE
// profile. expectedVersion 0 skips the optimistic-concurrency check; a stale
// one is ABORTED / CONFLICT. Values equal to the current ones change nothing
// (no version bump).
func (s *Service) UpdateProfile(ctx context.Context, p auth.Principal, profileID string, f domain.ProfileFields,
	mask []string, expectedVersion int64,
) (domain.Profile, error) {
	userID, err := auth.RequireUser(p)
	if err != nil {
		return domain.Profile{}, err
	}
	id, err := parseID("profile.id", profileID)
	if err != nil {
		return domain.Profile{}, err
	}
	if id != userID {
		return domain.Profile{}, apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
			"only the owner may update a profile")
	}
	if len(mask) == 0 {
		return domain.Profile{}, apperr.Validation(&commonv1.FieldViolation{
			Field: "update_mask", Description: "update_mask must name at least one field", Rule: "update_profile.update_mask_not_empty",
		})
	}
	if f, err = normalize(f, mask); err != nil {
		return domain.Profile{}, err
	}
	var prof domain.Profile
	err = s.store.InTx(ctx, func(q Queries) error {
		rows, err := q.LockProfiles(ctx, id)
		if err != nil {
			return err
		}
		cur, ok := rows[id]
		if !ok || !cur.Public() {
			return notFound("profile", id)
		}
		if expectedVersion > 0 && expectedVersion != cur.Version {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_CONFLICT,
				"the profile changed since it was read; reload it and retry",
				"current_version", strconv.FormatInt(cur.Version, 10))
		}
		if !applyMask(&cur.ProfileFields, f, mask) {
			prof, err = q.GetProfile(ctx, id, uuid.Nil)
			return err
		}
		cur.Version++
		cur.UpdatedAt = s.clock()
		if err := q.SaveProfile(ctx, cur); err != nil {
			return err
		}
		prof, err = q.GetProfile(ctx, id, uuid.Nil)
		return err
	})
	if err != nil {
		return domain.Profile{}, fmt.Errorf("update profile: %w", err)
	}
	return s.present(prof), nil
}

// DeleteProfile soft-deletes a profile: empty userID means the caller's;
// another user's needs ROLE_ADMIN. The profile leaves lists (its follow
// edges are removed and the other sides' counters lowered in the same
// transaction) while its fields, ratings and transaction history stay.
// Deleting a deleted profile is a no-op; a profile that was never created is
// NOT_FOUND.
func (s *Service) DeleteProfile(ctx context.Context, p auth.Principal, userID string) error {
	caller, err := auth.RequireUser(p)
	if err != nil {
		return err
	}
	target := caller
	if userID != "" {
		if target, err = parseID("user_id", userID); err != nil {
			return err
		}
		if target != caller && !p.Staff() {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_PERMISSION_DENIED,
				"deleting another user's profile requires ROLE_ADMIN")
		}
	}
	deleted := false
	err = s.store.InTx(ctx, func(q Queries) error {
		cur, err := q.LockFollowGraph(ctx, target)
		if errors.Is(err, domain.ErrNotFound) {
			return notFound("profile", target)
		}
		if err != nil {
			return err
		}
		switch cur.Status {
		case domain.ProfileDeleted:
			return nil
		case domain.ProfilePending:
			return notFound("profile", target)
		}
		if err := q.RemoveFollows(ctx, target); err != nil {
			return err
		}
		now := s.clock()
		cur.Status = domain.ProfileDeleted
		cur.DeletedAt = &now
		cur.UpdatedAt = now
		cur.Version++
		deleted = true
		return q.SaveProfile(ctx, cur)
	})
	if err != nil {
		return fmt.Errorf("delete profile: %w", err)
	}
	if deleted {
		s.log.InfoContext(ctx, "profile deleted", "user_id", target, "by", caller, "request_id", p.RequestID)
	}
	return nil
}

// normalize trims the fields named in paths and checks what the contract
// cannot express (a display name of blanks). Unknown paths are ignored: the
// contract restricts update_mask to the five public fields.
func normalize(f domain.ProfileFields, paths []string) (domain.ProfileFields, error) {
	var violations []*commonv1.FieldViolation
	bad := func(field, desc, rule string) {
		violations = append(violations, &commonv1.FieldViolation{Field: "profile." + field, Description: desc, Rule: rule})
	}
	for _, path := range paths {
		switch path {
		case domain.FieldDisplayName:
			f.DisplayName = strings.TrimSpace(f.DisplayName)
			if n := utf8.RuneCountInString(f.DisplayName); n < 2 || n > 80 {
				bad(path, "display_name must be 2-80 characters, not counting leading and trailing spaces", "string.min_len")
			}
		case domain.FieldBio:
			f.Bio = strings.TrimSpace(f.Bio)
			if utf8.RuneCountInString(f.Bio) > 500 {
				bad(path, "bio must be at most 500 characters", "string.max_len")
			}
		case domain.FieldDistrict:
			f.District = strings.TrimSpace(f.District)
			if utf8.RuneCountInString(f.District) > 120 {
				bad(path, "district must be at most 120 characters", "string.max_len")
			}
		case domain.FieldRegion:
			if _, ok := commonv1.Region_name[int32(f.Region)]; !ok {
				bad(path, "region must be a defined value", "enum.defined_only")
			}
		}
	}
	if len(violations) > 0 {
		return f, apperr.Validation(violations...)
	}
	return f, nil
}

// applyMask copies the masked fields of f into dst and reports whether any
// value changed.
func applyMask(dst *domain.ProfileFields, f domain.ProfileFields, mask []string) bool {
	before := *dst
	for _, path := range mask {
		switch path {
		case domain.FieldDisplayName:
			dst.DisplayName = f.DisplayName
		case domain.FieldAvatar:
			dst.AvatarMediaID = f.AvatarMediaID
		case domain.FieldBio:
			dst.Bio = f.Bio
		case domain.FieldRegion:
			dst.Region = f.Region
		case domain.FieldDistrict:
			dst.District = f.District
		}
	}
	return *dst != before
}
