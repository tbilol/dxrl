package uz.agrogram.orderescrow.domain;

import java.time.Instant;
import java.util.Objects;
import java.util.UUID;
import uz.agrogram.common.v1.Actor;

/**
 * One entry of a transaction's history (table transaction_state_changes): the
 * state entered at {@code version}, who caused it, and when.
 *
 * @param transactionId the transaction
 * @param version the transaction version the state was entered at
 * @param state the state entered
 * @param actor a party, or order-escrow for system steps
 * @param changedAt when it happened
 */
public record StateChange(UUID transactionId, long version, EscrowState state, Actor actor, Instant changedAt) {

    public StateChange {
        Objects.requireNonNull(transactionId, "transactionId");
        Objects.requireNonNull(state, "state");
        Objects.requireNonNull(actor, "actor");
        Objects.requireNonNull(changedAt, "changedAt");
        if (version < 1) {
            throw new IllegalArgumentException("version must be at least 1");
        }
    }
}
