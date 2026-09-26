package uz.agrogram.orderescrow.events;

import java.time.Instant;
import java.util.Collections;
import java.util.Map;
import java.util.Objects;
import java.util.TreeMap;
import java.util.UUID;

/**
 * One outbox row: a Kafka record ready to publish (table outbox, the same
 * columns as the Go services' outbox).
 *
 * @param id EventMetadata.event_id, also the row's primary key
 * @param topic Kafka topic
 * @param key record key (the aggregate id)
 * @param value the serialized envelope (Protobuf binary)
 * @param headers record headers, sorted by name
 * @param eventType EventMetadata.event_type
 * @param createdAt the commit time of the state change (EventMetadata.occurred_at)
 */
public record OutboxMessage(
        UUID id,
        String topic,
        String key,
        byte[] value,
        Map<String, String> headers,
        String eventType,
        Instant createdAt) {

    public OutboxMessage {
        Objects.requireNonNull(id, "id");
        Objects.requireNonNull(topic, "topic");
        Objects.requireNonNull(key, "key");
        Objects.requireNonNull(value, "value");
        Objects.requireNonNull(eventType, "eventType");
        Objects.requireNonNull(createdAt, "createdAt");
        value = value.clone();
        headers = Collections.unmodifiableSortedMap(new TreeMap<>(headers));
    }

    @Override
    public byte[] value() {
        return value.clone();
    }

    @Override
    public boolean equals(Object o) {
        return o instanceof OutboxMessage m && id.equals(m.id) && topic.equals(m.topic) && key.equals(m.key)
                && java.util.Arrays.equals(value, m.value) && headers.equals(m.headers)
                && eventType.equals(m.eventType) && createdAt.equals(m.createdAt);
    }

    @Override
    public int hashCode() {
        return id.hashCode();
    }

    @Override
    public String toString() {
        return "OutboxMessage{id=" + id + ", topic=" + topic + ", key=" + key + ", eventType=" + eventType + "}";
    }
}
