package uz.agrogram.orderescrow.auth;

import java.security.SecureRandom;
import java.util.Locale;
import java.util.Optional;
import java.util.UUID;

/** UUID helpers: UUIDv7 generation (RFC 9562) and strict parsing. */
public final class Uuids {

    private static final SecureRandom RANDOM = new SecureRandom();

    private Uuids() {}

    /** A new UUIDv7: 48-bit Unix milliseconds, version 7, variant 10, 74 random bits. */
    public static UUID newV7() {
        return newV7(System.currentTimeMillis());
    }

    static UUID newV7(long unixMillis) {
        long randA = RANDOM.nextInt() & 0xFFFL;
        long msb = ((unixMillis & 0xFFFF_FFFF_FFFFL) << 16) | 0x7000L | randA;
        long lsb = (RANDOM.nextLong() & 0x3FFF_FFFF_FFFF_FFFFL) | 0x8000_0000_0000_0000L;
        return new UUID(msb, lsb);
    }

    /**
     * Parses the forms Go's uuid.Parse accepts (the Go services use it on the
     * same headers): canonical 8-4-4-4-12 hex in either case, the same in
     * braces, with a {@code urn:uuid:} prefix, or 32 hex digits without dashes.
     * Unlike {@link UUID#fromString} it rejects everything else.
     */
    public static Optional<UUID> parse(String s) {
        if (s == null) {
            return Optional.empty();
        }
        String hex;
        switch (s.length()) {
            case 36:
                hex = dashed(s);
                break;
            case 38:
                hex = s.charAt(0) == '{' && s.charAt(37) == '}' ? dashed(s.substring(1, 37)) : null;
                break;
            case 45:
                hex = s.substring(0, 9).toLowerCase(Locale.ROOT).equals("urn:uuid:") ? dashed(s.substring(9)) : null;
                break;
            case 32:
                hex = s;
                break;
            default:
                hex = null;
        }
        if (hex == null || hex.length() != 32) {
            return Optional.empty();
        }
        long msb = 0;
        long lsb = 0;
        for (int i = 0; i < 32; i++) {
            int d = Character.digit(hex.charAt(i), 16);
            if (d < 0) {
                return Optional.empty();
            }
            if (i < 16) {
                msb = (msb << 4) | d;
            } else {
                lsb = (lsb << 4) | d;
            }
        }
        return Optional.of(new UUID(msb, lsb));
    }

    /** Parses the canonical 8-4-4-4-12 form only (what protovalidate's string.uuid accepts). */
    public static Optional<UUID> parseCanonical(String s) {
        return s != null && s.length() == 36 ? parse(s) : Optional.empty();
    }

    private static String dashed(String s) {
        if (s.length() != 36 || s.charAt(8) != '-' || s.charAt(13) != '-' || s.charAt(18) != '-'
                || s.charAt(23) != '-') {
            return null;
        }
        return s.substring(0, 8) + s.substring(9, 13) + s.substring(14, 18) + s.substring(19, 23) + s.substring(24);
    }
}
