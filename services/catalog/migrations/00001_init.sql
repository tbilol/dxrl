-- catalog: product reference data, the listings of both boards (farmers'
-- sell offers and resellers' buy-requests) and the event outbox.
--
-- Enums are readable text with CHECK constraints; the repository maps them to
-- the proto enums (value names without the enum prefix). Product types are
-- the exception: product_types.id IS the common.v1.ProductType number, the
-- stable id every service and event uses. Timestamps are timestamptz (UTC).
--
-- No extension is needed: search uses the built-in 'simple' text-search
-- configuration on a tsvector the service computes (see listings.search_document).
-- catalog keeps no profile data: a listing stores its owner's user id only.

-- +goose Up

-- Product reference data: one row per defined common.v1.ProductType value.
-- The proto fixes the list; rows are added by migrations only (00002 seeds
-- them), never renumbered or deleted. A product that is no longer offered is
-- retired: old listings keep its labels, new listings cannot use it.
CREATE TABLE product_types (
    id         integer PRIMARY KEY CHECK (id > 0), -- common.v1.ProductType number
    key        text    NOT NULL UNIQUE CHECK (key ~ '^[a-z]+(-[a-z]+)*$'),
    name_uz    text    NOT NULL CHECK (name_uz <> ''),
    name_ru    text    NOT NULL CHECK (name_ru <> ''),
    category   text    NOT NULL CHECK (category IN ('GRAIN_INDUSTRIAL', 'VEGETABLE', 'LEGUME', 'MELON',
                                                    'FRUIT', 'NUT')),
    sort_order integer NOT NULL,
    is_retired boolean NOT NULL DEFAULT false
);

-- Listings of both boards; `kind` tells them apart. Soft delete: DELETED rows
-- stay because transactions reference them.
CREATE TABLE listings (
    id                    uuid             PRIMARY KEY, -- UUIDv7
    kind                  text             NOT NULL CHECK (kind IN ('OFFER', 'BUY_REQUEST')),
    -- The user who posted it (farmer of an offer, reseller of a buy-request).
    -- Names, avatars and badges are profile-social's; the edge composes the
    -- owner card.
    owner_id              uuid             NOT NULL,
    product_type          integer          NOT NULL REFERENCES product_types (id),

    quantity_grams        bigint           NOT NULL CHECK (quantity_grams > 0),
    -- Display unit; NULL = unspecified.
    quantity_unit         text             CHECK (quantity_unit IN ('KILOGRAM', 'TONNE')),
    -- Quantity not yet committed to escrow transactions. TODO(phase3): equals
    -- quantity_grams until the transaction stream is consumed.
    remaining_grams       bigint           NOT NULL CHECK (remaining_grams BETWEEN 0 AND quantity_grams),

    -- Unit price in minor units (tiyin) per KILOGRAM or TONNE. All three NULL
    -- = negotiable ("kelishiladi").
    price_minor           bigint           CHECK (price_minor > 0),
    price_currency        text             CHECK (price_currency = 'UZS'),
    price_unit            text             CHECK (price_unit IN ('KILOGRAM', 'TONNE')),
    -- Price of one tonne (= price per kg x 1000): exact for both units, so it
    -- is the price filter and sort key. NULL = negotiable.
    price_per_tonne_minor bigint           GENERATED ALWAYS AS (
                                               CASE price_unit
                                                   WHEN 'KILOGRAM' THEN price_minor * 1000
                                                   WHEN 'TONNE' THEN price_minor
                                               END) STORED,

    -- Contact phone shown on the listing. Never published in events.
    phone                 text             NOT NULL CHECK (phone ~ '^\+998[0-9]{9}$'),
    region                text             NOT NULL CHECK (region IN ('TOSHKENT_SHAHRI', 'TOSHKENT_VILOYATI',
                                               'ANDIJON', 'FARGONA', 'NAMANGAN', 'SAMARQAND', 'BUXORO', 'NAVOIY',
                                               'QASHQADARYO', 'SURXONDARYO', 'JIZZAX', 'SIRDARYO', 'XORAZM',
                                               'QORAQALPOGISTON')),
    district              text             NOT NULL DEFAULT '' CHECK (char_length(district) <= 120),
    address_line          text             NOT NULL DEFAULT '' CHECK (char_length(address_line) <= 255),
    latitude              double precision CHECK (latitude BETWEEN -90 AND 90),
    longitude             double precision CHECK (longitude BETWEEN -180 AND 180),
    notes                 text             NOT NULL DEFAULT '' CHECK (char_length(notes) <= 1000),
    -- media-pipeline object ids in display order. TODO(media): type, status
    -- and URLs are media-pipeline's; only the ids are stored.
    media_ids             uuid[]           NOT NULL DEFAULT '{}' CHECK (cardinality(media_ids) <= 10),

    status                text             NOT NULL CHECK (status IN ('ACTIVE', 'CLOSED', 'SOLD_OUT', 'BLOCKED',
                                                                      'DELETED')),
    -- Opened by non-owners. Batched increments; does not bump version.
    views_count           bigint           NOT NULL DEFAULT 0 CHECK (views_count >= 0),
    -- Optimistic-concurrency version: 1 on creation, + 1 on every update and
    -- on deletion.
    version               bigint           NOT NULL CHECK (version >= 1),
    -- CreateListing idempotency key, unique per owner.
    create_request_id     uuid             NOT NULL,
    created_at            timestamptz      NOT NULL,
    updated_at            timestamptz      NOT NULL,
    deleted_at            timestamptz,

    -- What SearchListings matches: product names and key (weight A),
    -- district (B) and notes (C), normalized by the service (lower case,
    -- apostrophes removed) and parsed with the 'simple' configuration.
    -- Written with every insert and update of those fields.
    search_document       tsvector         NOT NULL,

    CONSTRAINT listings_owner_request_key UNIQUE (owner_id, create_request_id),
    CONSTRAINT listings_price_check CHECK (num_nulls(price_minor, price_currency, price_unit) IN (0, 3)),
    CONSTRAINT listings_location_check CHECK (num_nulls(latitude, longitude) IN (0, 2)),
    CONSTRAINT listings_deleted_check CHECK ((status = 'DELETED') = (deleted_at IS NOT NULL))
);

-- Board browsing, newest first (keyset created_at, id): ACTIVE listings
-- overall and per board, product and region.
CREATE INDEX listings_active_created_idx ON listings (created_at DESC, id DESC) WHERE status = 'ACTIVE';
CREATE INDEX listings_active_kind_created_idx ON listings (kind, created_at DESC, id DESC)
    WHERE status = 'ACTIVE';
CREATE INDEX listings_active_product_created_idx ON listings (product_type, created_at DESC, id DESC)
    WHERE status = 'ACTIVE';
CREATE INDEX listings_active_region_created_idx ON listings (region, created_at DESC, id DESC)
    WHERE status = 'ACTIVE';
-- One owner's listings in any status (profile-social's ListUserListings) and
-- the plan-quota count.
CREATE INDEX listings_owner_created_idx ON listings (owner_id, created_at DESC, id DESC);
-- Free-text search on the boards.
CREATE INDEX listings_search_idx ON listings USING gin (search_document) WHERE status = 'ACTIVE';

-- Transactional outbox: rows are written in the same transaction as the state
-- change and published to Kafka by the relay (at-least-once).
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

-- +goose Down

DROP TABLE outbox;
DROP TABLE listings;
DROP TABLE product_types;
