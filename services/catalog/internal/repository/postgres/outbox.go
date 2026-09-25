package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tbilol/dxrl/services/catalog/internal/events"
)

var _ events.OutboxStore = (*Store)(nil)

func (q *queries) EnqueueOutbox(ctx context.Context, m events.Message) error {
	_, err := q.db.Exec(ctx, `
INSERT INTO outbox (id, topic, record_key, payload, headers, event_type, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, m.ID, m.Topic, m.Key, m.Value, m.Headers, m.EventType, m.CreatedAt)
	if err != nil {
		return fmt.Errorf("enqueue outbox message: %w", err)
	}
	return nil
}

func scanMessage(row pgx.CollectableRow) (events.Message, error) {
	var m events.Message
	err := row.Scan(&m.ID, &m.Topic, &m.Key, &m.Value, &m.Headers, &m.EventType, &m.CreatedAt)
	m.CreatedAt = utc(m.CreatedAt)
	return m, err
}

// ProcessOutbox implements events.OutboxStore. The rows stay locked while
// publish runs, so concurrent relays never publish the same batch.
func (s *Store) ProcessOutbox(ctx context.Context, limit int, publish func(context.Context, []events.Message) error) (int, error) {
	var (
		n      int
		pubErr error
	)
	err := inTx(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
SELECT id, topic, record_key, payload, headers, event_type, created_at
FROM outbox
WHERE published_at IS NULL
ORDER BY seq
LIMIT $1
FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		msgs, err := pgx.CollectRows(rows, scanMessage)
		if err != nil || len(msgs) == 0 {
			return err
		}
		n = len(msgs)
		ids := make([]uuid.UUID, n)
		for i, m := range msgs {
			ids[i] = m.ID
		}
		if pubErr = publish(ctx, msgs); pubErr != nil {
			_, err = tx.Exec(ctx, `UPDATE outbox SET attempts = attempts + 1, last_error = left($2, 1000)
WHERE id = ANY($1)`, ids, pubErr.Error())
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE outbox SET published_at = $2 WHERE id = ANY($1)`, ids, s.now().UTC())
		return err
	})
	if err != nil {
		return n, fmt.Errorf("process outbox: %w", err)
	}
	if pubErr != nil {
		return n, fmt.Errorf("publish outbox batch: %w", pubErr)
	}
	return n, nil
}

// PurgePublished implements events.OutboxStore.
func (s *Store) PurgePublished(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM outbox WHERE published_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("purge outbox: %w", err)
	}
	return tag.RowsAffected(), nil
}
