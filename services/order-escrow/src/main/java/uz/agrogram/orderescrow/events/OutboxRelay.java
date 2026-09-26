package uz.agrogram.orderescrow.events;

import java.nio.charset.StandardCharsets;
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Properties;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.function.Supplier;
import org.apache.kafka.clients.producer.KafkaProducer;
import org.apache.kafka.clients.producer.Producer;
import org.apache.kafka.clients.producer.ProducerConfig;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.clients.producer.RecordMetadata;
import org.apache.kafka.common.header.internals.RecordHeader;
import org.apache.kafka.common.header.internals.RecordHeaders;
import org.apache.kafka.common.serialization.ByteArraySerializer;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.context.SmartLifecycle;

/**
 * Publishes outbox messages to Kafka: at-least-once, in outbox order per relay,
 * with an exponential backoff while Kafka or Postgres fail. The same semantics as
 * the Go services' relay (services/catalog/internal/events/relay.go).
 *
 * <p>Kafka being down never blocks a command: commands only write outbox rows,
 * and this thread retries them later. Several replicas can run relays at once;
 * row locks (SKIP LOCKED) keep them off each other's batches.
 */
public final class OutboxRelay implements SmartLifecycle {

    private static final Logger log = LoggerFactory.getLogger(OutboxRelay.class);

    /** Stops after the gRPC server (commands may still enqueue while it drains). */
    public static final int PHASE = Integer.MAX_VALUE - 200;

    /**
     * Relay tuning.
     *
     * @param interval idle poll interval
     * @param batchSize messages per batch
     * @param maxBackoff cap of the error backoff
     * @param publishTimeout per batch
     * @param retention published rows older than this are purged; zero keeps them
     */
    public record Config(Duration interval, int batchSize, Duration maxBackoff, Duration publishTimeout,
            Duration retention) {
        public Config {
            if (interval.isNegative() || interval.isZero()) {
                interval = Duration.ofSeconds(1);
            }
            if (batchSize <= 0) {
                batchSize = 100;
            }
            if (maxBackoff.compareTo(interval) < 0) {
                maxBackoff = interval;
            }
            if (publishTimeout.isNegative() || publishTimeout.isZero()) {
                publishTimeout = Duration.ofSeconds(15);
            }
        }
    }

    private final OutboxStore store;
    private final Supplier<Producer<byte[], byte[]>> producerFactory;
    private final Config cfg;
    private final Clock clock;
    private final Object wakeup = new Object();

    private volatile boolean running;
    private volatile Producer<byte[], byte[]> producer;
    private Thread thread;

    /**
     * @param producerFactory creates the Kafka producer on first use (and again
     *     after a failed creation); null disables the relay (no brokers
     *     configured): events stay in the outbox table
     */
    public OutboxRelay(OutboxStore store, Supplier<Producer<byte[], byte[]>> producerFactory, Config cfg, Clock clock) {
        this.store = Objects.requireNonNull(store);
        this.producerFactory = producerFactory;
        this.cfg = Objects.requireNonNull(cfg);
        this.clock = Objects.requireNonNull(clock);
    }

    /** A producer for the outbox: idempotent, acks=all, and bounded blocking when Kafka is down. */
    public static Supplier<Producer<byte[], byte[]>> kafkaProducerFactory(List<String> brokers, String clientId,
            Duration publishTimeout) {
        return () -> {
            long publishMs = publishTimeout.toMillis();
            long requestTimeoutMs = Math.max(1000L, Math.min(10_000L, publishMs / 2));
            long lingerMs = 5L;
            Properties props = new Properties();
            props.put(ProducerConfig.BOOTSTRAP_SERVERS_CONFIG, String.join(",", brokers));
            props.put(ProducerConfig.CLIENT_ID_CONFIG, clientId);
            props.put(ProducerConfig.ACKS_CONFIG, "all");
            props.put(ProducerConfig.ENABLE_IDEMPOTENCE_CONFIG, "true");
            props.put(ProducerConfig.MAX_IN_FLIGHT_REQUESTS_PER_CONNECTION, "5");
            props.put(ProducerConfig.LINGER_MS_CONFIG, Long.toString(lingerMs));
            // send() waits at most this long for metadata while Kafka is down.
            props.put(ProducerConfig.MAX_BLOCK_MS_CONFIG, Long.toString(Math.min(5_000L, publishMs)));
            props.put(ProducerConfig.REQUEST_TIMEOUT_MS_CONFIG, Long.toString(requestTimeoutMs));
            props.put(ProducerConfig.DELIVERY_TIMEOUT_MS_CONFIG,
                    Long.toString(Math.max(publishMs, requestTimeoutMs + lingerMs)));
            return new KafkaProducer<>(props, new ByteArraySerializer(), new ByteArraySerializer());
        };
    }

