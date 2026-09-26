package uz.agrogram.orderescrow.events;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.Supplier;
import org.apache.kafka.clients.producer.MockProducer;
import org.apache.kafka.clients.producer.Producer;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.common.KafkaException;
import org.apache.kafka.common.header.Header;
import org.apache.kafka.common.serialization.ByteArraySerializer;
import org.junit.jupiter.api.Test;
import uz.agrogram.common.v1.Mass;
import uz.agrogram.orderescrow.testing.Fixtures;
import uz.agrogram.orderescrow.testing.Fixtures.Harness;
import uz.agrogram.orderescrow.testing.InMemoryEscrowStore.OutboxRow;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.Transaction;

class OutboxRelayTest {

    private final Harness h = new Harness();
    private final MockProducer<byte[], byte[]> producer =
            new MockProducer<>(true, null, new ByteArraySerializer(), new ByteArraySerializer());

    private static OutboxRelay.Config config() {
        return new OutboxRelay.Config(Duration.ofMillis(20), 100, Duration.ofMillis(100), Duration.ofSeconds(2),
                Duration.ofHours(168));
    }

    /** Two events of one transaction: TransactionCreated and PaymentHeld. */
    private Transaction twoEvents() {
        var farmer = Fixtures.id();
        var listing = Fixtures.offer(farmer).build();
        h.catalog.put(listing);
        var reseller = Fixtures.reseller(Fixtures.id());
        Transaction t = h.service.createTransaction(reseller, CreateTransactionRequest.newBuilder()
                .setRequestId(Fixtures.rid()).setListingId(listing.getId())
                .setQuantity(Mass.newBuilder().setGrams(1_000_000)).build()).getTransaction();
        h.service.payIn(reseller, PayInRequest.newBuilder().setRequestId(Fixtures.rid())
                .setTransactionId(t.getId()).build());
        return t;
    }

    @Test
    void publishesInOutboxOrderKeyedByTransactionWithTheStandardHeaders() {
        Transaction t = twoEvents();
        OutboxRelay relay = new OutboxRelay(h.store, () -> producer, config(), h.clock);

        assertThat(relay.runOnce()).isEqualTo(2);

        List<ProducerRecord<byte[], byte[]>> sent = producer.history();
        assertThat(sent).hasSize(2);
        List<OutboxMessage> outbox = h.store.outbox();
        for (int i = 0; i < sent.size(); i++) {
            ProducerRecord<byte[], byte[]> r = sent.get(i);
            OutboxMessage m = outbox.get(i);
            assertThat(r.topic()).isEqualTo("agrogram.orderescrow.transaction.v1");
            assertThat(new String(r.key(), StandardCharsets.UTF_8)).isEqualTo(t.getId());
            assertThat(r.value()).isEqualTo(m.value());
            assertThat(r.headers().toArray()).extracting(Header::key)
                    .containsExactly("content-type", "event-id", "event-type", "traceparent", "tracestate",
                            "x-request-id");
            assertThat(new String(r.headers().lastHeader("event-id").value(), StandardCharsets.UTF_8))
                    .isEqualTo(m.id().toString());
            assertThat(new String(r.headers().lastHeader("content-type").value(), StandardCharsets.UTF_8))
                    .isEqualTo("application/x-protobuf");
        }
        assertThat(new String(sent.get(0).headers().lastHeader("event-type").value(), StandardCharsets.UTF_8))
                .isEqualTo("agrogram.events.v1.TransactionCreated");
        assertThat(new String(sent.get(1).headers().lastHeader("event-type").value(), StandardCharsets.UTF_8))
                .isEqualTo("agrogram.events.v1.PaymentHeld");
        assertThat(h.store.outboxRows()).allSatisfy(r -> assertThat(r.publishedAt).isNotNull());
        // Nothing left: the next run publishes nothing.
        assertThat(relay.runOnce()).isZero();
        assertThat(producer.history()).hasSize(2);
    }

