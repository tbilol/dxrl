package uz.agrogram.orderescrow.domain;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import io.grpc.Status;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.orderescrow.domain.StateMachine.StepActor;
import uz.agrogram.orderescrow.error.AppException;

/**
 * Every state x every step x every actor. The expected table is written out
 * here independently of {@link Command} (transaction.proto's event table).
 */
class StateMachineTest {

    /** The only allowed (step, state, actor) triples and the state each leads to. */
    private static final List<Object[]> ALLOWED = List.of(
            new Object[] {Command.PAY_IN, EscrowState.AWAITING_PAYMENT, StepActor.RESELLER, EscrowState.PAYMENT_HELD},
            new Object[] {Command.SUBMIT_WEIGHBRIDGE_RECEIPT, EscrowState.PAYMENT_HELD, StepActor.RESELLER,
                EscrowState.RECEIPT_SUBMITTED},
            new Object[] {Command.CONFIRM_HANDOVER, EscrowState.RECEIPT_SUBMITTED, StepActor.FARMER,
                EscrowState.HANDED_OVER},
            new Object[] {Command.CONFIRM_RECEIPT, EscrowState.HANDED_OVER, StepActor.RESELLER, EscrowState.RECEIVED},
            new Object[] {Command.RELEASE_PAYOUT, EscrowState.RECEIVED, StepActor.SYSTEM, EscrowState.COMPLETED});

    /** Who takes each step (the RPC comments in order_escrow.proto). */
    private static final Map<Command, StepActor> ACTOR = Map.of(
            Command.PAY_IN, StepActor.RESELLER,
            Command.SUBMIT_WEIGHBRIDGE_RECEIPT, StepActor.RESELLER,
            Command.CONFIRM_HANDOVER, StepActor.FARMER,
            Command.CONFIRM_RECEIPT, StepActor.RESELLER,
            Command.RELEASE_PAYOUT, StepActor.SYSTEM);

    static Stream<Arguments> everyCombination() {
        List<Arguments> out = new ArrayList<>();
        for (EscrowState state : EscrowState.values()) {
            for (Command command : Command.values()) {
                for (StepActor actor : StepActor.values()) {
                    out.add(Arguments.of(state, command, actor));
                }
            }
        }
        return out.stream();
    }

    @ParameterizedTest(name = "{1} by {2} in {0}")
    @MethodSource("everyCombination")
    void onlyTheTableIsAllowed(EscrowState state, Command command, StepActor actor) {
        EscrowState expected = null;
        for (Object[] a : ALLOWED) {
            if (a[0] == command && a[1] == state && a[2] == actor) {
                expected = (EscrowState) a[3];
            }
        }
        if (expected != null) {
            assertThat(StateMachine.next(command, state, actor)).isEqualTo(expected);
            return;
        }
        AppException e = catchApp(() -> StateMachine.next(command, state, actor));
        if (actor != ACTOR.get(command)) {
            assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);
            assertThat(e.code()).isEqualTo(Status.Code.PERMISSION_DENIED);
            assertThat(e.metadata()).containsEntry("command", command.rpcName());
        } else {
            assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
            assertThat(e.code()).isEqualTo(Status.Code.FAILED_PRECONDITION);
            assertThat(e.metadata()).containsEntry("state", state.name()).containsEntry("command", command.rpcName());
        }
    }

    @Test
    void completedIsFinal() {
        for (Command command : Command.values()) {
            for (StepActor actor : StepActor.values()) {
                assertThatThrownBy(() -> StateMachine.next(command, EscrowState.COMPLETED, actor))
                        .isInstanceOf(AppException.class);
            }
        }
        assertThat(EscrowState.COMPLETED.isOpen()).isFalse();
    }

    @Test
    void staffCannotActOnATransaction() {
        for (Command command : Command.values()) {
            AppException e = catchApp(() -> StateMachine.next(command, command.from(), StepActor.STAFF));
            assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);
        }
    }

    @Test
    void theHappyPathVisitsEveryStateInOrder() {
        EscrowState s = EscrowState.AWAITING_PAYMENT;
        List<EscrowState> visited = new ArrayList<>(List.of(s));
        for (Object[] step : ALLOWED) {
            s = StateMachine.next((Command) step[0], s, (StepActor) step[2]);
            visited.add(s);
        }
        assertThat(visited).containsExactly(EscrowState.values());
    }

    @Test
    void statesMapOneToOneToTheContract() {
        for (EscrowState s : EscrowState.values()) {
            assertThat(EscrowState.fromProto(s.proto())).isEqualTo(s);
            assertThat(s.proto().name()).isEqualTo("TRANSACTION_STATE_" + s.name());
        }
        assertThatThrownBy(() -> EscrowState.fromProto(
                uz.agrogram.events.v1.TransactionState.TRANSACTION_STATE_UNSPECIFIED))
                .isInstanceOf(IllegalArgumentException.class);
    }

    private static AppException catchApp(Runnable r) {
        try {
            r.run();
        } catch (AppException e) {
            return e;
        }
        throw new AssertionError("expected an AppException");
    }
}
