package uz.agrogram.orderescrow.store;

import java.util.Collection;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.StateChange;
import uz.agrogram.orderescrow.events.OutboxMessage;

/** The queries available inside one database transaction ({@link EscrowStore#inTransaction}). */
public interface EscrowTx {

    // ---- Idempotency ------------------------------------------------------------

    Optional<ProcessedRequest> findProcessedRequest(UUID callerId, UUID requestId);

    /** @throws ConflictException when (caller, request_id) is already recorded */
    void insertProcessedRequest(ProcessedRequest request);

    // ---- Creation -----------------------------------------------------------------

    /**
     * Serializes transaction creation on a listing until this database
     * transaction ends (pg_advisory_xact_lock), so the open-transaction lookup,
     * the committed-quantity sum and the insert of concurrent creations cannot
     * interleave. Reads after the lock see everything committed before it.
     */
    void lockListing(UUID listingId);

    /** The initiator's open (not final) transaction on the listing, if any. */
    Optional<EscrowTransaction> findOpenByInitiator(UUID listingId, UUID initiatorId);

    /**
     * Grams of the listing committed to this service's transactions: every
     * transaction except cancelled ones (there are none yet, TODO(cancel)),
     * including completed ones, which are sold.
     */
    long committedGrams(UUID listingId);

    /**
     * Inserts a new transaction with its payment, receipt and payout records.
     *
     * @throws ConflictException when the initiator already has an open
     *     transaction on the listing (partial unique index)
     */
    void insertTransaction(EscrowTransaction transaction);

    // ---- Steps ----------------------------------------------------------------------

    /**
     * Loads a transaction with its payment, receipt and payout. With
     * {@code forUpdate} the row stays locked until the database transaction
     * ends, which serializes every command on the transaction.
     */
    Optional<EscrowTransaction> findTransaction(UUID id, boolean forUpdate);

    /**
     * Stores {@code after}, the result of one or more steps on {@code before}:
     * updates the row only if it is still at {@code before.version()}
     * (optimistic concurrency) and inserts the payment, receipt and payout
     * records {@code after} has and {@code before} had not.
     *
     * @throws ConflictException when the stored version is not
     *     {@code before.version()}, or a record already exists (at most one
     *     payment, receipt and payout per transaction)
     */
    void updateTransaction(EscrowTransaction before, EscrowTransaction after);

    void appendStateChange(StateChange change);

    /** The transaction's history, oldest first. */
    List<StateChange> history(UUID transactionId);

    /** The histories of several transactions, oldest first each. */
    Map<UUID, List<StateChange>> histories(Collection<UUID> transactionIds);

    List<EscrowTransaction> listTransactions(ListQuery query);

    // ---- Outbox -----------------------------------------------------------------

    /** Writes an event to the outbox in this database transaction; the relay publishes it. */
    void enqueueOutbox(OutboxMessage message);
}
