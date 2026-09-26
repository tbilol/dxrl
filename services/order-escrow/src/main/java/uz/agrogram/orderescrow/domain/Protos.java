package uz.agrogram.orderescrow.domain;

import com.google.protobuf.Timestamp;
import java.time.Instant;
import java.util.List;
import uz.agrogram.common.v1.Mass;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.Role;
import uz.agrogram.common.v1.UnitPrice;
import uz.agrogram.events.v1.TransactionSnapshot;
import uz.agrogram.events.v1.WeighbridgeReceipt;
import uz.agrogram.orderescrow.v1.Payment;
import uz.agrogram.orderescrow.v1.Payout;
import uz.agrogram.orderescrow.v1.Transaction;
import uz.agrogram.orderescrow.v1.TransactionStateChange;

/** Maps the domain to the contract messages: the API Transaction and the event TransactionSnapshot. */
public final class Protos {

    private Protos() {}

    public static Timestamp timestamp(Instant i) {
        return Timestamp.newBuilder().setSeconds(i.getEpochSecond()).setNanos(i.getNano()).build();
    }

    public static Mass mass(long grams, MassUnit unit) {
        return Mass.newBuilder().setGrams(grams).setDisplayUnit(unit).build();
    }

    public static UnitPrice unitPrice(long amountMinor, MassUnit unit) {
        return UnitPrice.newBuilder().setAmount(Amounts.money(amountMinor)).setUnit(unit).build();
    }

    public static WeighbridgeReceipt receipt(ReceiptRecord r) {
        WeighbridgeReceipt.Builder b = WeighbridgeReceipt.newBuilder()
                .setTicketNumber(r.ticketNumber())
                .setVerificationStatus(r.verificationStatus())
                .setSubmittedAt(timestamp(r.submittedAt()));
        if (r.photoMediaId() != null) {
            b.setPhotoMediaId(r.photoMediaId().toString());
        }
        if (r.netMassGrams() > 0) {
            b.setNetMass(mass(r.netMassGrams(), r.netMassUnit()));
        }
        if (r.verifiedAt() != null) {
            b.setVerifiedAt(timestamp(r.verifiedAt()));
        }
        return b.build();
    }

    /**
     * The event snapshot: the transaction's state after an event. No contact
     * details and no payment-provider references (transaction.proto).
     */
    public static TransactionSnapshot snapshot(EscrowTransaction t) {
        TransactionSnapshot.Builder b = TransactionSnapshot.newBuilder()
                .setId(t.id().toString())
                .setListingId(t.listingId().toString())
                .setListingKind(t.listingKind())
                .setFarmerId(t.farmerId().toString())
                .setResellerId(t.resellerId().toString())
                .setProduct(t.product())
                .setQuantity(mass(t.quantityGrams(), t.quantityUnit()))
                .setUnitPrice(unitPrice(t.unitPriceMinor(), t.unitPriceUnit()))
                .setAddress(t.address())
                .setAmounts(t.amounts().toProto())
                .setState(t.state().proto())
                .setBonusPoints(t.bonusPoints())
                .setVersion(t.version())
                .setCreatedAt(timestamp(t.createdAt()))
                .setUpdatedAt(timestamp(t.updatedAt()));
        t.receipt().ifPresent(r -> b.setReceipt(receipt(r)));
        if (t.paymentHeldAt() != null) {
            b.setPaymentHeldAt(timestamp(t.paymentHeldAt()));
        }
        if (t.completedAt() != null) {
            b.setCompletedAt(timestamp(t.completedAt()));
        }
        return b.build();
    }

    public static TransactionStateChange stateChange(StateChange c) {
        return TransactionStateChange.newBuilder()
                .setState(c.state().proto())
                .setActor(c.actor())
                .setChangedAt(timestamp(c.changedAt()))
                .build();
    }

    /** The API view of a transaction for a caller on side {@code myRole} (ROLE_ADMIN for staff). */
    public static Transaction transaction(EscrowTransaction t, Role myRole, List<StateChange> history) {
        Transaction.Builder b = Transaction.newBuilder()
                .setId(t.id().toString())
                .setListingId(t.listingId().toString())
                .setListingKind(t.listingKind())
                .setFarmerId(t.farmerId().toString())
                .setResellerId(t.resellerId().toString())
                .setProduct(t.product())
                .setQuantity(mass(t.quantityGrams(), t.quantityUnit()))
                .setUnitPrice(unitPrice(t.unitPriceMinor(), t.unitPriceUnit()))
                .setAddress(t.address())
                .setAmounts(t.amounts().toProto())
                .setState(t.state().proto())
                .setBonusPoints(t.bonusPoints())
                .setMyRole(myRole)
                .setVersion(t.version())
                .setCreatedAt(timestamp(t.createdAt()))
                .setUpdatedAt(timestamp(t.updatedAt()));
        t.payment().ifPresent(p -> b.setPayment(Payment.newBuilder()
                .setProvider(p.provider())
                .setProviderPaymentId(p.providerPaymentId())
                .setAmount(Amounts.money(p.amountMinor()))
                .setHeldAt(timestamp(p.heldAt()))));
        t.receipt().ifPresent(r -> b.setReceipt(receipt(r)));
        t.payout().ifPresent(p -> b.setPayout(Payout.newBuilder()
                .setProvider(p.provider())
                .setProviderPayoutId(p.providerPayoutId())
                .setAmount(Amounts.money(p.amountMinor()))
                .setReleasedAt(timestamp(p.releasedAt()))));
        if (t.completedAt() != null) {
            b.setCompletedAt(timestamp(t.completedAt()));
        }
        for (StateChange c : history) {
            b.addHistory(stateChange(c));
        }
        return b.build();
    }
}
