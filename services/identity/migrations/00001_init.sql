-- identity: accounts, roles, e-ijara farmer verifications and the event outbox.
--
-- Enums are readable text with CHECK constraints; the repository maps them to
-- the proto enums (value names without the enum prefix). Timestamps are
-- timestamptz (UTC); lease dates are civil dates in Asia/Tashkent.

-- +goose Up

CREATE TABLE accounts (
    id                uuid        PRIMARY KEY,
    status            text        NOT NULL CHECK (status IN ('ACTIVE', 'BLOCKED', 'DELETED')),
    plan              text        NOT NULL CHECK (plan IN ('DEFAULT', 'PREMIUM')),
    premium_until     timestamptz,
    -- Optimistic-concurrency version; bumped on every change (roles included).
    version           bigint      NOT NULL DEFAULT 1 CHECK (version >= 1),
    -- CreateAccount idempotency key from auth-gateway.
    create_request_id uuid        NOT NULL,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    CONSTRAINT accounts_create_request_id_key UNIQUE (create_request_id)
);

CREATE TABLE account_roles (
    account_id uuid        NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    role       text        NOT NULL CHECK (role IN ('RESELLER', 'FARMER', 'ADMIN')),
    granted_at timestamptz NOT NULL,
    PRIMARY KEY (account_id, role)
);

CREATE TABLE farmer_verifications (
    id                    uuid        PRIMARY KEY,
    user_id               uuid        NOT NULL REFERENCES accounts (id),
    -- SubmitFarmerVerification idempotency key, unique per user.
    request_id            uuid        NOT NULL,
    -- Normalized (trimmed, upper case).
    lease_contract_number text        NOT NULL CHECK (char_length(lease_contract_number) BETWEEN 3 AND 64),
    status                text        NOT NULL CHECK (status IN ('PENDING', 'VERIFIED', 'REJECTED', 'EXPIRED')),
    rejection_reason      text        CHECK (rejection_reason IN ('LEASE_CONTRACT_INVALID', 'LEASE_CONTRACT_ALREADY_CLAIMED')),
    -- The lease as e-ijara returned it (VERIFIED and EXPIRED).
    lease_valid_from      date,
    lease_valid_until     date,
    -- Background e-ijara retries of PENDING verifications.
    attempts              integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at       timestamptz NOT NULL,
    last_error            text,
    submitted_at          timestamptz NOT NULL,
    decided_at            timestamptz,
    expired_at            timestamptz,
    updated_at            timestamptz NOT NULL,
    CONSTRAINT farmer_verifications_user_request_key UNIQUE (user_id, request_id),
    CONSTRAINT farmer_verifications_rejection_check
        CHECK ((status = 'REJECTED') = (rejection_reason IS NOT NULL)),
    CONSTRAINT farmer_verifications_decided_check
        CHECK ((status = 'PENDING') = (decided_at IS NULL)),
    CONSTRAINT farmer_verifications_lease_check
        CHECK (status NOT IN ('VERIFIED', 'EXPIRED')
               OR (lease_valid_from IS NOT NULL AND lease_valid_until IS NOT NULL
                   AND lease_valid_from <= lease_valid_until))
);

-- A lease contract can back at most one VERIFIED verification at a time. This
-- is the race-safe guard behind LEASE_CONTRACT_ALREADY_CLAIMED; the repository
-- recognizes it by name.
CREATE UNIQUE INDEX farmer_verifications_verified_contract_key
    ON farmer_verifications (lease_contract_number)
    WHERE status = 'VERIFIED';

-- Latest verification per user (GetFarmerVerification, Account.farmer_verification_status).
CREATE INDEX farmer_verifications_user_latest_idx
    ON farmer_verifications (user_id, submitted_at DESC, id DESC);

-- Worker: PENDING retries and VERIFIED expiry.
CREATE INDEX farmer_verifications_pending_idx
    ON farmer_verifications (next_attempt_at)
    WHERE status = 'PENDING';
CREATE INDEX farmer_verifications_verified_until_idx
    ON farmer_verifications (lease_valid_until)
    WHERE status = 'VERIFIED';

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
DROP TABLE farmer_verifications;
DROP TABLE account_roles;
DROP TABLE accounts;
