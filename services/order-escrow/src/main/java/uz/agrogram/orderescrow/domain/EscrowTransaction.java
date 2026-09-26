package uz.agrogram.orderescrow.domain;

import java.time.Instant;
import java.util.Objects;
import java.util.Optional;
import java.util.UUID;
import uz.agrogram.common.v1.Address;
import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.ProductType;

/**
 * An escrow transaction (tranzaksiya): the aggregate behind TransactionSnapshot.
 *
 * <p>Immutable. Every step returns a new instance at {@code version + 1}; the
 * repository stores it with a conditional update on the old version. The
 * constructor enforces the invariants that keep the money straight, so a bug
 * cannot produce an inconsistent transaction:
 *
 * <ul>
 *   <li>the parties differ, and the initiator is the counterparty of the
 *       listing owner (the reseller on an offer, the farmer on a buy-request);
 *   <li>a payment exists from PAYMENT_HELD on, for exactly amounts.total;
 *   <li>a receipt exists from RECEIPT_SUBMITTED on;
 *   <li>a payout exists exactly when COMPLETED, for exactly amounts.payout_amount,
 *       to the farmer, releasing that payment (released iff paid out).
 * </ul>
 */
public final class EscrowTransaction {

    private final UUID id;
    private final UUID listingId;
    private final ListingKind listingKind;
    private final long listingVersion;
    private final UUID farmerId;
    private final UUID resellerId;
    private final UUID initiatorId;
    private final ProductType product;
    private final long quantityGrams;
    private final MassUnit quantityUnit;
    private final long unitPriceMinor;
    private final MassUnit unitPriceUnit;
    private final Address address;
    private final Amounts amounts;
    private final EscrowState state;
    private final long bonusPoints;
    private final long version;
    private final Instant createdAt;
    private final Instant updatedAt;
    private final Instant paymentHeldAt;
    private final Instant receiptSubmittedAt;
    private final Instant handedOverAt;
    private final Instant receivedAt;
    private final Instant completedAt;
    private final PaymentRecord payment;
    private final ReceiptRecord receipt;
    private final PayoutRecord payout;

    private EscrowTransaction(Builder b) {
        id = Objects.requireNonNull(b.id, "id");
        listingId = Objects.requireNonNull(b.listingId, "listingId");
        listingKind = Objects.requireNonNull(b.listingKind, "listingKind");
        listingVersion = b.listingVersion;
        farmerId = Objects.requireNonNull(b.farmerId, "farmerId");
        resellerId = Objects.requireNonNull(b.resellerId, "resellerId");
        initiatorId = Objects.requireNonNull(b.initiatorId, "initiatorId");
        product = Objects.requireNonNull(b.product, "product");
        quantityGrams = b.quantityGrams;
        quantityUnit = Objects.requireNonNull(b.quantityUnit, "quantityUnit");
        unitPriceMinor = b.unitPriceMinor;
        unitPriceUnit = Objects.requireNonNull(b.unitPriceUnit, "unitPriceUnit");
        address = Objects.requireNonNull(b.address, "address");
        amounts = Objects.requireNonNull(b.amounts, "amounts");
        state = Objects.requireNonNull(b.state, "state");
        bonusPoints = b.bonusPoints;
        version = b.version;
        createdAt = Objects.requireNonNull(b.createdAt, "createdAt");
        updatedAt = Objects.requireNonNull(b.updatedAt, "updatedAt");
        paymentHeldAt = b.paymentHeldAt;
        receiptSubmittedAt = b.receiptSubmittedAt;
        handedOverAt = b.handedOverAt;
        receivedAt = b.receivedAt;
        completedAt = b.completedAt;
        payment = b.payment;
        receipt = b.receipt;
        payout = b.payout;
        checkInvariants();
    }

