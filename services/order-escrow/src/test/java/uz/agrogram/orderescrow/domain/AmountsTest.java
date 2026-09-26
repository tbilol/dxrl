package uz.agrogram.orderescrow.domain;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.math.BigDecimal;
import java.math.RoundingMode;
import java.util.SplittableRandom;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.events.v1.TransactionAmounts;

class AmountsTest {

    @ParameterizedTest(name = "{0} tiyin per {1} x {2} g = {3}")
    @CsvSource({
        // 6 000 so'm/kg x 5 t = 30 000 000 so'm
        "600000, MASS_UNIT_KILOGRAM, 5000000, 3000000000",
        // 1 500 000 so'm/t x 2.5 t = 3 750 000 so'm
        "150000000, MASS_UNIT_TONNE, 2500000, 375000000",
        // half-up, never banker's rounding
        "1, MASS_UNIT_KILOGRAM, 500, 1",
        "1, MASS_UNIT_KILOGRAM, 499, 0",
        "1, MASS_UNIT_KILOGRAM, 1500, 2",
        "1, MASS_UNIT_KILOGRAM, 2500, 3",
        "3, MASS_UNIT_TONNE, 500000, 2",
        "3, MASS_UNIT_TONNE, 499999, 1",
        "7, MASS_UNIT_KILOGRAM, 1, 0",
        "0, MASS_UNIT_KILOGRAM, 1000, 0",
        "123456, MASS_UNIT_KILOGRAM, 0, 0",
    })
    void lineTotalRoundsHalfUpToWholeTiyin(long price, MassUnit unit, long grams, long expected) {
        assertThat(MoneyMath.lineTotal(price, unit, grams)).isEqualTo(expected);
    }

    @Test
    void lineTotalIsExactWhereLongMultiplicationWouldOverflow() {
        // 10^12 tiyin/kg x 10^9 g: the product 10^21 overflows a long, the result 10^18 does not.
        assertThat(MoneyMath.lineTotal(1_000_000_000_000L, MassUnit.MASS_UNIT_KILOGRAM, 1_000_000_000L))
                .isEqualTo(1_000_000_000_000_000_000L);
    }

    @Test
    void lineTotalRejectsResultsBeyondLong() {
        assertThatThrownBy(() -> MoneyMath.lineTotal(Long.MAX_VALUE, MassUnit.MASS_UNIT_KILOGRAM, Long.MAX_VALUE))
                .isInstanceOf(ArithmeticException.class);
    }

    @Test
    void lineTotalRejectsNegativesAndUnknownUnits() {
        assertThatThrownBy(() -> MoneyMath.lineTotal(-1, MassUnit.MASS_UNIT_KILOGRAM, 1))
                .isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> MoneyMath.lineTotal(1, MassUnit.MASS_UNIT_KILOGRAM, -1))
                .isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> MoneyMath.lineTotal(1, MassUnit.MASS_UNIT_UNSPECIFIED, 1))
                .isInstanceOf(IllegalArgumentException.class);
    }

    @Test
    void lineTotalMatchesDecimalHalfUpForRandomInputs() {
        SplittableRandom random = new SplittableRandom(42);
        for (int i = 0; i < 20_000; i++) {
            long price = random.nextLong(1, 10_000_000_000L);
            long grams = random.nextLong(1, 100_000_000_000L);
            MassUnit unit = random.nextBoolean() ? MassUnit.MASS_UNIT_KILOGRAM : MassUnit.MASS_UNIT_TONNE;
            BigDecimal exact = BigDecimal.valueOf(price).multiply(BigDecimal.valueOf(grams))
                    .divide(BigDecimal.valueOf(MoneyMath.gramsIn(unit)), 0, RoundingMode.HALF_UP);
            assertThat(MoneyMath.lineTotal(price, unit, grams)).isEqualTo(exact.longValueExact());
        }
    }

    @Test
    void computeIsCommissionFreeTaxFreeAndConserved() {
        Amounts a = Amounts.compute(600_000, MassUnit.MASS_UNIT_KILOGRAM, 2_500_000, TaxPolicy.NONE);
        assertThat(a.subtotalMinor()).isEqualTo(1_500_000_000L);
        assertThat(a.taxMinor()).isZero();
        assertThat(a.platformFeeMinor()).isZero();
        assertThat(a.totalMinor()).isEqualTo(a.subtotalMinor());
        assertThat(a.payoutMinor()).isEqualTo(a.subtotalMinor());
        assertThat(a.isConserved()).isTrue();
    }

    @Test
    void aTaxHookIsAddedOnTopAndMoneyStaysConserved() {
        // TODO(tax): no regime yet; this only proves the hook keeps the books balanced.
        Amounts a = Amounts.compute(1_000, MassUnit.MASS_UNIT_KILOGRAM, 10_000, subtotal -> subtotal / 10);
        assertThat(a.subtotalMinor()).isEqualTo(10_000L);
        assertThat(a.taxMinor()).isEqualTo(1_000L);
        assertThat(a.totalMinor()).isEqualTo(11_000L);
        assertThat(a.payoutMinor()).isEqualTo(10_000L);
        assertThat(a.totalMinor()).isEqualTo(a.payoutMinor() + a.taxMinor() + a.platformFeeMinor());
    }

    @Test
    void inconsistentAmountsCannotExist() {
        assertThatThrownBy(() -> new Amounts(100, 0, 0, 101, 100)).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> new Amounts(100, 0, 0, 100, 99)).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> new Amounts(100, -1, 0, 99, 100)).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> new Amounts(0, 0, 0, 0, 0)).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> new Amounts(Long.MAX_VALUE, 1, 0, Long.MIN_VALUE, Long.MAX_VALUE))
                .isInstanceOf(ArithmeticException.class);
    }

    @Test
    void protoCarriesEveryAmountInUzs() {
        TransactionAmounts p = new Amounts(1_000, 20, 5, 1_025, 1_000).toProto();
        assertThat(p.getSubtotal().getAmountMinor()).isEqualTo(1_000);
        assertThat(p.getTax().getAmountMinor()).isEqualTo(20);
        assertThat(p.getPlatformFee().getAmountMinor()).isEqualTo(5);
        assertThat(p.getTotal().getAmountMinor()).isEqualTo(1_025);
        assertThat(p.getPayoutAmount().getAmountMinor()).isEqualTo(1_000);
        assertThat(p.getSubtotal().getCurrencyCode()).isEqualTo("UZS");
        assertThat(p.getTax().getCurrencyCode()).isEqualTo("UZS");
        assertThat(p.getPlatformFee().getCurrencyCode()).isEqualTo("UZS");
        assertThat(p.getTotal().getCurrencyCode()).isEqualTo("UZS");
        assertThat(p.getPayoutAmount().getCurrencyCode()).isEqualTo("UZS");
    }
}
