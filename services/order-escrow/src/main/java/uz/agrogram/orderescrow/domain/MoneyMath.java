package uz.agrogram.orderescrow.domain;

import java.math.BigInteger;
import uz.agrogram.common.v1.MassUnit;

/**
 * Exact money arithmetic in minor units (tiyin). Never floating point
 * (proto/common/v1/money.proto): amounts are longs, products go through
 * BigInteger, and rounding is half-up at the final step only.
 */
public final class MoneyMath {

    private static final BigInteger TWO = BigInteger.valueOf(2);

    private MoneyMath() {}

    /** grams_in(unit) of money.proto: KILOGRAM = 1 000, TONNE = 1 000 000. */
    public static long gramsIn(MassUnit unit) {
        switch (unit) {
            case MASS_UNIT_KILOGRAM:
                return 1_000L;
            case MASS_UNIT_TONNE:
                return 1_000_000L;
            default:
                throw new IllegalArgumentException("a price unit must be KILOGRAM or TONNE, got " + unit);
        }
    }

    /**
     * {@code round_half_up(priceMinor * grams / grams_in(unit))}: the price of
     * {@code grams} at {@code priceMinor} per {@code unit}, rounded half-up to a
     * whole tiyin (money.proto, TransactionAmounts.subtotal).
     *
     * @throws IllegalArgumentException for a negative price or mass
     * @throws ArithmeticException when the result does not fit in a long
     */
    public static long lineTotal(long priceMinor, MassUnit unit, long grams) {
        if (priceMinor < 0 || grams < 0) {
            throw new IllegalArgumentException("price and mass must not be negative");
        }
        BigInteger numerator = BigInteger.valueOf(priceMinor).multiply(BigInteger.valueOf(grams));
        BigInteger denominator = BigInteger.valueOf(gramsIn(unit));
        BigInteger[] qr = numerator.divideAndRemainder(denominator);
        BigInteger result = qr[0];
        if (qr[1].multiply(TWO).compareTo(denominator) >= 0) {
            result = result.add(BigInteger.ONE);
        }
        return result.longValueExact();
    }
}
