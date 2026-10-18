-- profile-social: profiles and follows only. AGROGRAM no longer has escrow
-- transactions (nothing to rate, no deals to count), a Premium plan or
-- farmer verification, so ratings, the completed transactions that unlocked
-- them, their counters and the Premium / verified-farmer badges go. ADMIN is
-- the only role left in the account projection.

-- +goose Up

DROP TABLE ratings;
DROP TABLE completed_transactions;

ALTER TABLE profiles
    DROP COLUMN rating_count,
    DROP COLUMN rating_sum,
    DROP COLUMN deals_count,
    DROP COLUMN is_verified_farmer,
    DROP COLUMN is_premium,
    DROP COLUMN premium_until;

UPDATE profiles SET roles = array_remove(array_remove(roles, 'RESELLER'), 'FARMER')
WHERE roles && ARRAY['RESELLER', 'FARMER']::text[];
ALTER TABLE profiles DROP CONSTRAINT profiles_roles_check;
ALTER TABLE profiles ADD CONSTRAINT profiles_roles_check CHECK (roles <@ ARRAY['ADMIN']::text[]);

-- +goose Down

-- Restores the 00001 schema, empty: no ratings, no deals, no badges, and the
-- removed roles are not restored (the next account event of each user
-- brings back whatever identity then holds).
ALTER TABLE profiles DROP CONSTRAINT profiles_roles_check;
ALTER TABLE profiles ADD CONSTRAINT profiles_roles_check
    CHECK (roles <@ ARRAY['RESELLER', 'FARMER', 'ADMIN']::text[]);

ALTER TABLE profiles
    ADD COLUMN rating_count       bigint      NOT NULL DEFAULT 0 CHECK (rating_count >= 0),
    ADD COLUMN rating_sum         bigint      NOT NULL DEFAULT 0 CHECK (rating_sum >= 0),
    ADD COLUMN deals_count        bigint      NOT NULL DEFAULT 0 CHECK (deals_count >= 0),
    ADD COLUMN is_verified_farmer boolean     NOT NULL DEFAULT false,
    ADD COLUMN is_premium         boolean     NOT NULL DEFAULT false,
    ADD COLUMN premium_until      timestamptz;

CREATE TABLE completed_transactions (
    id           uuid        PRIMARY KEY,
    farmer_id    uuid        NOT NULL,
    reseller_id  uuid        NOT NULL,
    completed_at timestamptz NOT NULL,
    recorded_at  timestamptz NOT NULL,
    CONSTRAINT completed_transactions_distinct_parties CHECK (farmer_id <> reseller_id)
);

CREATE TABLE ratings (
    id             uuid        PRIMARY KEY,
    transaction_id uuid        NOT NULL REFERENCES completed_transactions (id),
    rater_id       uuid        NOT NULL,
    rated_user_id  uuid        NOT NULL REFERENCES profiles (id),
    request_id     uuid        NOT NULL,
    score          smallint    NOT NULL CHECK (score BETWEEN 1 AND 5),
    comment        text        NOT NULL DEFAULT '' CHECK (char_length(comment) <= 500),
    created_at     timestamptz NOT NULL,
    CONSTRAINT ratings_transaction_rater_key UNIQUE (transaction_id, rater_id),
    CONSTRAINT ratings_rater_request_key UNIQUE (rater_id, request_id),
    CONSTRAINT ratings_no_self_rating CHECK (rater_id <> rated_user_id)
);
CREATE INDEX ratings_rated_user_created_idx ON ratings (rated_user_id, created_at DESC, id DESC);
