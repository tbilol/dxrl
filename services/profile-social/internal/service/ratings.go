package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	commonv1 "github.com/tbilol/dxrl/gen/go/common/v1"
	eventsv1 "github.com/tbilol/dxrl/gen/go/events/v1"

	"github.com/tbilol/dxrl/services/profile-social/internal/apperr"
	"github.com/tbilol/dxrl/services/profile-social/internal/auth"
	"github.com/tbilol/dxrl/services/profile-social/internal/domain"
	"github.com/tbilol/dxrl/services/profile-social/internal/events"
)

// RateUser rates the caller's counterparty in a completed transaction, once
// per transaction and rater. The rating, the rated user's aggregate (sum,
// count, version) and RatingUpdated in the outbox commit together.
//
// Eligibility comes from completed_transactions, which the consumer of
// order-escrow's transaction stream fills when a transaction reaches
// COMPLETED (PayoutReleased; see RecordCompletedTransaction). Errors:
// RATING_NOT_ALLOWED (no completed transaction with that id that the caller
// is a party of, which includes one whose PayoutReleased has not been
// consumed yet), ALREADY_RATED. A retry with the same request id returns the
// original rating.
//
// RatingUpdated needs a profile snapshot with a display name, so it is
// published only when the rated user's profile is ACTIVE; the rating and the
// aggregate are stored either way.
func (s *Service) RateUser(ctx context.Context, p auth.Principal, requestID, transactionID string, score int32, comment string) (domain.Rating, error) {
	rater, err := auth.RequireUser(p)
	if err != nil {
		return domain.Rating{}, err
	}
	reqID, err := parseID("request_id", requestID)
	if err != nil {
		return domain.Rating{}, err
	}
	txID, err := parseID("transaction_id", transactionID)
	if err != nil {
		return domain.Rating{}, err
	}
	comment = strings.TrimSpace(comment)
	switch {
	case score < 1 || score > 5:
		return domain.Rating{}, apperr.Validation(&commonv1.FieldViolation{
			Field: "score", Description: "score must be 1-5", Rule: "int32.gte_lte"})
	case utf8.RuneCountInString(comment) > 500:
		return domain.Rating{}, apperr.Validation(&commonv1.FieldViolation{
			Field: "comment", Description: "comment must be at most 500 characters", Rule: "string.max_len"})
	}
	id, err := newID()
	if err != nil {
		return domain.Rating{}, err
	}
	var (
		rating    domain.Rating
		rated     domain.Profile
		stored    bool
		published bool
	)
	err = s.store.InTx(ctx, func(q Queries) error {
		if prev, err := q.RatingByRequest(ctx, rater, reqID); err == nil {
			rating = prev // a retry
			return nil
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		tx, err := q.GetCompletedTransaction(ctx, txID)
		if errors.Is(err, domain.ErrNotFound) {
			return notAllowed(txID, "ratings need a completed transaction between the two users")
		}
		if err != nil {
			return err
		}
		ratedID, ok := tx.Counterparty(rater)
		if !ok {
			return notAllowed(txID, "the caller is not a party of this transaction")
		}
		if ratedID == rater {
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_SELF_ACTION_NOT_ALLOWED, "users cannot rate themselves")
		}
		now := s.clock()
		rating = domain.Rating{
			ID: id, TransactionID: txID, RaterID: rater, RatedUserID: ratedID, RequestID: reqID,
			Score: score, Comment: comment, CreatedAt: now,
		}
		if err := q.EnsureProfile(ctx, ratedID, now); err != nil {
			return err
		}
		inserted, err := q.InsertRating(ctx, rating)
		if err != nil {
			return err
		}
		if !inserted {
			if prev, err := q.RatingByRequest(ctx, rater, reqID); err == nil {
				rating = prev // a concurrent retry won
				return nil
			}
			return apperr.New(commonv1.ErrorReason_ERROR_REASON_ALREADY_RATED,
				"the caller has already rated this transaction", "transaction_id", txID.String())
		}
		stored = true
		if rated, err = q.AddRating(ctx, ratedID, score, now); err != nil {
			return err
		}
		if !rated.Public() {
			return nil
		}
		published = true
		return s.enqueueProfileEvent(ctx, q, rated, &eventsv1.RatingUpdated{
			RatingId:      rating.ID.String(),
			RaterId:       rater.String(),
			TransactionId: txID.String(),
			Score:         score,
		}, now, requestTrigger(p))
	})
	if err != nil {
		return domain.Rating{}, fmt.Errorf("rate user: %w", err)
	}
	if stored {
		s.log.InfoContext(ctx, "user rated", "rated_user_id", rating.RatedUserID, "rater_id", rater,
			"transaction_id", txID, "score", score, "rating_count", rated.RatingCount, "rating_updated_published", published)
	}
	return rating, nil
}

func notAllowed(txID uuid.UUID, msg string) error {
	return apperr.New(commonv1.ErrorReason_ERROR_REASON_RATING_NOT_ALLOWED, msg, "transaction_id", txID.String())
}