    @Test
    void whenKafkaIsDownTheRowsStayUnpublishedAndCountAttempts() {
        twoEvents();
        producer.sendException = new KafkaException("broker not available");
        OutboxRelay relay = new OutboxRelay(h.store, () -> producer, config(), h.clock);

        assertThatThrownBy(relay::runOnce).isInstanceOf(OutboxStore.PublishException.class);
        assertThatThrownBy(relay::runOnce).isInstanceOf(OutboxStore.PublishException.class);

        List<OutboxRow> rows = h.store.outboxRows();
        assertThat(rows).hasSize(2).allSatisfy(r -> {
            assertThat(r.publishedAt).isNull();
            assertThat(r.attempts).isEqualTo(2);
            assertThat(r.lastError).contains("broker not available");
        });

        // Kafka is back: the same rows go out, once.
        producer.sendException = null;
        assertThat(relay.runOnce()).isEqualTo(2);
        assertThat(h.store.outboxRows()).allSatisfy(r -> assertThat(r.publishedAt).isNotNull());
    }

    @Test
    void commandsDoNotWaitForKafka() {
        producer.sendException = new KafkaException("broker not available");
        // No relay running at all: commands still succeed and only write outbox rows.
        twoEvents();
        assertThat(h.store.outbox()).hasSize(2);
        assertThat(producer.history()).isEmpty();
    }

    @Test
    void aProducerThatCannotBeCreatedIsRetriedLater() {
        twoEvents();
        AtomicInteger attempts = new AtomicInteger();
        Supplier<Producer<byte[], byte[]>> flaky = () -> {
            if (attempts.incrementAndGet() == 1) {
                throw new KafkaException("Failed to construct kafka producer");
            }
            return producer;
        };
        OutboxRelay relay = new OutboxRelay(h.store, flaky, config(), h.clock);
        assertThatThrownBy(relay::runOnce).isInstanceOf(OutboxStore.PublishException.class);
        assertThat(relay.runOnce()).isEqualTo(2);
        assertThat(producer.history()).hasSize(2);
    }

    @Test
    void theLoopPublishesInTheBackgroundAndStopsCleanly() throws Exception {
        OutboxRelay relay = new OutboxRelay(h.store, () -> producer, config(), h.clock);
        relay.start();
        try {
            assertThat(relay.isRunning()).isTrue();
            twoEvents();
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
            while (producer.history().size() < 2 && System.nanoTime() < deadline) {
                Thread.sleep(10);
            }
            assertThat(producer.history()).hasSize(2);
        } finally {
            CountDownLatch stopped = new CountDownLatch(1);
            relay.stop(stopped::countDown);
            assertThat(stopped.await(10, TimeUnit.SECONDS)).isTrue();
        }
        assertThat(relay.isRunning()).isFalse();
        assertThat(producer.closed()).isTrue();
    }

    @Test
    void withoutBrokersTheRelayIsOff() {
        OutboxRelay relay = new OutboxRelay(h.store, null, config(), h.clock);
        relay.start();
        assertThat(relay.isRunning()).isFalse();
    }

    @Test
    void publishedRowsArePurgedAfterTheRetention() {
        twoEvents();
        OutboxRelay relay = new OutboxRelay(h.store, () -> producer, config(), h.clock);
        relay.runOnce();
        h.clock.advance(Duration.ofHours(200));
        assertThat(h.store.purgePublished(h.clock.instant().minus(Duration.ofHours(168)))).isEqualTo(2);
        assertThat(h.store.outbox()).isEmpty();
    }

    @Test
    void recordsCarryTheMessageUnchanged() {
        OutboxMessage m = new OutboxMessage(Fixtures.id(), "t", "k", new byte[] {1, 2, 3},
                Map.of("b", "2", "a", "1"), "agrogram.events.v1.X", h.clock.instant());
        ProducerRecord<byte[], byte[]> r = OutboxRelay.record(m);
        assertThat(r.topic()).isEqualTo("t");
        assertThat(r.key()).isEqualTo("k".getBytes(StandardCharsets.UTF_8));
        assertThat(r.value()).containsExactly(1, 2, 3);
        assertThat(r.partition()).isNull();
        assertThat(r.headers().toArray()).extracting(Header::key).containsExactly("a", "b");
    }
}
