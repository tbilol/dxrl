package uz.agrogram.orderescrow.events;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.time.Instant;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.ProductType;
import uz.agrogram.events.v1.TransactionCreated;
import uz.agrogram.events.v1.TransactionEvent;
import uz.agrogram.events.v1.TransactionSnapshot;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.domain.Amounts;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.TaxPolicy;
import uz.agrogram.orderescrow.testing.Fixtures;

class TransactionEventsTest {

    private final UUID eventId = UUID.fromString("0192f0c4-5d7e-7a3b-8c1d-2e3f4a5b6c7d");
    private final TransactionEvents events = new TransactionEvents(Fixtures.VALIDATOR, () -> eventId);
    private final Instant now = Instant.parse("2026-09-01T08:00:00Z");

    private EscrowTransaction transaction() {
        UUID farmer = Fixtures.id();
        UUID reseller = Fixtures.id();
        return EscrowTransaction.builder()
                .id(Fixtures.id()).listingId(Fixtures.id()).listingKind(ListingKind.LISTING_KIND_OFFER)
                .farmerId(farmer).resellerId(reseller).initiatorId(reseller)
                .product(ProductType.PRODUCT_TYPE_POMIDOR).quantityGrams(1_000).quantityUnit(MassUnit.MASS_UNIT_KILOGRAM)
                .unitPriceMinor(600_000).unitPriceUnit(MassUnit.MASS_UNIT_KILOGRAM).address(Fixtures.address())
                .amounts(Amounts.compute(600_000, MassUnit.MASS_UNIT_KILOGRAM, 1_000, TaxPolicy.NONE))
                .createdAt(now).updatedAt(now).build();
    }

    @Test
    void withoutTraceContextTheTraceHeadersAreLeftOut() throws Exception {
        EscrowTransaction t = transaction();
        OutboxMessage m = events.build(t, TransactionCreated.getDefaultInstance(), Principal.serviceActor(), now,
                new TransactionEvents.Origin("req-1", "", ""), null);
        assertThat(m.headers()).containsOnlyKeys("content-type", "event-type", "event-id", "x-request-id");
        TransactionEvent ev = TransactionEvent.parseFrom(m.value());
        assertThat(ev.getMetadata().getTraceparent()).isEmpty();
        assertThat(ev.getMetadata().getOccurredAt().getSeconds()).isEqualTo(now.getEpochSecond());
        assertThat(m.id()).isEqualTo(eventId);
        assertThat(m.createdAt()).isEqualTo(now);
    }

    @Test
    void anEventThatFailsValidationIsNeverBuilt() {
        // correlation_id must not be empty (metadata.proto): a bug, not a client error.
        assertThatThrownBy(() -> events.build(transaction(), TransactionCreated.getDefaultInstance(),
                Principal.serviceActor(), now, new TransactionEvents.Origin("", "", ""), null))
                .isInstanceOf(IllegalStateException.class).hasMessageContaining("TransactionEvent");
    }

    @Test
    void onlyTransactionEventCasesAreAccepted() {
        assertThatThrownBy(() -> events.build(transaction(), TransactionSnapshot.getDefaultInstance(),
                Principal.serviceActor(), now, new TransactionEvents.Origin("r", "", ""), null))
                .isInstanceOf(IllegalArgumentException.class);
    }
}
