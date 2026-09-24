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
// Eligibility comes from completed_transactions, filled from order-escrow's
// transaction stream: TODO(phase3), until then there are no rows and every
// call is RATING_NOT_ALLOWED. Errors: RATING_NOT_ALLOWED (no completed
// transaction with that id that the caller is a party of), ALREADY_RATED. A
// retry with the same request id returns the original rating.
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

// RecordCompletedTransaction records a completed escrow transaction: it
// unlocks one rating per party and adds one to both parties' deals_count,
// once per transaction id (a replay changes nothing and returns false).
//
// TODO(phase3): the caller is events.TransactionHandler, the consumer of
// agrogram.orderescrow.transaction.v1 (PayoutReleased: state COMPLETED),
// which is not subscribed until order-escrow exists.
func (s *Service) RecordCompletedTransaction(ctx context.Context, t domain.CompletedTransaction) (bool, error) {
	if t.ID == uuid.Nil || t.FarmerID == uuid.Nil || t.ResellerID == uuid.Nil || t.FarmerID == t.ResellerID {
		return false, fmt.Errorf("record completed transaction %s: needs an id and two distinct parties", t.ID)
	}
	var recorded bool
	err := s.store.InTx(ctx, func(q Queries) error {
		now := s.clock()
		var err error
		if recorded, err = q.InsertCompletedTransaction(ctx, t, now); err != nil || !recorded {
			return err
		}
		for _, id := range []uuid.UUID{t.FarmerID, t.ResellerID} {
			if err := q.EnsureProfile(ctx, id, now); err != nil {
				return err
			}
		}
		if _, err := q.LockProfiles(ctx, t.FarmerID, t.ResellerID); err != nil {
			return err
		}
		return q.AddDeal(ctx, t.FarmerID, t.ResellerID)
	})
	if err != nil {
		return false, fmt.Errorf("record completed transaction %s: %w", t.ID, err)
	}
	if recorded {
		s.log.InfoContext(ctx, "completed transaction recorded", "transaction_id", t.ID,
			"farmer_id", t.FarmerID, "reseller_id", t.ResellerID)
	}
	return recorded, nil
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
