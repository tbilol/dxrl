package service

import (
	"context"
	"fmt"

	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
)

// ApplyAccountEvent applies one event of identity's account stream to the
// account projection on the user's profile row (roles, verified-farmer and
// Premium badges), following the consumer rules of metadata.proto:
//
//   - de-duplication: the event id goes into the inbox (processed_events) in
//     the same transaction; a known id is a Duplicate and changes nothing;
//   - versioning: the snapshot is applied only when its version is newer than
//     the stored account_version, so an older event arriving after a newer
//     one, and FarmerVerified / FarmerVerificationRejected /
//     FarmerVerificationExpired (which repeat the current version; the badge
//     change comes with the AccountRoleChanged after them), are Stale;
//   - every event type, including a oneof case newer than this code, takes
//     this one path: the snapshot is all the projection needs.
//
// The upsert creates a PENDING row when the user has none yet (AccountCreated,
// or any event arriving first) and never touches the user-owned fields, so
// CreateProfile and account events may arrive in either order.
func (s *Service) ApplyAccountEvent(ctx context.Context, u domain.AccountUpdate) (domain.ApplyOutcome, error) {
	var outcome domain.ApplyOutcome
	err := s.store.InTx(ctx, func(q Queries) error {
		now := s.clock()
		fresh, err := q.MarkProcessed(ctx, domain.ProcessedEvent{
			EventID:          u.EventID,
			Topic:            u.Topic,
			EventType:        u.EventType,
			AggregateID:      u.UserID,
			AggregateVersion: u.Account.Version,
			ProcessedAt:      now,
		})
		if err != nil {
			return err
		}
		if !fresh {
			outcome = domain.Duplicate
			return nil
		}
		applied, err := q.ApplyAccount(ctx, u.UserID, u.Account, now)
		if err != nil {
			return err
		}
		outcome = domain.Stale
		if applied {
			outcome = domain.Applied
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("apply account event: %w", err)
	}
	if outcome == domain.Applied {
		s.log.InfoContext(ctx, "account projection updated", "user_id", u.UserID, "version", u.Account.Version,
			"event_type", u.EventType, "is_verified_farmer", u.Account.IsVerifiedFarmer, "is_premium", u.Account.IsPremium)
	}
	return outcome, nil
}
