package uz.agrogram.orderescrow.domain;

import java.util.Optional;

/**
 * A step of the escrow state machine: who may take it, from which state, and
 * the state it leads to (events/v1/transaction.proto, "emits its events in this
 * order"). {@link StateMachine} checks a step against a transaction.
 *
 * <pre>
 *   Command                     Actor          From               To                 Event
 *   PayIn                       reseller       AWAITING_PAYMENT   PAYMENT_HELD       PaymentHeld
 *   SubmitWeighbridgeReceipt    reseller       PAYMENT_HELD       RECEIPT_SUBMITTED  ReceiptSubmitted
 *   ConfirmHandover             farmer         RECEIPT_SUBMITTED  HANDED_OVER        ProductHandover
 *   ConfirmReceipt              reseller       HANDED_OVER        RECEIVED           ReceiptConfirmed
 *   (release payout)            order-escrow   RECEIVED           COMPLETED          PayoutReleased
 * </pre>
 *
 * CreateTransaction is not a step: it creates the transaction in
 * AWAITING_PAYMENT (TransactionCreated). BonusAwarded follows the payout on a
 * COMPLETED transaction without a state change.
 *
 * <p>TODO(cancel): no Cancel RPC or state in the contract. An unpaid transaction
 * therefore reserves its quantity on the listing for good; a cancel step (and
 * an expiry of unpaid transactions) belongs here.
 * TODO(refund): no refund step; held funds can only leave escrow as the payout.
 * TODO(dispute): no dispute step; staff (ROLE_ADMIN) can read transactions but
 * cannot act on them.
 */
public enum Command {
    PAY_IN("PayIn", Party.RESELLER, EscrowState.AWAITING_PAYMENT, EscrowState.PAYMENT_HELD),
    SUBMIT_WEIGHBRIDGE_RECEIPT("SubmitWeighbridgeReceipt", Party.RESELLER, EscrowState.PAYMENT_HELD,
            EscrowState.RECEIPT_SUBMITTED),
    CONFIRM_HANDOVER("ConfirmHandover", Party.FARMER, EscrowState.RECEIPT_SUBMITTED, EscrowState.HANDED_OVER),
    CONFIRM_RECEIPT("ConfirmReceipt", Party.RESELLER, EscrowState.HANDED_OVER, EscrowState.RECEIVED),
    /** System step: order-escrow releases the payout right after ConfirmReceipt. */
    RELEASE_PAYOUT("ReleasePayout", null, EscrowState.RECEIVED, EscrowState.COMPLETED);

    private final String rpcName;
    private final Party party;
    private final EscrowState from;
    private final EscrowState to;

    Command(String rpcName, Party party, EscrowState from, EscrowState to) {
        this.rpcName = rpcName;
        this.party = party;
        this.from = from;
        this.to = to;
    }

    /** The RPC name, used in errors ({"command": ...}) and the idempotency log. */
    public String rpcName() {
        return rpcName;
    }

    /** The party that takes the step; empty for system steps. */
    public Optional<Party> party() {
        return Optional.ofNullable(party);
    }

    public boolean isSystemStep() {
        return party == null;
    }

    public EscrowState from() {
        return from;
    }

    public EscrowState to() {
        return to;
    }
}
