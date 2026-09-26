package uz.agrogram.orderescrow.store;

import java.util.function.Function;

/**
 * The transaction repository. All reads and writes of a unit of work happen
 * inside {@link #inTransaction}, in one READ COMMITTED database transaction:
 * the state change, the payment/payout/receipt records, the history, the outbox
 * events and the idempotency record commit together or not at all.
 *
 * <p>Implementations: JdbcEscrowStore (Postgres) and, in the unit tests, an
 * in-memory store with the same semantics.
 */
public interface EscrowStore {

    /**
     * Runs {@code work} in one database transaction. It commits when
     * {@code work} returns and rolls back when it throws.
     *
     * @throws ConflictException when a concurrent transaction won a race (a
     *     unique key, a stale version, a serialization failure or a deadlock);
     *     the unit of work may be retried from the start
     */
    <T> T inTransaction(Function<EscrowTx, T> work);
}
