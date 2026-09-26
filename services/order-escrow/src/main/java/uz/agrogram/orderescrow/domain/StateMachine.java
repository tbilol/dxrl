package uz.agrogram.orderescrow.domain;

import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.orderescrow.error.AppException;

/**
 * The escrow state machine: the only place that decides whether a step is
 * allowed. The table is {@link Command}; everything not in it is rejected.
 *
 * <p>The actor is checked before the state, so the wrong party learns nothing
 * about the state: NOT_TRANSACTION_PARTY (PERMISSION_DENIED). The right party in
 * the wrong state gets INVALID_STATE_TRANSITION (FAILED_PRECONDITION) with
 * metadata {"state", "command"} (error.proto). Callers that are neither a party
 * nor staff never get here: the service answers NOT_FOUND so as not to reveal
 * that the transaction exists.
 */
public final class StateMachine {

    /** Who attempts a step on a transaction they can see. */
    public enum StepActor {
        FARMER,
        RESELLER,
        /** ROLE_ADMIN: can read every transaction, cannot act on one (TODO(dispute)). */
        STAFF,
        /** order-escrow itself (system steps). */
        SYSTEM;

        public static StepActor of(Party party) {
            return party == Party.FARMER ? FARMER : RESELLER;
        }
    }

    private StateMachine() {}

    /**
     * Checks that {@code actor} may take {@code command} on a transaction in
     * {@code current} and returns the state it leads to.
     *
     * @throws AppException NOT_TRANSACTION_PARTY or INVALID_STATE_TRANSITION
     */
    public static EscrowState next(Command command, EscrowState current, StepActor actor) {
        boolean allowedActor = command.isSystemStep()
                ? actor == StepActor.SYSTEM
                : command.party().map(StepActor::of).orElseThrow() == actor;
        if (!allowedActor) {
            String who = command.isSystemStep() ? "order-escrow" : "the " + command.party().orElseThrow()
                    .name().toLowerCase(java.util.Locale.ROOT);
            throw AppException.of(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY,
                    command.rpcName() + " can only be done by " + who + " of the transaction",
                    "command", command.rpcName());
        }
        if (current != command.from()) {
            throw AppException.of(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION,
                    command.rpcName() + " is not allowed in state " + current.name()
                            + " (it needs " + command.from().name() + ")",
                    "state", current.name(), "command", command.rpcName());
        }
        return command.to();
    }
}