// ListRatings lists the ratings userID received, newest first, with the
// aggregate. Public; the user's profile must be ACTIVE.
func (s *Service) ListRatings(ctx context.Context, _ auth.Principal, userID string, page domain.PageRequest) (domain.RatingPage, error) {
	id, err := parseID("user_id", userID)
	if err != nil {
		return domain.RatingPage{}, err
	}
	after, err := decodeCursor(page.Token, listRatings, id)
	if err != nil {
		return domain.RatingPage{}, err
	}
	size := pageSize(page.Size)
	var (
		owner   domain.Profile
		ratings []domain.Rating
	)
	err = s.store.Run(ctx, func(q Queries) error {
		if owner, err = q.GetProfile(ctx, id, uuid.Nil); err != nil {
			return err
		}
		if !owner.Public() {
			return domain.ErrNotFound
		}
		ratings, err = q.ListRatings(ctx, id, after, size+1)
		return err
	})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return domain.RatingPage{}, notFound("profile", id)
	case err != nil:
		return domain.RatingPage{}, fmt.Errorf("list ratings: %w", err)
	}
	out := domain.RatingPage{Average: owner.RatingAverage(), Count: owner.RatingCount}
	if len(ratings) > size {
		ratings = ratings[:size]
		last := ratings[size-1]
		out.NextPageToken = encodeCursor(listRatings, id, domain.Cursor{At: last.CreatedAt, ID: last.ID})
	}
	out.Ratings = ratings
	return out, nil
}

// RecordCompletedTransaction records a completed escrow transaction from one
// COMPLETED event of order-escrow's transaction stream (the caller is
// events.TransactionHandler; PayoutReleased is the first such event): it
// unlocks one rating per party and adds one to both parties' deals_count.
// Following the consumer rules of metadata.proto:
//
//   - de-duplication: the event id goes into the inbox (processed_events) in
//     the same transaction; a known id is a Duplicate and changes nothing;
//   - once per transaction id: the first COMPLETED event records the
//     transaction (Applied); any other one, such as the BonusAwarded that
//     follows PayoutReleased, finds it recorded and is Stale.
//
// deals_count therefore grows once per transaction id, which is stricter
// than metadata.proto's rule of reacting to a fact once per event_id:
// PayoutReleased and BonusAwarded both carry a COMPLETED snapshot, and
// counting per event would count the deal twice. No version guard is needed:
// COMPLETED is final and recording is monotonic (the row is only ever
// inserted), so a late or replayed earlier event can never undo it, and
// events before COMPLETED never get here.
func (s *Service) RecordCompletedTransaction(ctx context.Context, u domain.TransactionUpdate) (domain.ApplyOutcome, error) {
	t := u.Transaction
	if u.EventID == uuid.Nil || t.ID == uuid.Nil || t.FarmerID == uuid.Nil || t.ResellerID == uuid.Nil ||
		t.FarmerID == t.ResellerID {
		return 0, fmt.Errorf("record completed transaction %s: needs an event id, a transaction id and two distinct parties",
			t.ID)
	}
	var outcome domain.ApplyOutcome
	err := s.store.InTx(ctx, func(q Queries) error {
		now := s.clock()
		fresh, err := q.MarkProcessed(ctx, domain.ProcessedEvent{
			EventID:          u.EventID,
			Topic:            u.Topic,
			EventType:        u.EventType,
			AggregateID:      t.ID,
			AggregateVersion: u.Version,
			ProcessedAt:      now,
		})
		if err != nil {
			return err
		}
		if !fresh {
			outcome = domain.Duplicate
			return nil
		}
		recorded, err := q.InsertCompletedTransaction(ctx, t, now)
		if err != nil {
			return err
		}
		if !recorded {
			outcome = domain.Stale
			return nil
		}
		for _, id := range []uuid.UUID{t.FarmerID, t.ResellerID} {
			if err := q.EnsureProfile(ctx, id, now); err != nil {
				return err
			}
		}
		if _, err := q.LockProfiles(ctx, t.FarmerID, t.ResellerID); err != nil {
			return err
		}
		if err := q.AddDeal(ctx, t.FarmerID, t.ResellerID); err != nil {
			return err
		}
		outcome = domain.Applied
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("record completed transaction %s: %w", t.ID, err)
	}
	if outcome == domain.Applied {
		s.log.InfoContext(ctx, "completed transaction recorded", "transaction_id", t.ID,
			"farmer_id", t.FarmerID, "reseller_id", t.ResellerID, "event_type", u.EventType, "version", u.Version)
	}
	return outcome, nil
}

// enqueueProfileEvent writes one ProfileEvent to the outbox through q, so it
// commits with the change it describes (at is that change's time).
func (s *Service) enqueueProfileEvent(ctx context.Context, q Queries, prof domain.Profile, event *eventsv1.RatingUpdated,
	at time.Time, tr trigger,
) error {
	msg, err := events.NewProfileEvent(events.ProfileChange{
		Profile:       prof,
		Event:         event,
		OccurredAt:    at,
		CorrelationID: tr.correlationID,
		Traceparent:   tr.traceparent,
		Tracestate:    tr.tracestate,
		Actor:         tr.actor,
	})
	if err != nil {
		return err
	}
	return q.EnqueueOutbox(ctx, msg)
}
