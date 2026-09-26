package uz.agrogram.orderescrow.service;

import static org.assertj.core.api.Assertions.assertThat;
import static uz.agrogram.orderescrow.testing.Fixtures.farmer;
import static uz.agrogram.orderescrow.testing.Fixtures.id;
import static uz.agrogram.orderescrow.testing.Fixtures.reseller;
import static uz.agrogram.orderescrow.testing.Fixtures.rid;
import static uz.agrogram.orderescrow.testing.Fixtures.tonnes;

import io.grpc.Status;
import java.util.List;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.Mass;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.ResourceType;
import uz.agrogram.common.v1.Role;
import uz.agrogram.events.v1.ListingStatus;
import uz.agrogram.events.v1.TransactionEvent;
import uz.agrogram.events.v1.TransactionState;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.events.OutboxMessage;
import uz.agrogram.orderescrow.testing.Fixtures;
import uz.agrogram.orderescrow.testing.Fixtures.Harness;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.Transaction;

class CreateTransactionTest {

    private final Harness h = new Harness();
    private final UUID farmerId = id();
    private final UUID resellerId = id();

    private Listing put(Listing.Builder l) {
        Listing listing = l.build();
        h.catalog.put(listing);
        return listing;
    }

    private static CreateTransactionRequest req(Listing l, Mass quantity) {
        CreateTransactionRequest.Builder b = CreateTransactionRequest.newBuilder()
                .setRequestId(rid())
                .setListingId(l.getId());
        if (quantity != null) {
            b.setQuantity(quantity);
        }
        return b.build();
    }

    private AppException fails(Principal p, CreateTransactionRequest r) {
        try {
            h.service.createTransaction(p, r);
        } catch (AppException e) {
            return e;
        }
        throw new AssertionError("expected an error");
    }

    private void assertNothingWritten() {
        assertThat(h.store.transactions()).isEmpty();
        assertThat(h.store.outbox()).isEmpty();
        assertThat(h.store.processedRequests()).isZero();
    }

    @Test
    void aResellerStartsATransactionOnAnOffer() throws Exception {
        Listing offer = put(Fixtures.offer(farmerId));
        Principal p = reseller(resellerId);

        Transaction t = h.service.createTransaction(p, req(offer, tonnes(2))).getTransaction();

        assertThat(t.getState()).isEqualTo(TransactionState.TRANSACTION_STATE_AWAITING_PAYMENT);
        assertThat(t.getVersion()).isEqualTo(1);
        assertThat(t.getListingId()).isEqualTo(offer.getId());
        assertThat(t.getListingKind()).isEqualTo(ListingKind.LISTING_KIND_OFFER);
        assertThat(t.getFarmerId()).isEqualTo(farmerId.toString());
        assertThat(t.getResellerId()).isEqualTo(resellerId.toString());
        assertThat(t.getMyRole()).isEqualTo(Role.ROLE_RESELLER);
        assertThat(t.getProduct()).isEqualTo(offer.getProduct());
        assertThat(t.getQuantity().getGrams()).isEqualTo(2_000_000);
        assertThat(t.getQuantity().getDisplayUnit()).isEqualTo(MassUnit.MASS_UNIT_TONNE);
        assertThat(t.getUnitPrice()).isEqualTo(offer.getPrice());
        assertThat(t.getAddress()).isEqualTo(offer.getAddress());
        // 6 000 so'm/kg x 2 000 kg = 12 000 000 so'm = 1 200 000 000 tiyin, commission- and tax-free.
        assertThat(t.getAmounts().getSubtotal().getAmountMinor()).isEqualTo(1_200_000_000L);
        assertThat(t.getAmounts().getTotal().getAmountMinor()).isEqualTo(1_200_000_000L);
        assertThat(t.getAmounts().getPayoutAmount().getAmountMinor()).isEqualTo(1_200_000_000L);
        assertThat(t.getAmounts().getTax().getAmountMinor()).isZero();
        assertThat(t.getAmounts().getPlatformFee().getAmountMinor()).isZero();
        assertThat(t.hasPayment()).isFalse();
        assertThat(t.getHistoryList()).hasSize(1);
        assertThat(t.getHistory(0).getState()).isEqualTo(TransactionState.TRANSACTION_STATE_AWAITING_PAYMENT);
        assertThat(t.getHistory(0).getActor().getUserId()).isEqualTo(resellerId.toString());
        assertThat(Fixtures.VALIDATOR.validate(t).isSuccess()).isTrue();

        List<OutboxMessage> out = h.store.outbox();
        assertThat(out).hasSize(1);
        OutboxMessage m = out.get(0);
        assertThat(m.topic()).isEqualTo("agrogram.orderescrow.transaction.v1");
        assertThat(m.key()).isEqualTo(t.getId());
        assertThat(m.eventType()).isEqualTo("agrogram.events.v1.TransactionCreated");
        assertThat(m.headers()).containsEntry("content-type", "application/x-protobuf")
                .containsEntry("event-type", "agrogram.events.v1.TransactionCreated")
                .containsEntry("event-id", m.id().toString())
                .containsEntry("x-request-id", p.requestId())
                .containsEntry("traceparent", Fixtures.TRACEPARENT)
                .containsEntry("tracestate", "vendor=1");
        TransactionEvent ev = TransactionEvent.parseFrom(m.value());
        assertThat(ev.hasTransactionCreated()).isTrue();
        assertThat(ev.getMetadata().getEventId()).isEqualTo(m.id().toString());
        assertThat(ev.getMetadata().getProducer()).isEqualTo("order-escrow");
        assertThat(ev.getMetadata().getAggregateType()).isEqualTo(ResourceType.RESOURCE_TYPE_TRANSACTION);
        assertThat(ev.getMetadata().getAggregateId()).isEqualTo(t.getId());
        assertThat(ev.getMetadata().getAggregateVersion()).isEqualTo(1);
        assertThat(ev.getMetadata().getCorrelationId()).isEqualTo(p.requestId());
        assertThat(ev.getMetadata().getCausationId()).isEmpty();
        assertThat(ev.getMetadata().getActor().getUserId()).isEqualTo(resellerId.toString());
        assertThat(ev.getTransaction().getVersion()).isEqualTo(1);
        assertThat(ev.getTransaction().getAmounts()).isEqualTo(t.getAmounts());
        assertThat(ev.getTransaction().getAddress()).isEqualTo(t.getAddress());
        assertThat(Fixtures.VALIDATOR.validate(ev).isSuccess()).isTrue();
        // Never contact details: the listing's phone is not in the event.
        assertThat(new String(m.value(), java.nio.charset.StandardCharsets.ISO_8859_1))
                .doesNotContain(offer.getPhone());
    }

