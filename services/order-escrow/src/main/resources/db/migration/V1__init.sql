-- order-escrow: escrow transactions (tranzaksiya), their money records
-- (pay-in hold, payout), the weighbridge receipt, the state history, the
-- idempotency log and the event outbox.
--
-- Money is integer tiyin (bigint *_minor columns, currency UZS). Enums are
-- readable text with CHECK constraints (proto value names without the enum
-- prefix), except product and region, which store the common.v1.ProductType and
-- common.v1.Region numbers. Timestamps are timestamptz (UTC, microseconds).
--
-- Flyway OSS has no undo: the matching down script is db/down/V1.down.sql,
-- applied by `order-escrow migrate down` (make migrate-down).

-- One escrow transaction. Amounts are fixed at creation and never recomputed.
CREATE TABLE escrow_transactions (
    id                   uuid        PRIMARY KEY,
    listing_id           uuid        NOT NULL,
    listing_kind         text        NOT NULL CHECK (listing_kind IN ('OFFER', 'BUY_REQUEST')),
    listing_version      bigint      NOT NULL CHECK (listing_version >= 0), -- catalog version the terms came from
    farmer_id            uuid        NOT NULL,                              -- seller (dehqon)
    reseller_id          uuid        NOT NULL,                              -- buyer (olib sotuvchi)
    initiator_id         uuid        NOT NULL,                              -- who started it: the listing owner's counterparty
    product              integer     NOT NULL CHECK (product > 0),          -- common.v1.ProductType number
    quantity_grams       bigint      NOT NULL CHECK (quantity_grams > 0),
    quantity_unit        text        NOT NULL CHECK (quantity_unit IN ('UNSPECIFIED', 'KILOGRAM', 'TONNE')),
    unit_price_minor     bigint      NOT NULL CHECK (unit_price_minor > 0),
    unit_price_unit      text        NOT NULL CHECK (unit_price_unit IN ('KILOGRAM', 'TONNE')),
    currency_code        text        NOT NULL CHECK (currency_code = 'UZS'),
    subtotal_minor       bigint      NOT NULL CHECK (subtotal_minor > 0),
    tax_minor            bigint      NOT NULL CHECK (tax_minor >= 0),          -- TODO(tax): always 0
    platform_fee_minor   bigint      NOT NULL CHECK (platform_fee_minor >= 0), -- TODO(payments): always 0
    total_minor          bigint      NOT NULL,                                 -- what the reseller pays in
    payout_minor         bigint      NOT NULL,                                 -- what the farmer receives
    address_region       integer     NOT NULL CHECK (address_region > 0),      -- common.v1.Region number
    address_district     text        NOT NULL DEFAULT '',
    address_line         text        NOT NULL DEFAULT '',
    address_latitude     double precision,
    address_longitude    double precision,
    state                text        NOT NULL CHECK (state IN ('AWAITING_PAYMENT', 'PAYMENT_HELD', 'RECEIPT_SUBMITTED',
                                                               'HANDED_OVER', 'RECEIVED', 'COMPLETED')),
    bonus_points         bigint      NOT NULL DEFAULT 0 CHECK (bonus_points >= 0), -- TODO(bonus): placeholder
    version              bigint      NOT NULL CHECK (version >= 1),
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    payment_held_at      timestamptz,
    receipt_submitted_at timestamptz,
    handed_over_at       timestamptz,
    received_at          timestamptz,
    completed_at         timestamptz,
    CONSTRAINT escrow_transactions_distinct_parties CHECK (farmer_id <> reseller_id),
    CONSTRAINT escrow_transactions_initiator CHECK (
        (listing_kind = 'OFFER' AND initiator_id = reseller_id) OR
        (listing_kind = 'BUY_REQUEST' AND initiator_id = farmer_id)),
    -- transaction.proto: total = subtotal + tax + platform_fee, payout_amount =
    -- subtotal. Together: total = payout + tax + platform_fee, so every tiyin
    -- held leaves escrow exactly once. TODO(tax): if tax is withheld from the
    -- payout instead, change both formulas (the sum stays conserved).
    CONSTRAINT escrow_transactions_total CHECK (total_minor = subtotal_minor + tax_minor + platform_fee_minor),
    CONSTRAINT escrow_transactions_payout CHECK (payout_minor = subtotal_minor),
    CONSTRAINT escrow_transactions_location CHECK ((address_latitude IS NULL) = (address_longitude IS NULL)),
    CONSTRAINT escrow_transactions_paid CHECK ((state = 'AWAITING_PAYMENT') = (payment_held_at IS NULL)),
    CONSTRAINT escrow_transactions_completed CHECK ((state = 'COMPLETED') = (completed_at IS NOT NULL)),
    CONSTRAINT escrow_transactions_bonus CHECK (bonus_points = 0 OR state = 'COMPLETED')
);

-- "If the caller already has an open transaction on the listing, that one is
-- returned instead of a new one": at most one open transaction per initiator
-- and listing. Open = not final; keep the predicate in sync with
-- EscrowState.isOpen(). TODO(cancel): a CANCELLED state would be final too.
CREATE UNIQUE INDEX escrow_transactions_open_initiator_key
    ON escrow_transactions (listing_id, initiator_id) WHERE state <> 'COMPLETED';

-- The committed quantity of a listing (oversell guard).
CREATE INDEX escrow_transactions_listing_idx ON escrow_transactions (listing_id);

-- ListTransactions, newest first, per side.
CREATE INDEX escrow_transactions_farmer_idx ON escrow_transactions (farmer_id, created_at DESC, id DESC);
CREATE INDEX escrow_transactions_reseller_idx ON escrow_transactions (reseller_id, created_at DESC, id DESC);

