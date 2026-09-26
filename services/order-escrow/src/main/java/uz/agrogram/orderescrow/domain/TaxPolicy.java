package uz.agrogram.orderescrow.domain;

/**
 * The tax hook of a transaction's amounts.
 *
 * <p>TODO(tax): the tax regime is not designed (transaction.proto,
 * TransactionAmounts.tax). This is the single place to plug it in. Today the
 * tax is added on top of the subtotal (total = subtotal + tax + platform_fee,
 * payout_amount = subtotal). If the regime withholds tax from the farmer
 * instead, payout_amount becomes subtotal - tax and the contract comment, the
 * Amounts invariants and the escrow_transactions CHECK constraints change with
 * it; money stays conserved either way (total = payout + tax + platform_fee).
 */
@FunctionalInterface
public interface TaxPolicy {

    /**
     * STUB: TODO(tax) no tax regime yet, so the tax is always zero. Amounts are
     * fixed when a transaction is created and never recomputed, so a later rule
     * applies to new transactions only.
     */
    TaxPolicy NONE = subtotalMinor -> 0L;

    /** Tax, in tiyin, on a transaction whose subtotal is {@code subtotalMinor}. Never negative. */
    long taxMinor(long subtotalMinor);
}
