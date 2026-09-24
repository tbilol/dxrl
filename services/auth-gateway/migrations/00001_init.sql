-- auth-gateway: sign-in methods (credentials), sessions, refresh tokens,
-- phone code challenges and the event outbox.
--
-- Accounts live in identity; the only account data here is the user id.
-- Enums are readable text with CHECK constraints; the repository maps them to
-- the proto enums (value names without the enum prefix). Timestamps are
-- timestamptz (UTC). Nothing reversible is stored: passwords are argon2id PHC
-- strings, refresh tokens and SMS codes only ever reach this schema as
-- SHA-256 digests.

-- +goose Up

-- One credential per (user, method), and a subject (Google/Apple `sub`, or the
-- phone number in E.164) belongs to at most one account. Both unique
-- constraints are the race-safe guards behind PROVIDER_ALREADY_LINKED and
-- PHONE_ALREADY_REGISTERED; the repository recognizes them by name.
CREATE TABLE sign_in_methods (
    id            uuid        PRIMARY KEY,
    user_id       uuid        NOT NULL,
    method        text        NOT NULL CHECK (method IN ('GOOGLE', 'APPLE', 'PHONE_PASSWORD')),
    -- Google/Apple `sub`, or the E.164 phone number.
    subject       text        NOT NULL CHECK (char_length(subject) BETWEEN 1 AND 255),
    -- argon2id PHC string; PHONE_PASSWORD only.
    password_hash text,
    -- Masked identifier for display, e.g. "a***@gmail.com".
    display_hint  text        NOT NULL DEFAULT '',
    linked_at     timestamptz NOT NULL,
    last_used_at  timestamptz,
    CONSTRAINT sign_in_methods_user_method_key UNIQUE (user_id, method),
    CONSTRAINT sign_in_methods_method_subject_key UNIQUE (method, subject),
    CONSTRAINT sign_in_methods_password_check
        CHECK ((method = 'PHONE_PASSWORD') = (password_hash IS NOT NULL))
);

CREATE INDEX sign_in_methods_user_idx ON sign_in_methods (user_id);

-- A session is one sign-in on one device: the family a refresh token chain
-- belongs to. Revoking it stops every token in the chain at once.
CREATE TABLE sessions (
    id             uuid        PRIMARY KEY,
    user_id        uuid        NOT NULL,
    method         text        NOT NULL CHECK (method IN ('GOOGLE', 'APPLE', 'PHONE_PASSWORD')),
    platform       text        NOT NULL CHECK (platform IN ('UNSPECIFIED', 'WEB', 'ANDROID', 'IOS')),
    device_id      text        NOT NULL DEFAULT '',
    device_name    text        NOT NULL DEFAULT '',
    app_version    text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    last_seen_at   timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    revoked_at     timestamptz,
    revoked_reason text        CHECK (revoked_reason IN ('SIGN_OUT', 'SIGN_OUT_ALL', 'PASSWORD_RESET', 'TOKEN_REUSE')),
    CONSTRAINT sessions_revoked_check CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL))
);

-- SignOut with scope ALL_SESSIONS, and "how many devices is this user on".
CREATE INDEX sessions_user_live_idx ON sessions (user_id) WHERE revoked_at IS NULL;
-- Housekeeping: expired sessions (see the TODO(cleanup) in the service).
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at) WHERE revoked_at IS NULL;

-- Refresh tokens are opaque and single-use. Only their SHA-256 digest is
-- stored, so a database dump cannot be replayed. Every refresh writes a new
-- row and marks the old one used; a second use of a used row is the reuse
-- that revokes the whole session.
CREATE TABLE refresh_tokens (
    id          uuid        PRIMARY KEY,
    session_id  uuid        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL CHECK (octet_length(token_hash) = 32),
    issued_at   timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    replaced_by uuid        REFERENCES refresh_tokens (id) ON DELETE SET NULL,
    CONSTRAINT refresh_tokens_hash_key UNIQUE (token_hash),
    CONSTRAINT refresh_tokens_replaced_check CHECK (replaced_by IS NULL OR used_at IS NOT NULL)
);

CREATE INDEX refresh_tokens_session_idx ON refresh_tokens (session_id);
CREATE INDEX refresh_tokens_expires_at_idx ON refresh_tokens (expires_at);

-- One-time SMS codes. The code itself is never stored: code_hash is
-- SHA-256(challenge id || code), so the random id salts it.
CREATE TABLE phone_challenges (
    id                  uuid        PRIMARY KEY,
    phone_number        text        NOT NULL CHECK (phone_number ~ '^\+998[0-9]{9}$'),
    purpose             text        NOT NULL CHECK (purpose IN ('SIGN_UP', 'RESET_PASSWORD', 'LINK_PHONE')),
    code_hash           bytea       NOT NULL CHECK (octet_length(code_hash) = 32),
    attempts            integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts        integer     NOT NULL CHECK (max_attempts >= 1),
    created_at          timestamptz NOT NULL,
    expires_at          timestamptz NOT NULL,
    resend_available_at timestamptz NOT NULL,
    consumed_at         timestamptz
);

-- The resend window and "the latest code for this number and purpose".
CREATE INDEX phone_challenges_latest_idx
    ON phone_challenges (phone_number, purpose, created_at DESC);
-- Housekeeping: expired, unconsumed challenges.
CREATE INDEX phone_challenges_expires_at_idx
    ON phone_challenges (expires_at) WHERE consumed_at IS NULL;

-- Transactional outbox: rows are written in the same transaction as the state
-- change and published to Kafka by the relay (at-least-once). Codes, tokens
-- and hashes never go in here.
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
DROP TABLE phone_challenges;
DROP TABLE refresh_tokens;
DROP TABLE sessions;
DROP TABLE sign_in_methods;
