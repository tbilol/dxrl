package uz.agrogram.orderescrow.service;

import static org.assertj.core.api.Assertions.assertThat;
import static uz.agrogram.orderescrow.testing.Fixtures.id;
import static uz.agrogram.orderescrow.testing.Fixtures.rid;
import static uz.agrogram.orderescrow.testing.Fixtures.tonnes;

import com.google.protobuf.InvalidProtocolBufferException;
import io.grpc.Status;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.Callable;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import org.junit.jupiter.api.Test;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.Mass;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.Role;
import uz.agrogram.events.v1.ReceiptVerificationStatus;
import uz.agrogram.events.v1.TransactionEvent;
import uz.agrogram.events.v1.TransactionState;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.domain.EscrowState;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.events.OutboxMessage;
import uz.agrogram.orderescrow.testing.Fixtures;
import uz.agrogram.orderescrow.testing.Fixtures.Harness;
import uz.agrogram.orderescrow.testing.ProfileSocialConsumer;
import uz.agrogram.orderescrow.v1.ConfirmHandoverRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptResponse;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.GetTransactionRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsResponse;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.PayInResponse;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptRequest;
import uz.agrogram.orderescrow.v1.Transaction;

class LifecycleTest {

    private final Harness h = new Harness();
    private final UUID farmerId = id();
    private final UUID resellerId = id();
    private final Principal farmer = Fixtures.farmer(farmerId);
    private final Principal reseller = Fixtures.reseller(resellerId);
    private final Listing offer = Fixtures.offer(farmerId).build();

    LifecycleTest() {
        h.catalog.put(offer);
    }

    private Transaction create(Principal p, Mass quantity) {
        return h.service.createTransaction(p, CreateTransactionRequest.newBuilder().setRequestId(rid())
                .setListingId(offer.getId()).setQuantity(quantity).build()).getTransaction();
    }

