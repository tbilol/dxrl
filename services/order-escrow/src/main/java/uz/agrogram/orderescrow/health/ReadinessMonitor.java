package uz.agrogram.orderescrow.health;

import io.grpc.health.v1.HealthCheckResponse.ServingStatus;
import io.grpc.protobuf.services.HealthStatusManager;
import java.sql.Connection;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import javax.sql.DataSource;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.context.SmartLifecycle;

/**
 * Readiness: Postgres reachable and the service not shutting down. Pings the
 * database every interval in the background and publishes the result to the
 * gRPC health service and to GET /readyz, so probes answer at once. Kafka and
 * catalog do not count: Kafka being down only delays the outbox relay, and
 * catalog being down only fails CreateTransaction (DEPENDENCY_UNAVAILABLE).
 * Mirrors the Go services' health.Monitor.
 */
public final class ReadinessMonitor implements SmartLifecycle {

    private static final Logger log = LoggerFactory.getLogger(ReadinessMonitor.class);

    /** Starts last and stops first: readiness drops before the servers drain. */
    public static final int PHASE = Integer.MAX_VALUE;

    private final DataSource dataSource;
    private final HealthStatusManager grpcHealth;
    private final List<String> services;
    private final Duration interval;
    private final AtomicBoolean ready = new AtomicBoolean();
    private final AtomicBoolean stopping = new AtomicBoolean();
    private final AtomicBoolean checked = new AtomicBoolean();
    private volatile ScheduledExecutorService executor;

    /**
     * @param services gRPC health service names to publish ("" is the whole server)
     */
    public ReadinessMonitor(DataSource dataSource, HealthStatusManager grpcHealth, Duration interval,
            List<String> services) {
        this.dataSource = dataSource;
        this.grpcHealth = grpcHealth;
        this.interval = interval;
        this.services = List.copyOf(services);
        publish(false);
    }

    @Override
    public void start() {
        ScheduledExecutorService ex = Executors.newSingleThreadScheduledExecutor(r -> {
            Thread t = new Thread(r, "readiness-monitor");
            t.setDaemon(true);
            return t;
        });
        executor = ex;
        ex.scheduleWithFixedDelay(this::check, 0, interval.toMillis(), TimeUnit.MILLISECONDS);
    }

    /** Pings Postgres once and publishes the result. */
    public void check() {
        boolean ok;
        String error = null;
        int timeoutSeconds = (int) Math.max(1, interval.toSeconds());
        try (Connection c = dataSource.getConnection()) {
            ok = c.isValid(timeoutSeconds);
            if (!ok) {
                error = "connection is not valid";
            }
        } catch (Exception e) {
            ok = false;
            error = e.getMessage();
        }
        ok = ok && !stopping.get();
        boolean was = ready.get();
        boolean first = checked.compareAndSet(false, true);
        if ((was != ok || first) && !stopping.get()) {
            if (ok) {
                log.info("ready: postgres reachable");
            } else {
                log.atWarn().addKeyValue("error", error).log("not ready: postgres unreachable");
            }
        }
        publish(ok);
    }

    /** The last readiness result. */
    public boolean ready() {
        return ready.get();
    }

    private void publish(boolean ok) {
        boolean value = ok && !stopping.get();
        ready.set(value);
        ServingStatus status = value ? ServingStatus.SERVING : ServingStatus.NOT_SERVING;
        for (String s : services) {
            grpcHealth.setStatus(s, status);
        }
    }

    @Override
    public void stop() {
        stopping.set(true);
        ready.set(false);
        grpcHealth.enterTerminalState();
        ScheduledExecutorService ex = executor;
        if (ex != null) {
            ex.shutdownNow();
        }
        log.info("not ready: shutting down");
    }

    @Override
    public boolean isRunning() {
        return executor != null && !stopping.get();
    }

    @Override
    public int getPhase() {
        return PHASE;
    }
}
