package uz.agrogram.orderescrow.events;

import build.buf.protovalidate.Validator;
import com.google.protobuf.Message;
import java.time.Instant;
import java.util.Map;
import java.util.Objects;
import java.util.TreeMap;
import java.util.UUID;
import java.util.function.Supplier;
import uz.agrogram.common.v1.Actor;
import uz.agrogram.common.v1.ResourceType;
import uz.agrogram.events.v1.BonusAwarded;
import uz.agrogram.events.v1.EventMetadata;
import uz.agrogram.events.v1.PaymentHeld;
import uz.agrogram.events.v1.PayoutReleased;
import uz.agrogram.events.v1.ProductHandover;
import uz.agrogram.events.v1.ReceiptConfirmed;
import uz.agrogram.events.v1.ReceiptSubmitted;
import uz.agrogram.events.v1.TransactionCreated;
import uz.agrogram.events.v1.TransactionEvent;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.Protos;
import uz.agrogram.orderescrow.error.ProtoValidation;

/**
 * Builds the events order-escrow publishes on its transaction stream
 * (proto/events/v1/transaction.proto, envelope rules in metadata.proto):
 *
 * <pre>
 *   Topic   agrogram.orderescrow.transaction.v1
 *   Key     transaction id
 *   Value   TransactionEvent (Protobuf binary): metadata, the snapshot after the
 *           event, and exactly one oneof case
 *   Headers content-type, event-type, event-id, x-request-id, traceparent
 *           (and tracestate when present)
 * </pre>
 *
 * Every event is validated with protovalidate before it goes to the outbox; an
 * invalid event is a bug and fails the whole command (nothing is committed).
 * order-escrow consumes no stream; its dead letters would go to
 * agrogram.orderescrow.deadletter.v1 once it does.
 */
public final class TransactionEvents {

    public static final String TOPIC = "agrogram.orderescrow.transaction.v1";
    public static final String PRODUCER = Principal.SERVICE_NAME;
    public static final String CONTENT_TYPE = "application/x-protobuf";

    public static final String HEADER_CONTENT_TYPE = "content-type";
    public static final String HEADER_EVENT_TYPE = "event-type";
    public static final String HEADER_EVENT_ID = "event-id";
    public static final String HEADER_REQUEST_ID = "x-request-id";
    public static final String HEADER_TRACEPARENT = "traceparent";
    public static final String HEADER_TRACESTATE = "tracestate";

    private final Validator validator;
    private final Supplier<UUID> eventIds;

    /**
     * @param validator protovalidate validator for the envelopes
     * @param eventIds event id generator (UUIDv7 in production)
     */
    public TransactionEvents(Validator validator, Supplier<UUID> eventIds) {
        this.validator = Objects.requireNonNull(validator);
        this.eventIds = Objects.requireNonNull(eventIds);
    }

    /**
     * The request context every event of one command carries.
     *
     * @param correlationId the x-request-id of the command
     * @param traceparent W3C traceparent, or ""
     * @param tracestate W3C tracestate, or ""
     */
    public record Origin(String correlationId, String traceparent, String tracestate) {
        public static Origin of(Principal p) {
            return new Origin(p.requestId(), p.traceparent(), p.tracestate());
        }
    }

    /**
     * Builds and validates one TransactionEvent as an outbox message.
     *
     * @param after the transaction right after the event: the snapshot and
     *     EventMetadata.aggregate_version
     * @param event the concrete event, one of the TransactionEvent oneof cases
     * @param actor who performed it (a party, or order-escrow for system steps)
     * @param occurredAt the commit time of the state change
     * @param origin the command's correlation id and trace context
     * @param causationId event_id of the event that caused this one, or null
     *     when a command caused it
     */
    public OutboxMessage build(EscrowTransaction after, Message event, Actor actor, Instant occurredAt, Origin origin,
            UUID causationId) {
        UUID eventId = eventIds.get();
        TransactionEvent.Builder env = TransactionEvent.newBuilder().setTransaction(Protos.snapshot(after));
        setCase(env, event);
        EventMetadata.Builder md = EventMetadata.newBuilder()
                .setEventId(eventId.toString())
                .setEventType(event.getDescriptorForType().getFullName())
                .setOccurredAt(Protos.timestamp(occurredAt))
                .setProducer(PRODUCER)
                .setAggregateType(ResourceType.RESOURCE_TYPE_TRANSACTION)
                .setAggregateId(after.id().toString())
                .setAggregateVersion(after.version())
                .setCorrelationId(origin.correlationId())
                .setTraceparent(origin.traceparent())
                .setTracestate(origin.tracestate())
                .setActor(actor);
        if (causationId != null) {
            md.setCausationId(causationId.toString());
        }
        TransactionEvent envelope = env.setMetadata(md).build();
        ProtoValidation.require(validator, envelope);

        Map<String, String> headers = new TreeMap<>();
        headers.put(HEADER_CONTENT_TYPE, CONTENT_TYPE);
        headers.put(HEADER_EVENT_TYPE, md.getEventType());
        headers.put(HEADER_EVENT_ID, eventId.toString());
        headers.put(HEADER_REQUEST_ID, origin.correlationId());
        if (!origin.traceparent().isEmpty()) {
            headers.put(HEADER_TRACEPARENT, origin.traceparent());
        }
        if (!origin.tracestate().isEmpty()) {
            headers.put(HEADER_TRACESTATE, origin.tracestate());
        }
        return new OutboxMessage(eventId, TOPIC, after.id().toString(), envelope.toByteArray(), headers,
                md.getEventType(), occurredAt);
    }

    private static void setCase(TransactionEvent.Builder env, Message event) {
        if (event instanceof TransactionCreated e) {
            env.setTransactionCreated(e);
        } else if (event instanceof PaymentHeld e) {
            env.setPaymentHeld(e);
        } else if (event instanceof ReceiptSubmitted e) {
            env.setReceiptSubmitted(e);
        } else if (event instanceof ProductHandover e) {
            env.setProductHandover(e);
        } else if (event instanceof ReceiptConfirmed e) {
            env.setReceiptConfirmed(e);
        } else if (event instanceof PayoutReleased e) {
            env.setPayoutReleased(e);
        } else if (event instanceof BonusAwarded e) {
            env.setBonusAwarded(e);
        } else {
            throw new IllegalArgumentException(
                    (event == null ? "null" : event.getDescriptorForType().getFullName())
                            + " is not a TransactionEvent case");
        }
    }
}
