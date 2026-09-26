-- catalog: the consumer of order-escrow's transaction stream
-- (agrogram.orderescrow.transaction.v1). listing_transactions projects the
-- escrow transactions on listings: it drives the remaining quantity, SOLD_OUT
-- and which users besides the owner may read a listing that is not ACTIVE.
-- processed_events is the consumer inbox.

-- +goose Up

-- Escrow transactions on listings, one row per transaction id, inserted the
-- first time catalog sees ANY event of the transaction (normally
-- TransactionCreated). The insert is the commitment: listings.remaining_grams
-- is the listing's quantity_grams minus the sum of quantity_grams of its rows,
-- never below 0, recomputed from that sum on every new row, and an ACTIVE
-- listing whose remaining quantity reaches 0 becomes SOLD_OUT. The farmer and
-- the reseller of a row may read the listing in any status (GetListing).
--
-- The terms (listing, parties, quantity) are fixed when order-escrow creates
-- the transaction and never change; state and version follow the newest
-- event (an older version is ignored). The sold quantity is derivable from
-- state (COMPLETED rows) should it be needed later. Rows are never deleted:
-- the contract has no cancel or expiry event (order-escrow's TODO(cancel)), so
-- a commitment is permanent.
CREATE TABLE listing_transactions (
    transaction_id uuid        PRIMARY KEY, -- TransactionSnapshot.id
    listing_id     uuid        NOT NULL REFERENCES listings (id),
    -- Seller and buyer. The listing's owner is the farmer of an offer and the
    -- reseller of a buy-request; the other party started the transaction.
    farmer_id      uuid        NOT NULL,
    reseller_id    uuid        NOT NULL,
    -- Agreed quantity: what the transaction commits of the listing.
    quantity_grams bigint      NOT NULL CHECK (quantity_grams > 0),
    state          text        NOT NULL CHECK (state IN ('AWAITING_PAYMENT', 'PAYMENT_HELD', 'RECEIPT_SUBMITTED',
                                                        'HANDED_OVER', 'RECEIVED', 'COMPLETED')),
    -- TransactionSnapshot.version of the newest event applied.
    version        bigint      NOT NULL CHECK (version >= 1),
    -- TransactionSnapshot.created_at: when order-escrow created the transaction.
    created_at     timestamptz NOT NULL,
    -- When catalog first saw the transaction, i.e. committed its quantity.
    recorded_at    timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    CONSTRAINT listing_transactions_distinct_parties CHECK (farmer_id <> reseller_id)
);

-- One listing's committed sum and party lookup.
CREATE INDEX listing_transactions_listing_idx ON listing_transactions (listing_id);

-- Consumer inbox: event ids already processed, written in the transaction
-- that applies the event (de-duplication on EventMetadata.event_id). Rows are
-- purged after CATALOG_INBOX_RETENTION; a replay after that is still harmless,
-- because listing_transactions keys commitments by transaction id.
CREATE TABLE processed_events (
    event_id          uuid        PRIMARY KEY,
    topic             text        NOT NULL,
    event_type        text        NOT NULL,
    aggregate_id      uuid        NOT NULL,
    aggregate_version bigint      NOT NULL,
    processed_at      timestamptz NOT NULL
);

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

-- +goose Down

-- Drops the projection and the inbox only: listings keep the remaining
-- quantities and SOLD_OUT statuses the commitments produced. Migrating up
-- again starts an empty projection; reset the consumer group to the start of
-- the topic to rebuild it from the events still retained there.
DROP TABLE processed_events;
DROP TABLE listing_transactions;