    private void checkInvariants() {
        require(!farmerId.equals(resellerId), "farmer and reseller must differ");
        UUID expectedInitiator = listingKind == ListingKind.LISTING_KIND_OFFER ? resellerId : farmerId;
        require(listingKind == ListingKind.LISTING_KIND_OFFER || listingKind == ListingKind.LISTING_KIND_BUY_REQUEST,
                "listing kind must be OFFER or BUY_REQUEST");
        require(initiatorId.equals(expectedInitiator), "the initiator must be the counterparty of the listing owner");
        require(quantityGrams > 0, "quantity must be positive");
        require(unitPriceMinor > 0, "unit price must be positive");
        require(version >= 1, "version must be at least 1");
        require(bonusPoints >= 0, "bonus points must not be negative");
        require(amounts.isConserved(), "amounts must be conserved");

        boolean paid = state != EscrowState.AWAITING_PAYMENT;
        require(paid == (payment != null), "a payment exists exactly from PAYMENT_HELD on");
        require(paid == (paymentHeldAt != null), "payment_held_at is set exactly from PAYMENT_HELD on");
        if (payment != null) {
            require(payment.amountMinor() == amounts.totalMinor(), "the payment must equal amounts.total");
        }
        boolean receiptDue = state.compareTo(EscrowState.RECEIPT_SUBMITTED) >= 0;
        require(receiptDue == (receipt != null), "a receipt exists exactly from RECEIPT_SUBMITTED on");
        boolean completed = state == EscrowState.COMPLETED;
        require(completed == (payout != null), "a payout exists exactly when COMPLETED");
        require(completed == (completedAt != null), "completed_at is set exactly when COMPLETED");
        if (payout != null) {
            require(payout.amountMinor() == amounts.payoutMinor(), "the payout must equal amounts.payout_amount");
            require(payout.recipientId().equals(farmerId), "the payout goes to the farmer");
            require(payout.paymentId().equals(payment.id()), "the payout releases the transaction's payment");
        }
        if (payment != null) {
            require(payment.released() == completed, "the hold is released exactly by the payout");
        }
        require(bonusPoints == 0 || completed, "bonus points are awarded on completion only");
    }

    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new IllegalStateException("escrow transaction invariant violated: " + message);
        }
    }

    private void requireState(EscrowState expected) {
        if (state != expected) {
            throw new IllegalStateException("transaction " + id + " is " + state + ", not " + expected);
        }
    }

    // ---- Steps (the service checks the actor with StateMachine first) -------

    /** AWAITING_PAYMENT -> PAYMENT_HELD. */
    public EscrowTransaction payIn(PaymentRecord held, Instant now) {
        requireState(Command.PAY_IN.from());
        return toBuilder().state(Command.PAY_IN.to()).payment(held).paymentHeldAt(held.heldAt())
                .updatedAt(now).version(version + 1).build();
    }

    /** PAYMENT_HELD -> RECEIPT_SUBMITTED. */
    public EscrowTransaction submitReceipt(ReceiptRecord submitted, Instant now) {
        requireState(Command.SUBMIT_WEIGHBRIDGE_RECEIPT.from());
        return toBuilder().state(Command.SUBMIT_WEIGHBRIDGE_RECEIPT.to()).receipt(submitted)
                .receiptSubmittedAt(now).updatedAt(now).version(version + 1).build();
    }

    /** RECEIPT_SUBMITTED -> HANDED_OVER. */
    public EscrowTransaction confirmHandover(Instant now) {
        requireState(Command.CONFIRM_HANDOVER.from());
        return toBuilder().state(Command.CONFIRM_HANDOVER.to()).handedOverAt(now).updatedAt(now)
                .version(version + 1).build();
    }

    /** HANDED_OVER -> RECEIVED. */
    public EscrowTransaction confirmReceipt(Instant now) {
        requireState(Command.CONFIRM_RECEIPT.from());
        return toBuilder().state(Command.CONFIRM_RECEIPT.to()).receivedAt(now).updatedAt(now)
                .version(version + 1).build();
    }

    /** RECEIVED -> COMPLETED: the payout releases the payment's hold. */
    public EscrowTransaction releasePayout(PayoutRecord released, Instant now) {
        requireState(Command.RELEASE_PAYOUT.from());
        return toBuilder().state(Command.RELEASE_PAYOUT.to()).payout(released)
                .payment(payment.release(released.releasedAt())).completedAt(released.releasedAt())
                .updatedAt(now).version(version + 1).build();
    }

    /** Credits bonus points on a COMPLETED transaction (no state change; the snapshot changes). */
    public EscrowTransaction awardBonus(long points, Instant now) {
        requireState(EscrowState.COMPLETED);
        if (points <= 0 || bonusPoints != 0) {
            throw new IllegalStateException("bonus points are awarded once and must be positive");
        }
        return toBuilder().bonusPoints(points).updatedAt(now).version(version + 1).build();
    }

    // ---- Queries --------------------------------------------------------------

    /** The side {@code userId} is on, if any. */
    public Optional<Party> partyOf(UUID userId) {
        if (farmerId.equals(userId)) {
            return Optional.of(Party.FARMER);
        }
        if (resellerId.equals(userId)) {
            return Optional.of(Party.RESELLER);
        }
        return Optional.empty();
    }

    /** The owner of the listing: the farmer of an offer, the reseller of a buy-request. */
    public UUID listingOwnerId() {
        return listingKind == ListingKind.LISTING_KIND_OFFER ? farmerId : resellerId;
    }

    public UUID id() {
        return id;
    }

    public UUID listingId() {
        return listingId;
    }

    public ListingKind listingKind() {
        return listingKind;
    }

    public long listingVersion() {
        return listingVersion;
    }

    public UUID farmerId() {
        return farmerId;
    }

    public UUID resellerId() {
        return resellerId;
    }

    public UUID initiatorId() {
        return initiatorId;
    }

    public ProductType product() {
        return product;
    }

    public long quantityGrams() {
        return quantityGrams;
    }

    public MassUnit quantityUnit() {
        return quantityUnit;
    }

    public long unitPriceMinor() {
        return unitPriceMinor;
    }

    public MassUnit unitPriceUnit() {
        return unitPriceUnit;
    }

    public Address address() {
        return address;
    }

    public Amounts amounts() {
        return amounts;
    }

    public EscrowState state() {
        return state;
    }

    public long bonusPoints() {
        return bonusPoints;
    }

    public long version() {
        return version;
    }

    public Instant createdAt() {
        return createdAt;
    }

    public Instant updatedAt() {
        return updatedAt;
    }

    public Instant paymentHeldAt() {
        return paymentHeldAt;
    }

    public Instant receiptSubmittedAt() {
        return receiptSubmittedAt;
    }

    public Instant handedOverAt() {
        return handedOverAt;
    }

    public Instant receivedAt() {
        return receivedAt;
    }

    public Instant completedAt() {
        return completedAt;
    }

    public Optional<PaymentRecord> payment() {
        return Optional.ofNullable(payment);
    }

    public Optional<ReceiptRecord> receipt() {
        return Optional.ofNullable(receipt);
    }

    public Optional<PayoutRecord> payout() {
        return Optional.ofNullable(payout);
    }

    @Override
    public boolean equals(Object o) {
        if (this == o) {
            return true;
        }
        if (!(o instanceof EscrowTransaction t)) {
            return false;
        }
        return listingVersion == t.listingVersion && quantityGrams == t.quantityGrams
                && unitPriceMinor == t.unitPriceMinor && bonusPoints == t.bonusPoints && version == t.version
                && id.equals(t.id) && listingId.equals(t.listingId) && listingKind == t.listingKind
                && farmerId.equals(t.farmerId) && resellerId.equals(t.resellerId) && initiatorId.equals(t.initiatorId)
                && product == t.product && quantityUnit == t.quantityUnit && unitPriceUnit == t.unitPriceUnit
                && address.equals(t.address) && amounts.equals(t.amounts) && state == t.state
                && createdAt.equals(t.createdAt) && updatedAt.equals(t.updatedAt)
                && Objects.equals(paymentHeldAt, t.paymentHeldAt)
                && Objects.equals(receiptSubmittedAt, t.receiptSubmittedAt)
                && Objects.equals(handedOverAt, t.handedOverAt) && Objects.equals(receivedAt, t.receivedAt)
                && Objects.equals(completedAt, t.completedAt) && Objects.equals(payment, t.payment)
                && Objects.equals(receipt, t.receipt) && Objects.equals(payout, t.payout);
    }

    @Override
    public int hashCode() {
        return Objects.hash(id, version);
    }

    @Override
    public String toString() {
        return "EscrowTransaction{id=" + id + ", state=" + state + ", version=" + version + "}";
    }

    public static Builder builder() {
        return new Builder();
    }

    public Builder toBuilder() {
        Builder b = new Builder();
        b.id = id;
        b.listingId = listingId;
        b.listingKind = listingKind;
        b.listingVersion = listingVersion;
        b.farmerId = farmerId;
        b.resellerId = resellerId;
        b.initiatorId = initiatorId;
        b.product = product;
        b.quantityGrams = quantityGrams;
        b.quantityUnit = quantityUnit;
        b.unitPriceMinor = unitPriceMinor;
        b.unitPriceUnit = unitPriceUnit;
        b.address = address;
        b.amounts = amounts;
        b.state = state;
        b.bonusPoints = bonusPoints;
        b.version = version;
        b.createdAt = createdAt;
        b.updatedAt = updatedAt;
        b.paymentHeldAt = paymentHeldAt;
        b.receiptSubmittedAt = receiptSubmittedAt;
        b.handedOverAt = handedOverAt;
        b.receivedAt = receivedAt;
        b.completedAt = completedAt;
        b.payment = payment;
        b.receipt = receipt;
        b.payout = payout;
        return b;
    }

    /** Builds a transaction; {@link #build()} checks the invariants. */
    public static final class Builder {
        private UUID id;
        private UUID listingId;
        private ListingKind listingKind;
        private long listingVersion;
        private UUID farmerId;
        private UUID resellerId;
        private UUID initiatorId;
        private ProductType product;
        private long quantityGrams;
        private MassUnit quantityUnit = MassUnit.MASS_UNIT_UNSPECIFIED;
        private long unitPriceMinor;
        private MassUnit unitPriceUnit;
        private Address address;
        private Amounts amounts;
        private EscrowState state = EscrowState.AWAITING_PAYMENT;
        private long bonusPoints;
        private long version = 1;
        private Instant createdAt;
        private Instant updatedAt;
        private Instant paymentHeldAt;
        private Instant receiptSubmittedAt;
        private Instant handedOverAt;
        private Instant receivedAt;
        private Instant completedAt;
        private PaymentRecord payment;
        private ReceiptRecord receipt;
        private PayoutRecord payout;

        private Builder() {}

        public Builder id(UUID v) {
            id = v;
            return this;
        }

        public Builder listingId(UUID v) {
            listingId = v;
            return this;
        }

        public Builder listingKind(ListingKind v) {
            listingKind = v;
            return this;
        }

        public Builder listingVersion(long v) {
            listingVersion = v;
            return this;
        }

        public Builder farmerId(UUID v) {
            farmerId = v;
            return this;
        }

        public Builder resellerId(UUID v) {
            resellerId = v;
            return this;
        }

        public Builder initiatorId(UUID v) {
            initiatorId = v;
            return this;
        }

        public Builder product(ProductType v) {
            product = v;
            return this;
        }

        public Builder quantityGrams(long v) {
            quantityGrams = v;
            return this;
        }

        public Builder quantityUnit(MassUnit v) {
            quantityUnit = v;
            return this;
        }

        public Builder unitPriceMinor(long v) {
            unitPriceMinor = v;
            return this;
        }

        public Builder unitPriceUnit(MassUnit v) {
            unitPriceUnit = v;
            return this;
        }

        public Builder address(Address v) {
            address = v;
            return this;
        }

        public Builder amounts(Amounts v) {
            amounts = v;
            return this;
        }

        public Builder state(EscrowState v) {
            state = v;
            return this;
        }

        public Builder bonusPoints(long v) {
            bonusPoints = v;
            return this;
        }

        public Builder version(long v) {
            version = v;
            return this;
        }

        public Builder createdAt(Instant v) {
            createdAt = v;
            return this;
        }

        public Builder updatedAt(Instant v) {
            updatedAt = v;
            return this;
        }

        public Builder paymentHeldAt(Instant v) {
            paymentHeldAt = v;
            return this;
        }

        public Builder receiptSubmittedAt(Instant v) {
            receiptSubmittedAt = v;
            return this;
        }

        public Builder handedOverAt(Instant v) {
            handedOverAt = v;
            return this;
        }

        public Builder receivedAt(Instant v) {
            receivedAt = v;
            return this;
        }

        public Builder completedAt(Instant v) {
            completedAt = v;
            return this;
        }

        public Builder payment(PaymentRecord v) {
            payment = v;
            return this;
        }

        public Builder receipt(ReceiptRecord v) {
            receipt = v;
            return this;
        }

        public Builder payout(PayoutRecord v) {
            payout = v;
            return this;
        }

        public EscrowTransaction build() {
            return new EscrowTransaction(this);
        }
    }
}