    @Override
    public void start() {
        if (producerFactory == null) {
            log.warn("ORDER_ESCROW_KAFKA_BROKERS is empty: the outbox relay is off; transaction events stay "
                    + "in the outbox table until a relay runs");
            return;
        }
        running = true;
        thread = new Thread(this::loop, "outbox-relay");
        thread.start();
        log.atInfo().addKeyValue("topic", TransactionEvents.TOPIC).addKeyValue("interval", cfg.interval())
                .log("outbox relay started");
    }

    @Override
    public void stop(Runnable callback) {
        running = false;
        synchronized (wakeup) {
            wakeup.notifyAll();
        }
        Thread t = new Thread(() -> {
            try {
                if (thread != null) {
                    // A batch in flight may finish, bounded by the publish timeout.
                    thread.join(cfg.publishTimeout().plusSeconds(5).toMillis());
                }
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            } finally {
                closeProducer();
                log.info("outbox relay stopped");
                callback.run();
            }
        }, "outbox-relay-stop");
        t.start();
    }

    @Override
    public void stop() {
        stop(() -> {});
    }

    @Override
    public boolean isRunning() {
        return running;
    }

    @Override
    public int getPhase() {
        return PHASE;
    }

    private void loop() {
        Duration backoff = Duration.ZERO;
        Instant lastPurge = Instant.EPOCH;
        while (running) {
            Duration wait = cfg.interval();
            try {
                int n = runOnce();
                backoff = Duration.ZERO;
                if (n >= cfg.batchSize()) {
                    wait = Duration.ZERO; // more is waiting
                }
            } catch (RuntimeException e) {
                backoff = min(max(backoff.multipliedBy(2), cfg.interval()), cfg.maxBackoff());
                wait = backoff;
                log.atWarn().addKeyValue("backoff", backoff).addKeyValue("error", String.valueOf(e))
                        .log("outbox relay failed; backing off");
            }
            if (!running) {
                return;
            }
            Instant now = clock.instant();
            if (!cfg.retention().isZero() && Duration.between(lastPurge, now).compareTo(Duration.ofHours(1)) > 0) {
                lastPurge = now;
                try {
                    long purged = store.purgePublished(now.minus(cfg.retention()));
                    if (purged > 0) {
                        log.atInfo().addKeyValue("rows", purged).log("outbox purged");
                    }
                } catch (RuntimeException e) {
                    log.atWarn().addKeyValue("error", String.valueOf(e)).log("outbox purge failed");
                }
            }
            if (!wait.isZero()) {
                synchronized (wakeup) {
                    if (!running) {
                        return;
                    }
                    try {
                        wakeup.wait(Math.max(1L, wait.toMillis()));
                    } catch (InterruptedException e) {
                        Thread.currentThread().interrupt();
                        return;
                    }
                }
            }
        }
    }

    /** Publishes one batch and returns its size. */
    public int runOnce() {
        return store.processOutbox(cfg.batchSize(), this::publish);
    }

    private void publish(List<OutboxMessage> batch) throws Exception {
        Producer<byte[], byte[]> p = producer();
        long deadline = System.nanoTime() + cfg.publishTimeout().toNanos();
        List<Future<RecordMetadata>> acks = new ArrayList<>(batch.size());
        for (OutboxMessage m : batch) {
            acks.add(p.send(record(m)));
        }
        for (Future<RecordMetadata> ack : acks) {
            long remaining = deadline - System.nanoTime();
            if (remaining <= 0) {
                throw new TimeoutException("publish timeout of " + cfg.publishTimeout() + " exceeded");
            }
            ack.get(remaining, TimeUnit.NANOSECONDS);
        }
        log.atDebug().addKeyValue("messages", batch.size()).log("outbox batch published");
    }

    private Producer<byte[], byte[]> producer() {
        Producer<byte[], byte[]> p = producer;
        if (p == null) {
            synchronized (this) {
                p = producer;
                if (p == null) {
                    p = producerFactory.get();
                    producer = p;
                }
            }
        }
        return p;
    }

    private void closeProducer() {
        Producer<byte[], byte[]> p = producer;
        producer = null;
        if (p != null) {
            try {
                p.close(Duration.ofSeconds(5));
            } catch (RuntimeException e) {
                log.atWarn().addKeyValue("error", String.valueOf(e)).log("closing the Kafka producer failed");
            }
        }
    }

    /** The Kafka record of an outbox message: topic, key, value, and the headers sorted by name. */
    public static ProducerRecord<byte[], byte[]> record(OutboxMessage m) {
        RecordHeaders headers = new RecordHeaders();
        for (Map.Entry<String, String> h : m.headers().entrySet()) {
            headers.add(new RecordHeader(h.getKey(), h.getValue().getBytes(StandardCharsets.UTF_8)));
        }
        return new ProducerRecord<>(m.topic(), null, null, m.key().getBytes(StandardCharsets.UTF_8), m.value(),
                headers);
    }

    private static Duration min(Duration a, Duration b) {
        return a.compareTo(b) <= 0 ? a : b;
    }

    private static Duration max(Duration a, Duration b) {
        return a.compareTo(b) >= 0 ? a : b;
    }
}
