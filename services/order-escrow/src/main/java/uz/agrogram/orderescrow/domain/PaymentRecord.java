package uz.agrogram.orderescrow.domain;

import java.time.Instant;
import java.util.Objects;
import java.util.UUID;

/**
 * The reseller's pay-in into escrow (table payments, one per transaction).
 *
 * <p>STUB: TODO(payments) there is no payment provider (Payme, Click, bank
 * transfer, ...) and no MCHJ escrow account yet. {@link #stubHeld} records the
 * funds as held at once: provider "stub", no provider payment id, and no money
 * moves. A provider integration would create the record on checkout and mark it
 * held when the provider confirms.
 *
 * @param id internal payment id (UUIDv7), the reference of the hold
 * @param provider payment provider; "stub" for now
 * @param providerPaymentId the provider's id; "" with the stub
 * @param amountMinor amount held, equals the transaction's amounts.total
 * @param heldAt when escrow started holding the funds
 * @param releasedAt when the hold was released as the payout; null while held
 */
public record PaymentRecord(
        UUID id,
        String provider,
        String providerPaymentId,
        long amountMinor,
        Instant heldAt,
        Instant releasedAt) {

    /** Provider name of the stub (order_escrow.proto, Payment). */
    public static final String STUB_PROVIDER = "stub";

    public PaymentRecord {
        Objects.requireNonNull(id, "id");
        Objects.requireNonNull(provider, "provider");
        Objects.requireNonNull(providerPaymentId, "providerPaymentId");
        Objects.requireNonNull(heldAt, "heldAt");
        if (amountMinor <= 0) {
            throw new IllegalArgumentException("payment amount must be positive");
        }
    }

    /** STUB: TODO(payments) a pay-in that is held immediately, without a provider. */
    public static PaymentRecord stubHeld(UUID id, long amountMinor, Instant now) {
        return new PaymentRecord(id, STUB_PROVIDER, "", amountMinor, now, null);
    }

    public boolean released() {
        return releasedAt != null;
    }

    public PaymentRecord release(Instant at) {
        if (released()) {
            throw new IllegalStateException("payment " + id + " is already released");
        }
        return new PaymentRecord(id, provider, providerPaymentId, amountMinor, heldAt, at);
    }
}
