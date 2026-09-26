package uz.agrogram.orderescrow.config;

import java.math.BigDecimal;
import java.time.Duration;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Parses durations in Go's time.ParseDuration syntax ("1s", "500ms", "1h30m",
 * "168h", "1.5s"), the syntax of the Go services' duration variables, so the
 * same values work for every service.
 */
public final class GoDuration {

    private static final Pattern PART = Pattern.compile("(\\d+(?:\\.\\d*)?|\\.\\d+)(ns|us|µs|μs|ms|s|m|h)");

    private GoDuration() {}

    /** @throws IllegalArgumentException for anything Go would reject */
    public static Duration parse(String raw) {
        if (raw == null || raw.isEmpty()) {
            throw new IllegalArgumentException("is empty");
        }
        String s = raw;
        boolean negative = false;
        if (s.startsWith("-") || s.startsWith("+")) {
            negative = s.charAt(0) == '-';
            s = s.substring(1);
        }
        if (s.equals("0")) {
            return Duration.ZERO;
        }
        Matcher m = PART.matcher(s);
        int pos = 0;
        BigDecimal nanos = BigDecimal.ZERO;
        while (pos < s.length()) {
            if (!m.find(pos) || m.start() != pos) {
                throw new IllegalArgumentException("invalid duration \"" + raw + "\" (use e.g. 500ms, 2s, 1h30m)");
            }
            nanos = nanos.add(new BigDecimal(m.group(1)).multiply(unitNanos(m.group(2))));
            pos = m.end();
        }
        long n;
        try {
            n = nanos.toBigInteger().longValueExact();
        } catch (ArithmeticException e) {
            throw new IllegalArgumentException("duration \"" + raw + "\" is too large");
        }
        return Duration.ofNanos(negative ? -n : n);
    }

    private static BigDecimal unitNanos(String unit) {
        switch (unit) {
            case "ns":
                return BigDecimal.ONE;
            case "us":
            case "µs":
            case "μs":
                return BigDecimal.valueOf(1_000L);
            case "ms":
                return BigDecimal.valueOf(1_000_000L);
            case "s":
                return BigDecimal.valueOf(1_000_000_000L);
            case "m":
                return BigDecimal.valueOf(60_000_000_000L);
            default:
                return BigDecimal.valueOf(3_600_000_000_000L);
        }
    }
}
