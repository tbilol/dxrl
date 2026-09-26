package uz.agrogram.orderescrow.store;

import java.time.Instant;
import java.util.Objects;
import java.util.Set;
import java.util.UUID;
import uz.agrogram.orderescrow.domain.EscrowState;
import uz.agrogram.orderescrow.domain.Party;

/**
 * A page of the caller's transactions, newest first (created_at DESC, id DESC).
 *
 * @param callerId whose transactions
 * @param side FARMER (sales), RESELLER (purchases), or null for both
 * @param states states to include; empty = all
 * @param listingId only transactions on this listing, or null
 * @param after keyset cursor: rows strictly after this one in the order, or null
 * @param limit maximum rows to return
 */
public record ListQuery(UUID callerId, Party side, Set<EscrowState> states, UUID listingId, Cursor after, int limit) {

    public ListQuery {
        Objects.requireNonNull(callerId, "callerId");
        states = Set.copyOf(states);
        if (limit < 1) {
            throw new IllegalArgumentException("limit must be positive");
        }
    }

    /** The position of the last row of the previous page. */
    public record Cursor(Instant createdAt, UUID id) {
        public Cursor {
            Objects.requireNonNull(createdAt, "createdAt");
            Objects.requireNonNull(id, "id");
        }
    }
}
