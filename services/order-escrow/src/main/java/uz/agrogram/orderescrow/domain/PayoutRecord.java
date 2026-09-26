package uz.agrogram.orderescrow.domain;

import java.time.Instant;
import java.util.Objects;
import java.util.UUID;

/**
 * The release of the escrowed funds to the farmer (table payouts: UNIQUE per
 * transaction and per payment, so a transaction is paid out at most once).
 *
 * <p>STUB: TODO(payments) no provider and no MCHJ account: {@link #stubReleased}
 * records the payout as released at once and no money moves. TODO(tax): the
 * amount may later exclude withheld tax.
 *
 * @param id internal payout id (UUIDv7)
 * @param paymentId the pay-in whose hold this payout releases
 * @param recipientId the farmer
 * @param provider payment provider; "stub" for now
 * @param providerPayoutId the provider's id; "" with the stub
 * @param amountMinor amount paid out, equals amounts.payout_amount
 * @param releasedAt when the payout was released
 */
public record PayoutRecord(
        UUID id,
        UUID paymentId,
        UUID recipientId,
        String provider,
        String providerPayoutId,
        long amountMinor,
        Instant releasedAt) {

    public PayoutRecord {
        Objects.requireNonNull(id, "id");
        Objects.requireNonNull(paymentId, "paymentId");
        Objects.requireNonNull(recipientId, "recipientId");
        Objects.requireNonNull(provider, "provider");
        Objects.requireNonNull(providerPayoutId, "providerPayoutId");
        Objects.requireNonNull(releasedAt, "releasedAt");
        if (amountMinor <= 0) {
            throw new IllegalArgumentException("payout amount must be positive");
        }
    }

    /** STUB: TODO(payments) a payout released immediately, without a provider. */
    public static PayoutRecord stubReleased(UUID id, UUID paymentId, UUID farmerId, long amountMinor, Instant now) {
        return new PayoutRecord(id, paymentId, farmerId, PaymentRecord.STUB_PROVIDER, "", amountMinor, now);
    }
}
