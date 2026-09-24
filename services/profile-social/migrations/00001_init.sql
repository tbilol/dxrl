-- profile-social: public profiles (with the account projection mirrored from
-- identity's account stream), follow edges, ratings and the completed
-- transactions that unlock them, the consumer inbox and the event outbox.
--
-- Enums are readable text with CHECK constraints; the repository maps them to
-- the proto enums (value names without the enum prefix). Timestamps are
-- timestamptz (UTC).

-- +goose Up

-- One row per user id. A row appears when the first account event arrives
-- (status PENDING), when the user calls CreateProfile (ACTIVE), or when the
-- user receives a rating before either. Only ACTIVE profiles are visible
-- through the API; DELETED is the soft delete (fields, ratings and history
-- stay, follow edges are removed).
CREATE TABLE profiles (
    id                 uuid        PRIMARY KEY, -- the user id
    status             text        NOT NULL CHECK (status IN ('PENDING', 'ACTIVE', 'DELETED')),

    -- Public fields, owned by the user (CreateProfile / UpdateProfile).
    display_name       text        CHECK (char_length(display_name) BETWEEN 2 AND 80),
    avatar_media_id    uuid,
    bio                text        NOT NULL DEFAULT '' CHECK (char_length(bio) <= 500),
    region             text        CHECK (region IN ('TOSHKENT_SHAHRI', 'TOSHKENT_VILOYATI', 'ANDIJON',
                                       'FARGONA', 'NAMANGAN', 'SAMARQAND', 'BUXORO', 'NAVOIY', 'QASHQADARYO',
                                       'SURXONDARYO', 'JIZZAX', 'SIRDARYO', 'XORAZM', 'QORAQALPOGISTON')),
    district           text        NOT NULL DEFAULT '' CHECK (char_length(district) <= 120),
    -- CreateProfile idempotency key of the current creation.
    create_request_id  uuid,
    -- Optimistic-concurrency version of the profile: 0 until the user creates
    -- it; + 1 on every create, update, delete and received rating. Counters
    -- and the account projection do not bump it.
    version            bigint      NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at         timestamptz, -- set by CreateProfile
    updated_at         timestamptz NOT NULL,
    deleted_at         timestamptz,

    -- Counters, changed in the transactions that change what they count.
    follower_count     bigint      NOT NULL DEFAULT 0 CHECK (follower_count >= 0),
    following_count    bigint      NOT NULL DEFAULT 0 CHECK (following_count >= 0),
    rating_count       bigint      NOT NULL DEFAULT 0 CHECK (rating_count >= 0),
    rating_sum         bigint      NOT NULL DEFAULT 0 CHECK (rating_sum >= 0),
    -- Completed escrow transactions. TODO(phase3): stays 0 until the
    -- transaction stream consumer records completed transactions.
    deals_count        bigint      NOT NULL DEFAULT 0 CHECK (deals_count >= 0),

    -- Account projection: the AccountSnapshot of agrogram.identity.account.v1,
    -- applied only when its version is newer than account_version (0 = no
    -- account event applied yet).
    roles              text[]      NOT NULL DEFAULT '{}'
                                   CHECK (roles <@ ARRAY['RESELLER', 'FARMER', 'ADMIN']::text[]),
    is_verified_farmer boolean     NOT NULL DEFAULT false,
    is_premium         boolean     NOT NULL DEFAULT false,
    premium_until      timestamptz,
    account_version    bigint      NOT NULL DEFAULT 0 CHECK (account_version >= 0),
    account_synced_at  timestamptz,

    CONSTRAINT profiles_created_check
        CHECK (status = 'PENDING' OR (display_name IS NOT NULL AND created_at IS NOT NULL AND version >= 1)),
    CONSTRAINT profiles_deleted_check
        CHECK ((status = 'DELETED') = (deleted_at IS NOT NULL))
);

-- Follow edges. Both ends are ACTIVE profiles: FollowUser checks it under the
-- rows' locks and DeleteProfile removes a deleted user's edges (adjusting the
-- other side's counters) in its own transaction.
CREATE TABLE follows (
    follower_id uuid        NOT NULL REFERENCES profiles (id),
    followee_id uuid        NOT NULL REFERENCES profiles (id),
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT follows_no_self_follow CHECK (follower_id <> followee_id)
);

-- ListFollowers / ListFollowing: newest first, keyset (created_at, other id).
CREATE INDEX follows_followee_created_idx ON follows (followee_id, created_at DESC, follower_id DESC);
CREATE INDEX follows_follower_created_idx ON follows (follower_id, created_at DESC, followee_id DESC);

-- Completed escrow transactions between two parties; each unlocks one rating
-- per party. TODO(phase3): written by the consumer of order-escrow's
-- transaction stream (not built yet); until then the table is empty and
-- RateUser answers RATING_NOT_ALLOWED.
CREATE TABLE completed_transactions (
    id           uuid        PRIMARY KEY, -- transaction id
    farmer_id    uuid        NOT NULL,
    reseller_id  uuid        NOT NULL,
    completed_at timestamptz NOT NULL,
    recorded_at  timestamptz NOT NULL,
    CONSTRAINT completed_transactions_distinct_parties CHECK (farmer_id <> reseller_id)
);

-- Ratings: one per (transaction, rater). The aggregate lives on profiles
-- (rating_sum, rating_count) and changes in the same transaction.
CREATE TABLE ratings (
    id             uuid        PRIMARY KEY,
    transaction_id uuid        NOT NULL REFERENCES completed_transactions (id),
    rater_id       uuid        NOT NULL,
    rated_user_id  uuid        NOT NULL REFERENCES profiles (id),
    -- RateUser idempotency key, unique per rater.
    request_id     uuid        NOT NULL,
    score          smallint    NOT NULL CHECK (score BETWEEN 1 AND 5),
    comment        text        NOT NULL DEFAULT '' CHECK (char_length(comment) <= 500),
    created_at     timestamptz NOT NULL,
    CONSTRAINT ratings_transaction_rater_key UNIQUE (transaction_id, rater_id),
    CONSTRAINT ratings_rater_request_key UNIQUE (rater_id, request_id),
    CONSTRAINT ratings_no_self_rating CHECK (rater_id <> rated_user_id)
);

-- ListRatings: newest first, keyset (created_at, id).
CREATE INDEX ratings_rated_user_created_idx ON ratings (rated_user_id, created_at DESC, id DESC);

-- Consumer inbox: event ids already processed, written in the transaction
-- that applies the event (de-duplication on EventMetadata.event_id).
CREATE TABLE processed_events (
    event_id          uuid        PRIMARY KEY,
    topic             text        NOT NULL,
    event_type        text        NOT NULL,
    aggregate_id      uuid        NOT NULL,
    aggregate_version bigint      NOT NULL,
    processed_at      timestamptz NOT NULL
);

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

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
DROP TABLE processed_events;
DROP TABLE ratings;
DROP TABLE completed_transactions;
DROP TABLE follows;
DROP TABLE profiles;