    @Test
    void withoutAQuantityTheTransactionTakesEverythingStillAvailable() {
        Listing offer = put(Fixtures.offer(farmerId));
        h.service.createTransaction(reseller(id()), req(offer, tonnes(1.5)));

        Transaction t = h.service.createTransaction(reseller(resellerId), req(offer, null)).getTransaction();

        assertThat(t.getQuantity().getGrams()).isEqualTo(3_500_000);
        assertThat(t.getQuantity().getDisplayUnit()).isEqualTo(MassUnit.MASS_UNIT_TONNE);
    }

    @Test
    void aFarmerSellsToABuyRequestAtTheAgreedPrice() {
        Listing request = put(Fixtures.buyRequest(resellerId));
        CreateTransactionRequest r = req(request, tonnes(1)).toBuilder()
                .setUnitPrice(Fixtures.pricePerKg(350_000)).build();

        Transaction t = h.service.createTransaction(farmer(farmerId), r).getTransaction();

        assertThat(t.getListingKind()).isEqualTo(ListingKind.LISTING_KIND_BUY_REQUEST);
        assertThat(t.getFarmerId()).isEqualTo(farmerId.toString());
        assertThat(t.getResellerId()).isEqualTo(resellerId.toString());
        assertThat(t.getMyRole()).isEqualTo(Role.ROLE_FARMER);
        assertThat(t.getUnitPrice().getAmount().getAmountMinor()).isEqualTo(350_000);
        assertThat(t.getAmounts().getTotal().getAmountMinor()).isEqualTo(350_000_000L);
    }

