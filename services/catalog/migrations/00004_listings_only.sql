-- catalog: listings only. AGROGRAM no longer has escrow transactions, so
-- nothing commits a listing's quantity any more: the transaction-stream
-- projection and its consumer inbox go, listings lose the remaining quantity,
-- and SOLD_OUT is no longer a status. A listing that sold out is CLOSED: off
-- the board, visible to its owner and staff only, and not counted against the
-- quota, which is exactly what SOLD_OUT meant for it.

-- +goose Up

DROP TABLE processed_events;
DROP TABLE listing_transactions;

UPDATE listings SET status = 'CLOSED' WHERE status = 'SOLD_OUT';
ALTER TABLE listings DROP CONSTRAINT listings_status_check;
ALTER TABLE listings ADD CONSTRAINT listings_status_check
    CHECK (status IN ('ACTIVE', 'CLOSED', 'BLOCKED', 'DELETED'));

-- Drops listings_remaining_grams_check with it.
ALTER TABLE listings DROP COLUMN remaining_grams;

-- +goose Down

-- Restores the schema of migration 00003, empty: the remaining quantity is
-- the whole quantity again, and listings closed by this migration stay
-- CLOSED (which ones were SOLD_OUT is not kept).
ALTER TABLE listings ADD COLUMN remaining_grams bigint;
UPDATE listings SET remaining_grams = quantity_grams;
ALTER TABLE listings ALTER COLUMN remaining_grams SET NOT NULL;
ALTER TABLE listings ADD CONSTRAINT listings_remaining_grams_check
    CHECK (remaining_grams BETWEEN 0 AND quantity_grams);

ALTER TABLE listings DROP CONSTRAINT listings_status_check;
ALTER TABLE listings ADD CONSTRAINT listings_status_check
    CHECK (status IN ('ACTIVE', 'CLOSED', 'SOLD_OUT', 'BLOCKED', 'DELETED'));

CREATE TABLE listing_transactions (
    transaction_id uuid        PRIMARY KEY,
    listing_id     uuid        NOT NULL REFERENCES listings (id),
    farmer_id      uuid        NOT NULL,
    reseller_id    uuid        NOT NULL,
    quantity_grams bigint      NOT NULL CHECK (quantity_grams > 0),
    state          text        NOT NULL CHECK (state IN ('AWAITING_PAYMENT', 'PAYMENT_HELD', 'RECEIPT_SUBMITTED',
                                                        'HANDED_OVER', 'RECEIVED', 'COMPLETED')),
    version        bigint      NOT NULL CHECK (version >= 1),
    created_at     timestamptz NOT NULL,
    recorded_at    timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    CONSTRAINT listing_transactions_distinct_parties CHECK (farmer_id <> reseller_id)
);
CREATE INDEX listing_transactions_listing_idx ON listing_transactions (listing_id);

CREATE TABLE processed_events (
    event_id          uuid        PRIMARY KEY,
    topic             text        NOT NULL,
    event_type        text        NOT NULL,
    aggregate_id      uuid        NOT NULL,
    aggregate_version bigint      NOT NULL,
    processed_at      timestamptz NOT NULL
);
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);
