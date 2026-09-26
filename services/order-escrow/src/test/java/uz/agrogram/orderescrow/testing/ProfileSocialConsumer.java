package uz.agrogram.orderescrow.testing;

import build.buf.protovalidate.ValidationResult;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.protobuf.Timestamp;
import java.time.Instant;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;
import uz.agrogram.events.v1.TransactionEvent;
import uz.agrogram.events.v1.TransactionSnapshot;
import uz.agrogram.events.v1.TransactionState;

/**
 * The consumer checks of profile-social's DecodeTransactionRecord
 * (services/profile-social/internal/events/transactions.go), in Java: the
 * record must have content-type application/x-protobuf (or none), decode as a
 * TransactionEvent and pass protovalidate; a COMPLETED snapshot must have
 * parseable ids and a completion time (completed_at, else occurred_at).
 * Anything that would be "poison" (Permanent) there throws here.
 */
public final class ProfileSocialConsumer {

    /** domain.CompletedTransaction. */
    public record Completed(UUID id, UUID farmerId, UUID resellerId, Instant completedAt) {}

    private ProfileSocialConsumer() {}

    public static Optional<Completed> decode(Map<String, String> headers, byte[] value) {
        String ct = headers.getOrDefault("content-type", "");
        if (!ct.isEmpty() && !ct.equals("application/x-protobuf")) {
            throw new AssertionError("unsupported content-type " + ct);
        }
        TransactionEvent ev;
        try {
            ev = TransactionEvent.parseFrom(value);
        } catch (InvalidProtocolBufferException e) {
            throw new AssertionError("decode TransactionEvent", e);
        }
        ValidationResult result;
        try {
            result = Fixtures.VALIDATOR.validate(ev);
        } catch (Exception e) {
            throw new AssertionError("validate", e);
        }
        if (!result.isSuccess()) {
            throw new AssertionError("invalid TransactionEvent: " + result);
        }
        TransactionSnapshot snap = ev.getTransaction();
        if (snap.getState() != TransactionState.TRANSACTION_STATE_COMPLETED) {
            return Optional.empty();
        }
        UUID id = UUID.fromString(snap.getId());
        UUID farmer = UUID.fromString(snap.getFarmerId());
        UUID reseller = UUID.fromString(snap.getResellerId());
        Timestamp at = snap.hasCompletedAt() ? snap.getCompletedAt()
                : ev.getMetadata().hasOccurredAt() ? ev.getMetadata().getOccurredAt() : null;
        if (at == null) {
            throw new AssertionError("completed transaction without a completion time");
        }
        return Optional.of(new Completed(id, farmer, reseller, Instant.ofEpochSecond(at.getSeconds(), at.getNanos())));
    }
}