    @Test
    void sellingToABuyRequestNeedsAVerifiedFarmer() {
        Listing request = put(Fixtures.buyRequest(resellerId));
        CreateTransactionRequest r = req(request, tonnes(1)).toBuilder()
                .setUnitPrice(Fixtures.pricePerKg(350_000)).build();

        AppException e = fails(reseller(farmerId), r);

        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_FARMER_VERIFICATION_REQUIRED);
        assertThat(e.code()).isEqualTo(Status.Code.FAILED_PRECONDITION);
        assertThat(e.metadata()).containsEntry("listing_kind", "BUY_REQUEST").containsEntry("required_role", "FARMER");
        assertNothingWritten();
    }

    @Test
    void buyingFromAnOfferNeedsTheResellerRole() {
        Listing offer = put(Fixtures.offer(farmerId));
        AppException e = fails(Fixtures.user(resellerId, Role.ROLE_ADMIN), req(offer, tonnes(1)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_PERMISSION_DENIED);
        assertThat(e.metadata()).containsEntry("required_role", "RESELLER");
        assertNothingWritten();
    }

    @Test
    void theOwnerCannotTradeWithThemselves() {
        Listing offer = put(Fixtures.offer(farmerId));
        AppException e = fails(farmer(farmerId), req(offer, tonnes(1)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_SELF_TRADE_NOT_ALLOWED);
        assertThat(e.code()).isEqualTo(Status.Code.FAILED_PRECONDITION);
        assertNothingWritten();
    }

    @ParameterizedTest
    @EnumSource(value = ListingStatus.class, names = {"LISTING_STATUS_CLOSED", "LISTING_STATUS_SOLD_OUT",
        "LISTING_STATUS_BLOCKED", "LISTING_STATUS_DELETED"})
    void onlyActiveListingsStartTransactions(ListingStatus status) {
        Listing offer = put(Fixtures.offer(farmerId).setStatus(status));
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_LISTING_NOT_ACTIVE);
        assertThat(e.metadata()).containsEntry("status", status.name().substring("LISTING_STATUS_".length()));
        assertNothingWritten();
    }

    @Test
    void aPricedListingRejectsAPriceInTheRequest() {
        Listing offer = put(Fixtures.offer(farmerId));
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)).toBuilder()
                .setUnitPrice(Fixtures.pricePerKg(1)).build());
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_VALIDATION_FAILED);
        assertThat(e.violations().get(0).getField()).isEqualTo("unit_price");
        assertNothingWritten();
    }

    @Test
    void aNegotiableListingNeedsAPriceInTheRequest() {
        Listing offer = put(Fixtures.offer(farmerId).clearPrice());
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_VALIDATION_FAILED);
        assertThat(e.violations().get(0).getRule()).isEqualTo("create_transaction.unit_price_required");
        assertNothingWritten();
    }

    @Test
    void aTotalThatRoundsToZeroIsRejected() {
        Listing offer = put(Fixtures.offer(farmerId).setPrice(Fixtures.pricePerKg(1)));
        Mass oneGram = Mass.newBuilder().setGrams(1).setDisplayUnit(MassUnit.MASS_UNIT_KILOGRAM).build();
        AppException e = fails(reseller(resellerId), req(offer, oneGram));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_VALIDATION_FAILED);
        assertThat(e.violations().get(0).getRule()).isEqualTo("create_transaction.total_zero");
        assertNothingWritten();
    }

    @Test
    void moreThanTheListingOffersIsInsufficientQuantity() {
        Listing offer = put(Fixtures.offer(farmerId));
        AppException e = fails(reseller(resellerId), req(offer, tonnes(5.000001)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY);
        assertThat(e.metadata()).containsEntry("requested_grams", "5000001").containsEntry("available_grams",
                "5000000");
        assertNothingWritten();
    }

    @Test
    void transactionsNeverOversellAListing() {
        Listing offer = put(Fixtures.offer(farmerId));
        h.service.createTransaction(reseller(id()), req(offer, tonnes(3)));

        AppException e = fails(reseller(resellerId), req(offer, tonnes(3)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY);
        assertThat(e.metadata()).containsEntry("available_grams", "2000000");

        h.service.createTransaction(reseller(resellerId), req(offer, tonnes(2)));
        AppException none = fails(reseller(id()), req(offer, null));
        assertThat(none.reason()).isEqualTo(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY);
        assertThat(none.metadata()).containsEntry("available_grams", "0");

        long committed = h.store.transactions().stream().mapToLong(t -> t.quantityGrams()).sum();
        assertThat(committed).isEqualTo(offer.getQuantity().getGrams());
    }

    @Test
    void oversellIsCountedFromTheListingQuantityNotCatalogsRemainingQuantity() {
        // catalog will subtract escrow commitments from remaining_quantity (TODO(phase3) there);
        // order-escrow must not subtract them a second time.
        Listing offer = put(Fixtures.offer(farmerId).setRemainingQuantity(tonnes(1)));
        Transaction t = h.service.createTransaction(reseller(resellerId), req(offer, tonnes(4))).getTransaction();
        assertThat(t.getQuantity().getGrams()).isEqualTo(4_000_000);
    }

    @Test
    void theCallersOpenTransactionIsReturnedInsteadOfANewOne() {
        Listing offer = put(Fixtures.offer(farmerId));
        Principal p = reseller(resellerId);
        Transaction first = h.service.createTransaction(p, req(offer, tonnes(1))).getTransaction();
        int catalogCalls = h.catalog.calls();

        CreateTransactionRequest again = req(offer, tonnes(2));
        Transaction second = h.service.createTransaction(p, again).getTransaction();

        assertThat(second.getId()).isEqualTo(first.getId());
        assertThat(second.getQuantity().getGrams()).isEqualTo(1_000_000);
        assertThat(h.catalog.calls()).isEqualTo(catalogCalls);
        assertThat(h.store.transactions()).hasSize(1);
        assertThat(h.store.outbox()).hasSize(1);
        // ...and that request is recorded too: its retry answers the same.
        assertThat(h.service.createTransaction(p, again).getTransaction()).isEqualTo(second);
        // Even when the listing is no longer active (e.g. SOLD_OUT by this transaction).
        put(offer.toBuilder().setStatus(ListingStatus.LISTING_STATUS_SOLD_OUT));
        assertThat(h.service.createTransaction(p, req(offer, null)).getTransaction().getId())
                .isEqualTo(first.getId());
    }

    @Test
    void aRetryReturnsTheRecordedResultWithoutSideEffects() {
        Listing offer = put(Fixtures.offer(farmerId));
        Principal p = reseller(resellerId);
        CreateTransactionRequest r = req(offer, tonnes(1));
        Transaction first = h.service.createTransaction(p, r).getTransaction();
        int calls = h.catalog.calls();

        Transaction retry = h.service.createTransaction(p, r).getTransaction();

        assertThat(retry).isEqualTo(first);
        assertThat(h.catalog.calls()).isEqualTo(calls);
        assertThat(h.store.transactions()).hasSize(1);
        assertThat(h.store.outbox()).hasSize(1);
        assertThat(h.store.processedRequests()).isEqualTo(1);
    }

    @Test
    void aRequestIdCannotBeReusedForADifferentRequest() {
        Listing offer = put(Fixtures.offer(farmerId));
        Principal p = reseller(resellerId);
        CreateTransactionRequest r = req(offer, tonnes(1));
        Transaction t = h.service.createTransaction(p, r).getTransaction();

        AppException payload = fails(p, r.toBuilder().setQuantity(tonnes(2)).build());
        assertThat(payload.reason()).isEqualTo(ErrorReason.ERROR_REASON_VALIDATION_FAILED);
        assertThat(payload.violations().get(0).getField()).isEqualTo("request_id");
        assertThat(payload.violations().get(0).getRule()).isEqualTo("request_id.reused");

        AppException operation = catchApp(() -> h.service.payIn(p, PayInRequest.newBuilder()
                .setRequestId(r.getRequestId()).setTransactionId(t.getId()).build()));
        assertThat(operation.violations().get(0).getRule()).isEqualTo("request_id.reused");
        assertThat(h.store.transaction(UUID.fromString(t.getId())).orElseThrow().version()).isEqualTo(1);
    }

    @Test
    void requestIdsBelongToTheirCaller() {
        Listing offer = put(Fixtures.offer(farmerId));
        CreateTransactionRequest r = req(offer, tonnes(1));
        Transaction mine = h.service.createTransaction(reseller(resellerId), r).getTransaction();
        Transaction theirs = h.service.createTransaction(reseller(id()), r).getTransaction();
        assertThat(theirs.getId()).isNotEqualTo(mine.getId());
    }

    @Test
    void catalogBeingDownFailsCleanlyAndWritesNothing() {
        Listing offer = put(Fixtures.offer(farmerId));
        h.catalog.failWith(AppException.of(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE, "catalog is unavailable",
                "dependency", "catalog"));
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)));
        assertThat(e.code()).isEqualTo(Status.Code.UNAVAILABLE);
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
        assertNothingWritten();
    }

    @Test
    void anUnknownListingIsNotFound() {
        Listing offer = Fixtures.offer(farmerId).build();
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_FOUND);
        assertNothingWritten();
    }

    @Test
    void anIncompleteListingFromCatalogIsAnInternalError() {
        Listing offer = put(Fixtures.offer(farmerId).clearAddress());
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)));
        assertThat(e.code()).isEqualTo(Status.Code.INTERNAL);
        assertNothingWritten();
    }

    @Test
    void anonymousCallersAreUnauthenticated() {
        Listing offer = put(Fixtures.offer(farmerId));
        AppException e = fails(Principal.anonymous(), req(offer, tonnes(1)));
        assertThat(e.code()).isEqualTo(Status.Code.UNAUTHENTICATED);
    }

    @Test
    void aLostRaceIsRetriedAndCreatesExactlyOneTransaction() {
        Listing offer = put(Fixtures.offer(farmerId));
        h.store.failNextCommits(2);
        Transaction t = h.service.createTransaction(reseller(resellerId), req(offer, tonnes(1))).getTransaction();
        assertThat(h.store.transactions()).extracting(x -> x.id().toString()).containsExactly(t.getId());
        assertThat(h.store.outbox()).hasSize(1);
    }

    @Test
    void racesLostEveryTimeEndInConflictWithNothingWritten() {
        Listing offer = put(Fixtures.offer(farmerId));
        h.store.failNextCommits(100);
        AppException e = fails(reseller(resellerId), req(offer, tonnes(1)));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_CONFLICT);
        assertThat(e.code()).isEqualTo(Status.Code.ABORTED);
        h.store.failNextCommits(0);
        assertNothingWritten();
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
