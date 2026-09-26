package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
	"github.com/tbilol/dxrl/services/catalog/internal/events"
)

var _ events.InboxStore = (*Store)(nil)

// transactionCols are the listing_transactions columns, in
// GetListingTransaction's scan order and InsertListingTransaction's value
// order.
const transactionCols = `transaction_id, listing_id, farmer_id, reseller_id, quantity_grams, state, version,
	created_at, recorded_at, updated_at`

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

func (q *queries) GetListingTransaction(ctx context.Context, id uuid.UUID) (domain.ListingTransaction, error) {
	var (
		t     domain.ListingTransaction
		state string
	)
	err := q.db.QueryRow(ctx, `SELECT `+transactionCols+` FROM listing_transactions WHERE transaction_id = $1`, id).
		Scan(&t.ID, &t.ListingID, &t.FarmerID, &t.ResellerID, &t.QuantityGrams, &state, &t.Version,
			&t.CreatedAt, &t.RecordedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, domain.ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("get listing transaction: %w", err)
	}
	if t.State, err = transactionStates.value(state); err != nil {
		return t, err
	}
	t.CreatedAt, t.RecordedAt, t.UpdatedAt = utc(t.CreatedAt), utc(t.RecordedAt), utc(t.UpdatedAt)
	return t, nil
}

func (q *queries) InsertListingTransaction(ctx context.Context, t domain.ListingTransaction) error {
	state, err := transactionStates.text(t.State)
	if err != nil {
		return err
	}
	_, err = q.db.Exec(ctx, `
INSERT INTO listing_transactions (`+transactionCols+`)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, t.ID, t.ListingID, t.FarmerID, t.ResellerID, t.QuantityGrams,
		state, t.Version, t.CreatedAt, t.RecordedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert listing transaction: %w", err)
	}
	return nil
}

// UpdateListingTransaction implements service.Queries: the version check and
// the write are one statement, and the terms are never written.
func (q *queries) UpdateListingTransaction(ctx context.Context, t domain.ListingTransaction) (bool, error) {
	state, err := transactionStates.text(t.State)
	if err != nil {
		return false, err
	}
	tag, err := q.db.Exec(ctx, `
UPDATE listing_transactions SET state = $2, version = $3, updated_at = $4
WHERE transaction_id = $1 AND version < $3`, t.ID, state, t.Version, t.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("update listing transaction: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *queries) CommittedGrams(ctx context.Context, listingID uuid.UUID) (int64, error) {
	var grams int64
	// sum(bigint) is numeric; the oversell guard of order-escrow keeps it
	// far below the bigint range.
	err := q.db.QueryRow(ctx, `
SELECT COALESCE(sum(quantity_grams), 0)::bigint FROM listing_transactions WHERE listing_id = $1`, listingID).Scan(&grams)
	if err != nil {
		return 0, fmt.Errorf("sum committed quantity: %w", err)
	}
	return grams, nil
}

func (q *queries) IsTransactionParty(ctx context.Context, listingID, user uuid.UUID) (bool, error) {
	var party bool
	err := q.db.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM listing_transactions WHERE listing_id = $1 AND (farmer_id = $2 OR reseller_id = $2))`,
		listingID, user).Scan(&party)
	if err != nil {
		return false, fmt.Errorf("look up transaction party: %w", err)
	}
	return party, nil
}

// PurgeProcessedEvents implements events.InboxStore.
func (s *Store) PurgeProcessedEvents(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM processed_events WHERE processed_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("purge inbox: %w", err)
	}
	return tag.RowsAffected(), nil
}