    private static PayInRequest pay(Transaction t) {
        return PayInRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId()).build();
    }

    private static SubmitWeighbridgeReceiptRequest receipt(Transaction t) {
        return SubmitWeighbridgeReceiptRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId())
                .setTicketNumber("TB-000123").setPhotoMediaId(id().toString())
                .setNetMass(Mass.newBuilder().setGrams(1_980_000).setDisplayUnit(MassUnit.MASS_UNIT_TONNE)).build();
    }

    private static ConfirmHandoverRequest handover(Transaction t) {
        return ConfirmHandoverRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId()).build();
    }

    private static ConfirmReceiptRequest confirm(Transaction t) {
        return ConfirmReceiptRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId()).build();
    }

    /** Runs the whole flow and returns the final transaction. */
    private Transaction complete(Transaction t) {
        h.clock.advance(Duration.ofMinutes(1));
        h.service.payIn(reseller, pay(t));
        h.clock.advance(Duration.ofHours(3));
        h.service.submitWeighbridgeReceipt(reseller, receipt(t));
        h.clock.advance(Duration.ofMinutes(5));
        h.service.confirmHandover(farmer, handover(t));
        h.clock.advance(Duration.ofMinutes(5));
        return h.service.confirmReceipt(reseller, confirm(t)).getTransaction();
    }

    private static AppException catchApp(Runnable r) {
        try {
            r.run();
        } catch (AppException e) {
            return e;
        }
        throw new AssertionError("expected an AppException");
    }

    private static TransactionEvent decode(OutboxMessage m) {
        try {
            return TransactionEvent.parseFrom(m.value());
        } catch (InvalidProtocolBufferException e) {
            throw new AssertionError(e);
        }
    }

    @Test
    void theWholeFlowEndsCompletedWithTheMoneyHeldThenPaidOutOnce() {
        Transaction t = create(reseller, tonnes(2));
        long total = t.getAmounts().getTotal().getAmountMinor();

        h.clock.advance(Duration.ofMinutes(1));
        PayInResponse paid = h.service.payIn(reseller, pay(t));
        assertThat(paid.getTransaction().getState()).isEqualTo(TransactionState.TRANSACTION_STATE_PAYMENT_HELD);
        assertThat(paid.getTransaction().getVersion()).isEqualTo(2);
        assertThat(paid.getCheckoutUrl()).isEmpty(); // TODO(payments)
        assertThat(paid.getTransaction().getPayment().getProvider()).isEqualTo("stub");
        assertThat(paid.getTransaction().getPayment().getProviderPaymentId()).isEmpty();
        assertThat(paid.getTransaction().getPayment().getAmount().getAmountMinor()).isEqualTo(total);

        Transaction submitted = h.service.submitWeighbridgeReceipt(reseller, receipt(t)).getTransaction();
        assertThat(submitted.getState()).isEqualTo(TransactionState.TRANSACTION_STATE_RECEIPT_SUBMITTED);
        assertThat(submitted.getReceipt().getTicketNumber()).isEqualTo("TB-000123");
        assertThat(submitted.getReceipt().getNetMass().getGrams()).isEqualTo(1_980_000);
        assertThat(submitted.getReceipt().getVerificationStatus())
                .isEqualTo(ReceiptVerificationStatus.RECEIPT_VERIFICATION_STATUS_VERIFIED); // TODO(weighbridge)
        assertThat(submitted.getReceipt().hasVerifiedAt()).isTrue();

        Transaction handedOver = h.service.confirmHandover(farmer, handover(t)).getTransaction();
        assertThat(handedOver.getState()).isEqualTo(TransactionState.TRANSACTION_STATE_HANDED_OVER);
        assertThat(handedOver.getMyRole()).isEqualTo(Role.ROLE_FARMER);

        h.clock.advance(Duration.ofMinutes(10));
        ConfirmReceiptResponse done = h.service.confirmReceipt(reseller, confirm(t));
        Transaction c = done.getTransaction();
        assertThat(c.getState()).isEqualTo(TransactionState.TRANSACTION_STATE_COMPLETED);
        assertThat(c.getVersion()).isEqualTo(7); // ReceiptConfirmed 5, PayoutReleased 6, BonusAwarded 7
        assertThat(c.getBonusPoints()).isEqualTo(1); // STUB TODO(bonus)
        assertThat(c.getPayout().getAmount().getAmountMinor()).isEqualTo(c.getAmounts().getPayoutAmount()
                .getAmountMinor());
        assertThat(c.getPayout().getProvider()).isEqualTo("stub");
        assertThat(c.hasCompletedAt()).isTrue();
        assertThat(c.getHistoryList()).extracting(x -> x.getState()).containsExactly(
                TransactionState.TRANSACTION_STATE_AWAITING_PAYMENT, TransactionState.TRANSACTION_STATE_PAYMENT_HELD,
                TransactionState.TRANSACTION_STATE_RECEIPT_SUBMITTED, TransactionState.TRANSACTION_STATE_HANDED_OVER,
                TransactionState.TRANSACTION_STATE_RECEIVED, TransactionState.TRANSACTION_STATE_COMPLETED);
        assertThat(c.getHistoryList()).extracting(x -> x.getActor().hasUserId() ? x.getActor().getUserId()
                : x.getActor().getService()).containsExactly(resellerId.toString(), resellerId.toString(),
                        resellerId.toString(), farmerId.toString(), resellerId.toString(), "order-escrow");

        // Money: held once for the total, released once as the payout; nothing created or lost.
        EscrowTransaction stored = h.store.transaction(UUID.fromString(t.getId())).orElseThrow();
        assertThat(stored.payment().orElseThrow().amountMinor()).isEqualTo(total);
        assertThat(stored.payment().orElseThrow().released()).isTrue();
        assertThat(stored.amounts().totalMinor()).isEqualTo(stored.payout().orElseThrow().amountMinor()
                + stored.amounts().platformFeeMinor() + stored.amounts().taxMinor());
        assertThat(stored.payout().orElseThrow().recipientId()).isEqualTo(farmerId);
    }

    @Test
    void eachStepPublishesItsEventInOrderWithIncreasingVersions() throws Exception {
        Transaction t = create(reseller, tonnes(2));
        Transaction done = complete(t);

        List<OutboxMessage> out = h.store.outbox();
        assertThat(out).extracting(OutboxMessage::eventType).containsExactly(
                "agrogram.events.v1.TransactionCreated", "agrogram.events.v1.PaymentHeld",
                "agrogram.events.v1.ReceiptSubmitted", "agrogram.events.v1.ProductHandover",
                "agrogram.events.v1.ReceiptConfirmed", "agrogram.events.v1.PayoutReleased",
                "agrogram.events.v1.BonusAwarded");
        List<TransactionEvent> events = out.stream().map(LifecycleTest::decode).toList();
        assertThat(events).extracting(e -> e.getMetadata().getAggregateVersion()).containsExactly(1L, 2L, 3L, 4L, 5L,
                6L, 7L);
        assertThat(events).extracting(e -> e.getTransaction().getVersion()).containsExactly(1L, 2L, 3L, 4L, 5L, 6L,
                7L);
        assertThat(events).extracting(e -> e.getTransaction().getState()).containsExactly(
                TransactionState.TRANSACTION_STATE_AWAITING_PAYMENT, TransactionState.TRANSACTION_STATE_PAYMENT_HELD,
                TransactionState.TRANSACTION_STATE_RECEIPT_SUBMITTED, TransactionState.TRANSACTION_STATE_HANDED_OVER,
                TransactionState.TRANSACTION_STATE_RECEIVED, TransactionState.TRANSACTION_STATE_COMPLETED,
                TransactionState.TRANSACTION_STATE_COMPLETED);
        for (int i = 0; i < out.size(); i++) {
            OutboxMessage m = out.get(i);
            TransactionEvent e = events.get(i);
            assertThat(m.key()).isEqualTo(t.getId());
            assertThat(e.getMetadata().getAggregateId()).isEqualTo(t.getId());
            assertThat(e.getMetadata().getEventId()).isEqualTo(m.id().toString());
            assertThat(e.getMetadata().getEventType()).isEqualTo(m.eventType());
            assertThat(m.headers()).containsEntry("event-type", m.eventType())
                    .containsEntry("event-id", m.id().toString());
            assertThat(e.getMetadata().getEventType())
                    .isEqualTo("agrogram.events.v1." + toPascal(e.getEventCase().name()));
        }

        TransactionEvent paymentHeld = events.get(1);
        assertThat(paymentHeld.getPaymentHeld().getAmount()).isEqualTo(t.getAmounts().getTotal());
        assertThat(paymentHeld.getTransaction().hasPaymentHeldAt()).isTrue();
        TransactionEvent submitted = events.get(2);
        assertThat(submitted.getReceiptSubmitted().getReceipt().getTicketNumber()).isEqualTo("TB-000123");
        assertThat(submitted.getTransaction().getReceipt()).isEqualTo(submitted.getReceiptSubmitted().getReceipt());
        assertThat(events.get(3).getMetadata().getActor().getUserId()).isEqualTo(farmerId.toString());

        // ReceiptConfirmed (the reseller) -> PayoutReleased -> BonusAwarded (order-escrow), one correlation chain.
        TransactionEvent confirmed = events.get(4);
        TransactionEvent released = events.get(5);
        TransactionEvent bonus = events.get(6);
        assertThat(confirmed.getMetadata().getActor().getUserId()).isEqualTo(resellerId.toString());
        assertThat(released.getMetadata().getActor().getService()).isEqualTo("order-escrow");
        assertThat(bonus.getMetadata().getActor().getService()).isEqualTo("order-escrow");
        assertThat(released.getMetadata().getCausationId()).isEqualTo(confirmed.getMetadata().getEventId());
        assertThat(bonus.getMetadata().getCausationId()).isEqualTo(released.getMetadata().getEventId());
        assertThat(released.getMetadata().getCorrelationId()).isEqualTo(confirmed.getMetadata().getCorrelationId());
        assertThat(released.getPayoutReleased().getAmount()).isEqualTo(done.getAmounts().getPayoutAmount());
        assertThat(released.getTransaction().hasCompletedAt()).isTrue();
        assertThat(bonus.getBonusAwarded().getRecipientId()).isEqualTo(resellerId.toString());
        assertThat(bonus.getBonusAwarded().getPoints()).isEqualTo(1);
        assertThat(bonus.getTransaction().getBonusPoints()).isEqualTo(1);
        assertThat(released.getTransaction().getBonusPoints()).isZero();

        for (TransactionEvent e : events) {
            assertThat(Fixtures.VALIDATOR.validate(e).isSuccess()).as(e.getEventCase().name()).isTrue();
        }
    }

    @Test
    void profileSocialRecordsTheCompletedTransactionFromTheStream() {
        Transaction t = create(reseller, tonnes(2));
        Transaction done = complete(t);

        List<ProfileSocialConsumer.Completed> completed = new ArrayList<>();
        for (OutboxMessage m : h.store.outbox()) {
            Optional<ProfileSocialConsumer.Completed> c = ProfileSocialConsumer.decode(m.headers(), m.value());
            c.ifPresent(completed::add);
        }
        // PayoutReleased introduces COMPLETED and BonusAwarded repeats it; profile-social is idempotent by id.
        assertThat(completed).hasSize(2).allSatisfy(c -> {
            assertThat(c.id()).isEqualTo(UUID.fromString(t.getId()));
            assertThat(c.farmerId()).isEqualTo(farmerId);
            assertThat(c.resellerId()).isEqualTo(resellerId);
            assertThat(c.completedAt()).isEqualTo(Instant.ofEpochSecond(done.getCompletedAt().getSeconds(),
                    done.getCompletedAt().getNanos()));
        });
    }

    @Test
    void withTheBonusTurnedOffNoBonusAwardedIsPublished() {
        Harness noBonus = new Harness(0);
        noBonus.catalog.put(offer);
        Transaction t = noBonus.service.createTransaction(reseller, CreateTransactionRequest.newBuilder()
                .setRequestId(rid()).setListingId(offer.getId()).setQuantity(tonnes(1)).build()).getTransaction();
        noBonus.service.payIn(reseller, pay(t));
        noBonus.service.submitWeighbridgeReceipt(reseller, receipt(t));
        noBonus.service.confirmHandover(farmer, handover(t));
        Transaction done = noBonus.service.confirmReceipt(reseller, confirm(t)).getTransaction();
        assertThat(done.getVersion()).isEqualTo(6);
        assertThat(done.getBonusPoints()).isZero();
        assertThat(noBonus.store.outbox()).extracting(OutboxMessage::eventType)
                .doesNotContain("agrogram.events.v1.BonusAwarded").hasSize(6);
    }

    @Test
    void theWrongPartyIsRefusedAndStrangersDoNotSeeTheTransaction() {
        Transaction t = create(reseller, tonnes(1));
        AppException farmerPays = catchApp(() -> h.service.payIn(farmer, pay(t)));
        assertThat(farmerPays.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);
        assertThat(farmerPays.code()).isEqualTo(Status.Code.PERMISSION_DENIED);

        AppException staffPays = catchApp(() -> h.service.payIn(Fixtures.staff(id()), pay(t)));
        assertThat(staffPays.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);

        AppException stranger = catchApp(() -> h.service.payIn(Fixtures.reseller(id()), pay(t)));
        assertThat(stranger.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_FOUND);
        AppException unknown = catchApp(() -> h.service.payIn(reseller, PayInRequest.newBuilder()
                .setRequestId(rid()).setTransactionId(id().toString()).build()));
        assertThat(unknown.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_FOUND);

        h.service.payIn(reseller, pay(t));
        h.service.submitWeighbridgeReceipt(reseller, receipt(t));
        AppException resellerHandsOver = catchApp(() -> h.service.confirmHandover(reseller, handover(t)));
        assertThat(resellerHandsOver.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);
        h.service.confirmHandover(farmer, handover(t));
        AppException farmerConfirms = catchApp(() -> h.service.confirmReceipt(farmer, confirm(t)));
        assertThat(farmerConfirms.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);
        assertThat(h.store.transaction(UUID.fromString(t.getId())).orElseThrow().state())
                .isEqualTo(EscrowState.HANDED_OVER);
    }

    @Test
    void stepsOutOfOrderAreInvalidStateTransitions() {
        Transaction t = create(reseller, tonnes(1));
        AppException early = catchApp(() -> h.service.submitWeighbridgeReceipt(reseller, receipt(t)));
        assertThat(early.reason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        assertThat(early.code()).isEqualTo(Status.Code.FAILED_PRECONDITION);
        assertThat(early.metadata()).containsEntry("state", "AWAITING_PAYMENT")
                .containsEntry("command", "SubmitWeighbridgeReceipt");

        h.service.payIn(reseller, pay(t));
        AppException payTwice = catchApp(() -> h.service.payIn(reseller, pay(t)));
        assertThat(payTwice.reason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        assertThat(payTwice.metadata()).containsEntry("state", "PAYMENT_HELD").containsEntry("command", "PayIn");

        AppException handoverEarly = catchApp(() -> h.service.confirmHandover(farmer, handover(t)));
        assertThat(handoverEarly.metadata()).containsEntry("state", "PAYMENT_HELD");
        assertThat(h.store.outbox()).hasSize(2);
    }

    @Test
    void aCompletedTransactionAcceptsNoFurtherSteps() {
        Transaction t = complete(create(reseller, tonnes(1)));
        int events = h.store.outbox().size();
        assertThat(catchApp(() -> h.service.payIn(reseller, pay(t))).reason())
                .isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        assertThat(catchApp(() -> h.service.submitWeighbridgeReceipt(reseller, receipt(t))).reason())
                .isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        assertThat(catchApp(() -> h.service.confirmHandover(farmer, handover(t))).reason())
                .isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        AppException again = catchApp(() -> h.service.confirmReceipt(reseller, confirm(t)));
        assertThat(again.reason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        assertThat(again.metadata()).containsEntry("state", "COMPLETED");
        assertThat(h.store.outbox()).hasSize(events);
    }

    @Test
    void retriesOfEveryStepReturnTheRecordedResultWithoutSideEffects() {
        Transaction t = create(reseller, tonnes(1));
        PayInRequest payIn = pay(t);
        PayInResponse first = h.service.payIn(reseller, payIn);
        SubmitWeighbridgeReceiptRequest submit = receipt(t);
        Transaction submitted = h.service.submitWeighbridgeReceipt(reseller, submit).getTransaction();
        ConfirmHandoverRequest ho = handover(t);
        Transaction handedOver = h.service.confirmHandover(farmer, ho).getTransaction();
        ConfirmReceiptRequest cr = confirm(t);
        ConfirmReceiptResponse done = h.service.confirmReceipt(reseller, cr);
        int events = h.store.outbox().size();

        // Retries after the transaction moved on still answer what the first call answered.
        assertThat(h.service.payIn(reseller, payIn)).isEqualTo(first);
        assertThat(h.service.submitWeighbridgeReceipt(reseller, submit).getTransaction()).isEqualTo(submitted);
        assertThat(h.service.confirmHandover(farmer, ho).getTransaction()).isEqualTo(handedOver);
        assertThat(h.service.confirmReceipt(reseller, cr)).isEqualTo(done);
        assertThat(h.service.confirmReceipt(reseller, cr)).isEqualTo(done);

        assertThat(h.store.outbox()).hasSize(events);
        assertThat(h.store.outbox()).filteredOn(m -> m.eventType().endsWith("PayoutReleased")).hasSize(1);
        assertThat(h.store.outbox()).filteredOn(m -> m.eventType().endsWith("PaymentHeld")).hasSize(1);
        assertThat(h.store.transaction(UUID.fromString(t.getId())).orElseThrow().version()).isEqualTo(7);
    }

    @Test
    void aRequestIdUsedOnAnotherTransactionOrPayloadIsRejected() {
        Transaction a = create(reseller, tonnes(1));
        Transaction b = create(Fixtures.reseller(id()), tonnes(1));
        PayInRequest payA = pay(a);
        h.service.payIn(reseller, payA);

        AppException otherPayload = catchApp(() -> h.service.payIn(reseller, payA.toBuilder()
                .setReturnUrl("https://app.agrogram.uz/return").build()));
        assertThat(otherPayload.violations().get(0).getRule()).isEqualTo("request_id.reused");

        // On another of the caller's transactions the same request id is a reuse too.
        Listing other = Fixtures.offer(farmerId).build();
        h.catalog.put(other);
        Transaction c = h.service.createTransaction(reseller, CreateTransactionRequest.newBuilder()
                .setRequestId(rid()).setListingId(other.getId()).setQuantity(tonnes(1)).build()).getTransaction();
        AppException otherTransaction = catchApp(() -> h.service.payIn(reseller, payA.toBuilder()
                .setTransactionId(c.getId()).build()));
        assertThat(otherTransaction.violations().get(0).getRule()).isEqualTo("request_id.reused");
        assertThat(h.store.transaction(UUID.fromString(c.getId())).orElseThrow().state())
                .isEqualTo(EscrowState.AWAITING_PAYMENT);

        // The request id is the caller's; on a transaction the caller cannot see, the answer is NOT_FOUND.
        AppException hidden = catchApp(() -> h.service.payIn(reseller, payA.toBuilder()
                .setTransactionId(b.getId()).build()));
        assertThat(hidden.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_FOUND);
    }

    @Test
    void concurrentDuplicatesOfOneRequestAllGetTheSameResult() throws Exception {
        Transaction t = create(reseller, tonnes(1));
        PayInRequest payIn = pay(t);
        List<Object> results = race(16, () -> h.service.payIn(reseller, payIn));
        assertThat(results).allSatisfy(r -> assertThat(r).isEqualTo(results.get(0)));
        assertThat(results.get(0)).isInstanceOf(PayInResponse.class);
        assertThat(h.store.outbox()).filteredOn(m -> m.eventType().endsWith("PaymentHeld")).hasSize(1);
    }

    @Test
    void concurrentDifferentRequestsForOneStepSucceedExactlyOnce() throws Exception {
        Transaction t = create(reseller, tonnes(1));
        h.service.payIn(reseller, pay(t));
        h.service.submitWeighbridgeReceipt(reseller, receipt(t));
        h.service.confirmHandover(farmer, handover(t));

        List<Object> results = race(16, () -> h.service.confirmReceipt(reseller, confirm(t)));

        assertThat(results).filteredOn(r -> r instanceof ConfirmReceiptResponse).hasSize(1);
        assertThat(results).filteredOn(r -> r instanceof AppException).allSatisfy(r -> assertThat(
                ((AppException) r).reason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION));
        assertThat(h.store.outbox()).filteredOn(m -> m.eventType().endsWith("PayoutReleased")).hasSize(1);
        assertThat(h.store.transaction(UUID.fromString(t.getId())).orElseThrow().payout()).isPresent();
    }

    @Test
    void concurrentCreationsNeverOversell() throws Exception {
        List<Object> results = race(20, () -> create(Fixtures.reseller(id()), tonnes(1)));
        assertThat(results).filteredOn(r -> r instanceof Transaction).hasSize(5);
        assertThat(results).filteredOn(r -> r instanceof AppException).allSatisfy(r -> assertThat(
                ((AppException) r).reason()).isEqualTo(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY));
        assertThat(h.store.transactions().stream().mapToLong(EscrowTransaction::quantityGrams).sum())
                .isEqualTo(5_000_000);
    }

    @Test
    void aStepThatLosesARaceIsRetriedAndHappensOnce() {
        Transaction t = create(reseller, tonnes(1));
        h.store.failNextCommits(3);
        PayInResponse r = h.service.payIn(reseller, pay(t));
        assertThat(r.getTransaction().getVersion()).isEqualTo(2);
        assertThat(h.store.outbox()).filteredOn(m -> m.eventType().endsWith("PaymentHeld")).hasSize(1);
    }

    @Test
    void aStepThatKeepsLosingChangesNothing() {
        Transaction t = create(reseller, tonnes(1));
        int events = h.store.outbox().size();
        h.store.failNextCommits(100);
        AppException e = catchApp(() -> h.service.payIn(reseller, pay(t)));
        h.store.failNextCommits(0);
        assertThat(e.code()).isEqualTo(Status.Code.ABORTED);
        EscrowTransaction stored = h.store.transaction(UUID.fromString(t.getId())).orElseThrow();
        assertThat(stored.state()).isEqualTo(EscrowState.AWAITING_PAYMENT);
        assertThat(stored.payment()).isEmpty();
        assertThat(h.store.outbox()).hasSize(events);
        assertThat(h.store.processedRequests()).isEqualTo(1);
    }

    @Test
    void getTransactionIsForThePartiesAndStaff() {
        Transaction t = create(reseller, tonnes(1));
        GetTransactionRequest get = GetTransactionRequest.newBuilder().setTransactionId(t.getId()).build();
        assertThat(h.service.getTransaction(reseller, get).getTransaction().getMyRole()).isEqualTo(Role.ROLE_RESELLER);
        assertThat(h.service.getTransaction(farmer, get).getTransaction().getMyRole()).isEqualTo(Role.ROLE_FARMER);
        Transaction asStaff = h.service.getTransaction(Fixtures.staff(id()), get).getTransaction();
        assertThat(asStaff.getMyRole()).isEqualTo(Role.ROLE_ADMIN);
        assertThat(asStaff.getHistoryCount()).isEqualTo(1);
        AppException stranger = catchApp(() -> h.service.getTransaction(Fixtures.reseller(id()), get));
        assertThat(stranger.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_FOUND);
        AppException anonymous = catchApp(() -> h.service.getTransaction(Principal.anonymous(), get));
        assertThat(anonymous.code()).isEqualTo(Status.Code.UNAUTHENTICATED);
    }

    @Test
    void listTransactionsPagesNewestFirstWithFilters() {
        List<Transaction> mine = new ArrayList<>();
        for (int i = 0; i < 5; i++) {
            Listing l = Fixtures.offer(farmerId).build();
            h.catalog.put(l);
            h.clock.advance(Duration.ofSeconds(1));
            mine.add(h.service.createTransaction(reseller, CreateTransactionRequest.newBuilder().setRequestId(rid())
                    .setListingId(l.getId()).setQuantity(tonnes(1)).build()).getTransaction());
        }
        h.service.payIn(reseller, pay(mine.get(1)));

        ListTransactionsRequest firstPage = ListTransactionsRequest.newBuilder()
                .setPage(uz.agrogram.common.v1.PageRequest.newBuilder().setPageSize(2)).build();
        ListTransactionsResponse p1 = h.service.listTransactions(reseller, firstPage);
        assertThat(p1.getTransactionsList()).extracting(Transaction::getId)
                .containsExactly(mine.get(4).getId(), mine.get(3).getId());
        ListTransactionsResponse p2 = h.service.listTransactions(reseller, firstPage.toBuilder()
                .setPage(firstPage.getPage().toBuilder().setPageToken(p1.getPage().getNextPageToken())).build());
        assertThat(p2.getTransactionsList()).extracting(Transaction::getId)
                .containsExactly(mine.get(2).getId(), mine.get(1).getId());
        ListTransactionsResponse p3 = h.service.listTransactions(reseller, firstPage.toBuilder()
                .setPage(firstPage.getPage().toBuilder().setPageToken(p2.getPage().getNextPageToken())).build());
        assertThat(p3.getTransactionsList()).extracting(Transaction::getId).containsExactly(mine.get(0).getId());
        assertThat(p3.getPage().getNextPageToken()).isEmpty();

        // The farmer sees the same five as sales; the reseller has no sales.
        assertThat(h.service.listTransactions(farmer, ListTransactionsRequest.newBuilder()
                .setRole(Role.ROLE_FARMER).build()).getTransactionsList()).hasSize(5)
                .allSatisfy(x -> assertThat(x.getMyRole()).isEqualTo(Role.ROLE_FARMER));
        assertThat(h.service.listTransactions(reseller, ListTransactionsRequest.newBuilder()
                .setRole(Role.ROLE_FARMER).build()).getTransactionsList()).isEmpty();
        // State and listing filters.
        assertThat(h.service.listTransactions(reseller, ListTransactionsRequest.newBuilder()
                .addStates(TransactionState.TRANSACTION_STATE_PAYMENT_HELD).build()).getTransactionsList())
                .extracting(Transaction::getId).containsExactly(mine.get(1).getId());
        assertThat(h.service.listTransactions(reseller, ListTransactionsRequest.newBuilder()
                .setListingId(mine.get(3).getListingId()).build()).getTransactionsList())
                .extracting(Transaction::getId).containsExactly(mine.get(3).getId());

        // A token only works with the filter that produced it.
        AppException misuse = catchApp(() -> h.service.listTransactions(reseller, ListTransactionsRequest
                .newBuilder().setRole(Role.ROLE_RESELLER).setPage(uz.agrogram.common.v1.PageRequest.newBuilder()
                        .setPageToken(p1.getPage().getNextPageToken())).build()));
        assertThat(misuse.violations().get(0).getField()).isEqualTo("page.page_token");
        AppException garbage = catchApp(() -> h.service.listTransactions(reseller, ListTransactionsRequest
                .newBuilder().setPage(uz.agrogram.common.v1.PageRequest.newBuilder().setPageToken("%%%")).build()));
        assertThat(garbage.violations().get(0).getRule()).isEqualTo("page_token.malformed");
    }

    private static String toPascal(String enumName) {
        StringBuilder sb = new StringBuilder();
        for (String part : enumName.split("_")) {
            sb.append(part.charAt(0)).append(part.substring(1).toLowerCase(java.util.Locale.ROOT));
        }
        return sb.toString();
    }

    /** Runs {@code n} copies of {@code call} at once; returns each result or AppException. */
    private static List<Object> race(int n, Callable<Object> call) throws Exception {
        ExecutorService pool = Executors.newFixedThreadPool(n);
        try {
            CountDownLatch start = new CountDownLatch(1);
            List<Future<Object>> futures = new ArrayList<>();
            for (int i = 0; i < n; i++) {
                futures.add(pool.submit(() -> {
                    start.await();
                    try {
                        return call.call();
                    } catch (AppException e) {
                        return e;
                    }
                }));
            }
            start.countDown();
            List<Object> out = new ArrayList<>();
            for (Future<Object> f : futures) {
                out.add(f.get());
            }
            return out;
        } finally {
            pool.shutdownNow();
        }
    }
}
