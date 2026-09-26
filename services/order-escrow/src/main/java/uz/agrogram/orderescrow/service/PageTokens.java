package uz.agrogram.orderescrow.service;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Instant;
import java.time.temporal.ChronoUnit;
import java.util.Base64;
import java.util.HexFormat;
import java.util.List;
import java.util.Set;
import java.util.UUID;
import uz.agrogram.orderescrow.auth.Uuids;
import uz.agrogram.orderescrow.domain.EscrowState;
import uz.agrogram.orderescrow.domain.Party;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.store.ListQuery;

/**
 * Opaque ListTransactions cursors: base64url of
 * {@code v1:<filter fingerprint>:<created_at micros>:<id>}. A token is only
 * valid for the caller and filter that produced it (pagination.proto); any
 * other use is VALIDATION_FAILED on page.page_token.
 */
final class PageTokens {

    private static final String FIELD = "page.page_token";

    private PageTokens() {}

    /** A short hash of the caller and the filter. */
    static String fingerprint(UUID caller, Party side, Set<EscrowState> states, UUID listingId) {
        List<String> sorted = states.stream().map(Enum::name).sorted().toList();
        String canonical = caller + "|" + (side == null ? "" : side.name()) + "|" + String.join(",", sorted) + "|"
                + (listingId == null ? "" : listingId);
        try {
            byte[] sha = MessageDigest.getInstance("SHA-256").digest(canonical.getBytes(StandardCharsets.UTF_8));
            return HexFormat.of().formatHex(sha, 0, 8);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is not available", e);
        }
    }

    static String encode(String fingerprint, ListQuery.Cursor cursor) {
        long micros = ChronoUnit.MICROS.between(Instant.EPOCH, cursor.createdAt());
        String raw = "v1:" + fingerprint + ":" + micros + ":" + cursor.id();
        return Base64.getUrlEncoder().withoutPadding().encodeToString(raw.getBytes(StandardCharsets.UTF_8));
    }

    static ListQuery.Cursor decode(String token, String fingerprint) {
        String raw;
        try {
            raw = new String(Base64.getUrlDecoder().decode(token), StandardCharsets.UTF_8);
        } catch (IllegalArgumentException e) {
            throw invalid();
        }
        String[] parts = raw.split(":", -1);
        if (parts.length != 4 || !parts[0].equals("v1")) {
            throw invalid();
        }
        if (!parts[1].equals(fingerprint)) {
            throw AppException.invalidField(FIELD, "page_token belongs to a different filter",
                    "page_token.filter_mismatch");
        }
        long micros;
        try {
            micros = Long.parseLong(parts[2]);
        } catch (NumberFormatException e) {
            throw invalid();
        }
        UUID id = Uuids.parseCanonical(parts[3]).orElseThrow(PageTokens::invalid);
        return new ListQuery.Cursor(Instant.EPOCH.plus(micros, ChronoUnit.MICROS), id);
    }

    private static AppException invalid() {
        return AppException.invalidField(FIELD, "page_token is malformed", "page_token.malformed");
    }
}
