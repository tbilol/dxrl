-- Reverts db/migration/V1__init.sql. Applied by `order-escrow migrate down`
-- (make migrate-down) together with removing version 1 from
-- flyway_schema_history, in one transaction. Destroys every transaction:
-- development databases only.

DROP TABLE outbox;
DROP TABLE processed_requests;
DROP TABLE weighbridge_receipts;
DROP TABLE payouts;
DROP TABLE payments;
DROP TABLE transaction_state_changes;
DROP TABLE escrow_transactions;
