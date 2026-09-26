package uz.agrogram.orderescrow.domain;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.time.Instant;
import java.util.UUID;
import org.junit.jupiter.api.Test;
import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.ProductType;
import uz.agrogram.orderescrow.testing.Fixtures;

class EscrowTransactionTest {

    private final UUID farmer = Fixtures.id();
    private final UUID reseller = Fixtures.id();
    private final Instant t0 = Instant.parse("2026-09-01T08:00:00Z");

    private EscrowTransaction created() {
        return EscrowTransaction.builder()
                .id(Fixtures.id())
                .listingId(Fixtures.id())
                .listingKind(ListingKind.LISTING_KIND_OFFER)
                .listingVersion(1)
                .farmerId(farmer)
                .resellerId(reseller)
                .initiatorId(reseller)
                .product(ProductType.PRODUCT_TYPE_POMIDOR)
                .quantityGrams(1_000_000)
                .quantityUnit(MassUnit.MASS_UNIT_TONNE)
                .unitPriceMinor(600_000)
                .unitPriceUnit(MassUnit.MASS_UNIT_KILOGRAM)
                .address(Fixtures.address())
                .amounts(Amounts.compute(600_000, MassUnit.MASS_UNIT_KILOGRAM, 1_000_000, TaxPolicy.NONE))
                .createdAt(t0)
                .updatedAt(t0)
                .build();
    }

    private EscrowTransaction completed() {
        EscrowTransaction t = created();
        PaymentRecord payment = PaymentRecord.stubHeld(Fixtures.id(), t.amounts().totalMinor(), t0);
        t = t.payIn(payment, t0);
        t = t.submitReceipt(ReceiptRecord.stubVerified("T-1", null, 0, MassUnit.MASS_UNIT_UNSPECIFIED, reseller, t0),
                t0);
        t = t.confirmHandover(t0).confirmReceipt(t0);
        return t.releasePayout(PayoutRecord.stubReleased(Fixtures.id(), payment.id(), farmer,
                t.amounts().payoutMinor(), t0), t0);
    }

    @Test
    void everyStepAddsOneVersionAndMoneyIsHeldThenReleasedExactlyOnce() {
        EscrowTransaction t = created();
        assertThat(t.version()).isEqualTo(1);
        assertThat(t.payment()).isEmpty();

        EscrowTransaction done = completed();
        assertThat(done.state()).isEqualTo(EscrowState.COMPLETED);
        assertThat(done.version()).isEqualTo(6);
        assertThat(done.payment().orElseThrow().amountMinor()).isEqualTo(done.amounts().totalMinor());
        assertThat(done.payment().orElseThrow().released()).isTrue();
        assertThat(done.payout().orElseThrow().amountMinor()).isEqualTo(done.amounts().payoutMinor());
        assertThat(done.payout().orElseThrow().recipientId()).isEqualTo(farmer);
        // total held == payout + platform_fee + tax
        assertThat(done.payment().orElseThrow().amountMinor()).isEqualTo(done.payout().orElseThrow().amountMinor()
                + done.amounts().platformFeeMinor() + done.amounts().taxMinor());
        assertThat(done.completedAt()).isEqualTo(t0);

        EscrowTransaction bonus = done.awardBonus(5, t0);
        assertThat(bonus.version()).isEqualTo(7);
        assertThat(bonus.bonusPoints()).isEqualTo(5);
        assertThat(bonus.state()).isEqualTo(EscrowState.COMPLETED);
    }

    @Test
    void stepsOutOfOrderAreBugs() {
        EscrowTransaction t = created();
        assertThatThrownBy(() -> t.confirmHandover(t0)).isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> t.confirmReceipt(t0)).isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> t.awardBonus(1, t0)).isInstanceOf(IllegalStateException.class);
        EscrowTransaction done = completed();
        assertThatThrownBy(() -> done.releasePayout(done.payout().orElseThrow(), t0))
                .isInstanceOf(IllegalStateException.class);
        EscrowTransaction bonus = done.awardBonus(1, t0);
        assertThatThrownBy(() -> bonus.awardBonus(1, t0)).isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> done.awardBonus(0, t0)).isInstanceOf(IllegalStateException.class);
    }

    @Test
    void aPaymentMustEqualTheTotal() {
        EscrowTransaction t = created();
        PaymentRecord wrong = PaymentRecord.stubHeld(Fixtures.id(), t.amounts().totalMinor() - 1, t0);
        assertThatThrownBy(() -> t.payIn(wrong, t0)).isInstanceOf(IllegalStateException.class)
                .hasMessageContaining("amounts.total");
    }

    @Test
    void aPayoutMustEqualThePayoutAmountGoToTheFarmerAndReleaseThePayment() {
        EscrowTransaction t = created();
        PaymentRecord payment = PaymentRecord.stubHeld(Fixtures.id(), t.amounts().totalMinor(), t0);
        EscrowTransaction received = t.payIn(payment, t0)
                .submitReceipt(ReceiptRecord.stubVerified("T-1", null, 0, MassUnit.MASS_UNIT_UNSPECIFIED, reseller,
                        t0), t0)
                .confirmHandover(t0).confirmReceipt(t0);
        long payout = received.amounts().payoutMinor();
        assertThatThrownBy(() -> received.releasePayout(
                PayoutRecord.stubReleased(Fixtures.id(), payment.id(), farmer, payout + 1, t0), t0))
                .isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> received.releasePayout(
                PayoutRecord.stubReleased(Fixtures.id(), payment.id(), reseller, payout, t0), t0))
                .isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> received.releasePayout(
                PayoutRecord.stubReleased(Fixtures.id(), Fixtures.id(), farmer, payout, t0), t0))
                .isInstanceOf(IllegalStateException.class);
    }

    @Test
    void thePartiesMustDifferAndTheInitiatorIsTheListingOwnersCounterparty() {
        assertThatThrownBy(() -> created().toBuilder().resellerId(farmer).initiatorId(farmer).build())
                .isInstanceOf(IllegalStateException.class);
        // On an offer the reseller starts the transaction, not the farmer.
        assertThatThrownBy(() -> created().toBuilder().initiatorId(farmer).build())
                .isInstanceOf(IllegalStateException.class);
        EscrowTransaction onBuyRequest = created().toBuilder().listingKind(ListingKind.LISTING_KIND_BUY_REQUEST)
                .initiatorId(farmer).build();
        assertThat(onBuyRequest.listingOwnerId()).isEqualTo(reseller);
        assertThat(created().listingOwnerId()).isEqualTo(farmer);
    }

    @Test
    void recordsAndStateMustAgree() {
        assertThatThrownBy(() -> created().toBuilder().state(EscrowState.PAYMENT_HELD).build())
                .isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> created().toBuilder().bonusPoints(1).build())
                .isInstanceOf(IllegalStateException.class);
        assertThatThrownBy(() -> completed().toBuilder().payout(null).build())
                .isInstanceOf(IllegalStateException.class);
    }

    @Test
    void partyOfKnowsBothSides() {
        EscrowTransaction t = created();
        assertThat(t.partyOf(farmer)).contains(Party.FARMER);
        assertThat(t.partyOf(reseller)).contains(Party.RESELLER);
        assertThat(t.partyOf(Fixtures.id())).isEmpty();
    }
}
