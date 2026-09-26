package uz.agrogram.orderescrow.domain;

import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.Money;
import uz.agrogram.events.v1.TransactionAmounts;

/**
 * The money side of a transaction (TransactionAmounts), in tiyin, fixed when the
 * transaction is created and never recomputed.
 *
 * <pre>
 *   subtotal      = round_half_up(unit_price x quantity)   (MoneyMath.lineTotal)
 *   tax           = TaxPolicy (STUB: 0, TODO(tax))
 *   platform_fee  = 0 (TODO(payments): commission-free)
 *   total         = subtotal + tax + platform_fee          (what the reseller pays in)
 *   payout_amount = subtotal                               (what the farmer receives)
 * </pre>
 *
 * Money is conserved: every tiyin held is released exactly once,
 * {@code total == payout_amount + tax + platform_fee}. The constructor refuses
 * any other combination, and escrow_transactions has the same CHECKs.
 */
public record Amounts(long subtotalMinor, long taxMinor, long platformFeeMinor, long totalMinor, long payoutMinor) {

    /** Every trade amount is in Uzbek so'm (money.proto). */
    public static final String CURRENCY = "UZS";

    public Amounts {
        if (subtotalMinor <= 0) {
            throw new IllegalArgumentException("subtotal must be positive");
        }
        if (taxMinor < 0 || platformFeeMinor < 0) {
            throw new IllegalArgumentException("tax and platform fee must not be negative");
        }
        if (totalMinor != Math.addExact(Math.addExact(subtotalMinor, taxMinor), platformFeeMinor)) {
            throw new IllegalArgumentException("total must equal subtotal + tax + platform_fee");
        }
        // TODO(tax): relax when tax is withheld from the payout instead of added on top.
        if (payoutMinor != subtotalMinor) {
            throw new IllegalArgumentException("payout_amount must equal subtotal");
        }
    }

    /**
     * Computes the amounts of {@code grams} at {@code unitPriceMinor} per
     * {@code priceUnit}.
     *
     * @throws ArithmeticException when an amount does not fit in a long
     * @throws IllegalArgumentException when the subtotal rounds to zero
     */
    public static Amounts compute(long unitPriceMinor, MassUnit priceUnit, long grams, TaxPolicy taxPolicy) {
        long subtotal = MoneyMath.lineTotal(unitPriceMinor, priceUnit, grams);
        long tax = taxPolicy.taxMinor(subtotal);
        // TODO(payments): payments are commission-free today; a fee would be added here.
        long platformFee = 0L;
        long total = Math.addExact(Math.addExact(subtotal, tax), platformFee);
        return new Amounts(subtotal, tax, platformFee, total, subtotal);
    }

    /** The conservation invariant: what escrow holds is exactly what leaves it. */
    public boolean isConserved() {
        return totalMinor == Math.addExact(Math.addExact(payoutMinor, taxMinor), platformFeeMinor);
    }

    public static Money money(long minor) {
        return Money.newBuilder().setAmountMinor(minor).setCurrencyCode(CURRENCY).build();
    }

    public TransactionAmounts toProto() {
        return TransactionAmounts.newBuilder()
                .setSubtotal(money(subtotalMinor))
                .setTax(money(taxMinor))
                .setPlatformFee(money(platformFeeMinor))
                .setTotal(money(totalMinor))
                .setPayoutAmount(money(payoutMinor))
                .build();
    }
}
