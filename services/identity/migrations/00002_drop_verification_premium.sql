-- identity: accounts and roles only. AGROGRAM has one flat tier (no plan,
-- no Premium) and no e-ijara farmer verification, so the plan columns and
-- farmer_verifications go. Any signed-in user may post on either board, so
-- RESELLER and FARMER carry nothing any more: ADMIN (staff) is the only role.
--
-- The removed role rows bump no version and publish no AccountRoleChanged:
-- the consumers of the account stream drop those roles themselves.

-- +goose Up

DROP TABLE farmer_verifications;

ALTER TABLE accounts
    DROP COLUMN plan,
    DROP COLUMN premium_until;

DELETE FROM account_roles WHERE role IN ('RESELLER', 'FARMER');
ALTER TABLE account_roles DROP CONSTRAINT account_roles_role_check;
ALTER TABLE account_roles ADD CONSTRAINT account_roles_role_check CHECK (role IN ('ADMIN'));

-- +goose Down

-- Restores the 00001 schema, empty: every account is on the DEFAULT plan,
-- nobody is verified, and the deleted RESELLER / FARMER rows are not
-- restored.
ALTER TABLE account_roles DROP CONSTRAINT account_roles_role_check;
ALTER TABLE account_roles ADD CONSTRAINT account_roles_role_check
    CHECK (role IN ('RESELLER', 'FARMER', 'ADMIN'));

ALTER TABLE accounts
    ADD COLUMN plan          text NOT NULL DEFAULT 'DEFAULT' CHECK (plan IN ('DEFAULT', 'PREMIUM')),
    ADD COLUMN premium_until timestamptz;
ALTER TABLE accounts ALTER COLUMN plan DROP DEFAULT;

CREATE TABLE farmer_verifications (
    id                    uuid        PRIMARY KEY,
    user_id               uuid        NOT NULL REFERENCES accounts (id),
    request_id            uuid        NOT NULL,
    lease_contract_number text        NOT NULL CHECK (char_length(lease_contract_number) BETWEEN 3 AND 64),
    status                text        NOT NULL CHECK (status IN ('PENDING', 'VERIFIED', 'REJECTED', 'EXPIRED')),
    rejection_reason      text        CHECK (rejection_reason IN ('LEASE_CONTRACT_INVALID', 'LEASE_CONTRACT_ALREADY_CLAIMED')),
    lease_valid_from      date,
    lease_valid_until     date,
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
CREATE UNIQUE INDEX farmer_verifications_verified_contract_key
    ON farmer_verifications (lease_contract_number)
    WHERE status = 'VERIFIED';
CREATE INDEX farmer_verifications_user_latest_idx
    ON farmer_verifications (user_id, submitted_at DESC, id DESC);
CREATE INDEX farmer_verifications_pending_idx
    ON farmer_verifications (next_attempt_at)
    WHERE status = 'PENDING';
CREATE INDEX farmer_verifications_verified_until_idx
    ON farmer_verifications (lease_valid_until)
    WHERE status = 'VERIFIED';
