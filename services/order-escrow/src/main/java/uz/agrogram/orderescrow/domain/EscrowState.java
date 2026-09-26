package uz.agrogram.orderescrow.domain;

import uz.agrogram.events.v1.TransactionState;

/**
 * The step an escrow transaction is at (events/v1/transaction.proto,
 * TransactionState). Stored as the name in escrow_transactions.state.
 *
 * <p>TODO(cancel) / TODO(refund) / TODO(dispute): the contract has no
 * cancelled, refunded or disputed state yet. When it gets one, add it here as a
 * final state and exclude it from {@link #isOpen()} and from the committed
 * quantity (see EscrowTx#committedGrams).
 */
public enum EscrowState {
    AWAITING_PAYMENT(TransactionState.TRANSACTION_STATE_AWAITING_PAYMENT),
    PAYMENT_HELD(TransactionState.TRANSACTION_STATE_PAYMENT_HELD),
    RECEIPT_SUBMITTED(TransactionState.TRANSACTION_STATE_RECEIPT_SUBMITTED),
    HANDED_OVER(TransactionState.TRANSACTION_STATE_HANDED_OVER),
    RECEIVED(TransactionState.TRANSACTION_STATE_RECEIVED),
    COMPLETED(TransactionState.TRANSACTION_STATE_COMPLETED);

    private final TransactionState proto;

    EscrowState(TransactionState proto) {
        this.proto = proto;
    }

    public TransactionState proto() {
        return proto;
    }

    /** Not final yet. The partial unique index escrow_transactions_open_initiator_key uses the same rule. */
    public boolean isOpen() {
        return this != COMPLETED;
    }

    /** The state for a proto value; throws for UNSPECIFIED and unknown values. */
    public static EscrowState fromProto(TransactionState s) {
        for (EscrowState e : values()) {
            if (e.proto == s) {
                return e;
            }
        }
        throw new IllegalArgumentException("no escrow state for " + s);
    }
}