-- Every state a transaction entered (Transaction.history), keyed by the
-- transaction version it was entered at.
CREATE TABLE transaction_state_changes (
    transaction_id uuid        NOT NULL REFERENCES escrow_transactions (id),
    version        bigint      NOT NULL CHECK (version >= 1),
    state          text        NOT NULL CHECK (state IN ('AWAITING_PAYMENT', 'PAYMENT_HELD', 'RECEIPT_SUBMITTED',
                                                         'HANDED_OVER', 'RECEIVED', 'COMPLETED')),
    actor_user_id  uuid,        -- a party
    actor_service  text,        -- or a service acting on its own ("order-escrow")
    changed_at     timestamptz NOT NULL,
    PRIMARY KEY (transaction_id, version),
    CONSTRAINT transaction_state_changes_actor CHECK ((actor_user_id IS NULL) <> (actor_service IS NULL))
);

-- The reseller's pay-in, held in escrow. One per transaction.
-- STUB: TODO(payments) provider 'stub', no provider id, no money moves.
CREATE TABLE payments (
    id                  uuid        PRIMARY KEY,
    transaction_id      uuid        NOT NULL UNIQUE REFERENCES escrow_transactions (id),
    provider            text        NOT NULL CHECK (provider <> ''),
    provider_payment_id text        NOT NULL DEFAULT '',
    amount_minor        bigint      NOT NULL CHECK (amount_minor > 0),  -- = escrow_transactions.total_minor
    currency_code       text        NOT NULL CHECK (currency_code = 'UZS'),
    held_at             timestamptz NOT NULL,
    released_at         timestamptz,                                     -- set by the payout; TODO(refund)
    CONSTRAINT payments_released_after_held CHECK (released_at IS NULL OR released_at >= held_at)
);

-- The release of the held funds to the farmer. Exactly one per transaction and
-- per payment: a transaction can never be paid out twice.
-- STUB: TODO(payments) provider 'stub', no provider id, no money moves.
CREATE TABLE payouts (
    id                 uuid        PRIMARY KEY,
    transaction_id     uuid        NOT NULL UNIQUE REFERENCES escrow_transactions (id),
    payment_id         uuid        NOT NULL UNIQUE REFERENCES payments (id),
    recipient_id       uuid        NOT NULL,                               -- the farmer
    provider           text        NOT NULL CHECK (provider <> ''),
    provider_payout_id text        NOT NULL DEFAULT '',
    amount_minor       bigint      NOT NULL CHECK (amount_minor > 0),      -- = escrow_transactions.payout_minor
    currency_code      text        NOT NULL CHECK (currency_code = 'UZS'),
    released_at        timestamptz NOT NULL
);

-- The weighbridge receipt (tarozi cheki). One per transaction.
-- STUB: TODO(weighbridge) accepted as VERIFIED on submission.
CREATE TABLE weighbridge_receipts (
    transaction_id      uuid        PRIMARY KEY REFERENCES escrow_transactions (id),
    ticket_number       text        NOT NULL DEFAULT '' CHECK (char_length(ticket_number) <= 64),
    photo_media_id      uuid,       -- media-pipeline upload (TODO(media): not checked)
    net_mass_grams      bigint      CHECK (net_mass_grams > 0),
    net_mass_unit       text        NOT NULL DEFAULT 'UNSPECIFIED'
                                    CHECK (net_mass_unit IN ('UNSPECIFIED', 'KILOGRAM', 'TONNE')),
    verification_status text        NOT NULL CHECK (verification_status IN ('PENDING', 'VERIFIED', 'REJECTED')),
    submitted_by        uuid        NOT NULL,
    submitted_at        timestamptz NOT NULL,
    verified_at         timestamptz,
    CONSTRAINT weighbridge_receipts_ticket_or_photo CHECK (ticket_number <> '' OR photo_media_id IS NOT NULL)
);

-- Idempotency log: a command's (caller, request_id), what it was used for,
-- a SHA-256 of the request without its request_id, and the response
-- returned. Written in the same transaction as the command's changes.
CREATE TABLE processed_requests (
    caller_id      uuid        NOT NULL,
    request_id     uuid        NOT NULL,
    operation      text        NOT NULL,
    transaction_id uuid        NOT NULL REFERENCES escrow_transactions (id),
    request_hash   bytea       NOT NULL,
    response       bytea       NOT NULL,
    created_at     timestamptz NOT NULL,
    PRIMARY KEY (caller_id, request_id)
);

CREATE INDEX processed_requests_transaction_idx ON processed_requests (transaction_id);

-- Transactional outbox: rows are written in the same transaction as the state
-- change and published to Kafka by the relay (at-least-once). The same table
-- as the Go services' outbox.
CREATE TABLE outbox (
    id           uuid        PRIMARY KEY, -- EventMetadata.event_id
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    topic        text        NOT NULL,
    record_key   text        NOT NULL,
    payload      bytea       NOT NULL,
    headers      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    event_type   text        NOT NULL,
    created_at   timestamptz NOT NULL,
    published_at timestamptz,
    attempts     integer     NOT NULL DEFAULT 0,
    last_error   text,
    CONSTRAINT outbox_seq_key UNIQUE (seq)
);

CREATE INDEX outbox_unpublished_idx ON outbox (seq) WHERE published_at IS NULL;
CREATE INDEX outbox_published_at_idx ON outbox (published_at) WHERE published_at IS NOT NULL;
