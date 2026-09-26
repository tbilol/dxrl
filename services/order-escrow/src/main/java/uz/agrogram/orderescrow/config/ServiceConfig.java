package uz.agrogram.orderescrow.config;

import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.function.Function;

/**
 * order-escrow's configuration, read from ORDER_ESCROW_* environment variables
 * with the same names, defaults and syntax as the Go services (durations like
 * "2s", booleans like "true", listen addresses like ":50051"). README.md and
 * .env.example document every variable.
 *
 * <p>{@link #load} checks everything and reports every problem at once, like
 * the Go services' config.Load. The Spring settings derived from it (data
 * source, HTTP address, logging, shutdown) come from {@link #springProperties}.
 */
public record ServiceConfig(
        DatabaseUrl database,
        int databaseMaxConns,
        List<String> kafkaBrokers,
        String kafkaClientId,
        HostPort grpcAddr,
        HostPort httpAddr,
        boolean grpcReflection,
        String logLevel,
        String logFormat,
        String catalogAddr,
        Duration catalogTimeout,
        Duration outboxInterval,
        int outboxBatchSize,
        Duration outboxMaxBackoff,
        Duration outboxPublishTimeout,
        Duration outboxRetention,
        Duration healthInterval,
        Duration shutdownTimeout,
        long bonusPlaceholderPoints) {

    public static final String PREFIX = "ORDER_ESCROW_";

    public ServiceConfig {
        kafkaBrokers = List.copyOf(kafkaBrokers);
    }

    /** Invalid configuration; the message lists every problem. */
    public static final class ConfigException extends RuntimeException {
        private static final long serialVersionUID = 1L;

        public ConfigException(List<String> problems) {
            super("config: " + String.join("; ", problems));
        }
    }

    /** Reads and checks the whole configuration. */
    public static ServiceConfig load(Map<String, String> env) {
        Reader r = new Reader(env);
        DatabaseUrl db = r.required("DATABASE_URL", DatabaseUrl::parse);
        int maxConns = r.integer("DATABASE_MAX_CONNS", 10, 1);
        List<String> brokers = r.list("KAFKA_BROKERS");
        String clientId = r.string("KAFKA_CLIENT_ID", "order-escrow");
        HostPort grpc = r.value("GRPC_ADDR", ":50051", HostPort::parse);
        HostPort http = r.value("HTTP_ADDR", ":8081", HostPort::parse);
        boolean reflection = r.bool("GRPC_REFLECTION", false);
        String level = r.value("LOG_LEVEL", "info", ServiceConfig::logLevel);
        String format = r.value("LOG_FORMAT", "json", ServiceConfig::logFormat);
        String catalog = r.string("CATALOG_ADDR", "");
        Duration catalogTimeout = r.positive("CATALOG_TIMEOUT", "2s");
        Duration outboxInterval = r.positive("OUTBOX_INTERVAL", "1s");
        int batch = r.integer("OUTBOX_BATCH_SIZE", 100, 1);
        Duration maxBackoff = r.positive("OUTBOX_MAX_BACKOFF", "30s");
        Duration publishTimeout = r.positive("OUTBOX_PUBLISH_TIMEOUT", "15s");
        Duration retention = r.nonNegative("OUTBOX_RETENTION", "168h");
        Duration health = r.positive("HEALTH_INTERVAL", "5s");
        Duration shutdown = r.positive("SHUTDOWN_TIMEOUT", "20s");
        long bonus = r.integer("BONUS_PLACEHOLDER_POINTS", 1, 0);
        if (grpc != null && http != null && grpc.port() != 0 && grpc.port() == http.port()
                && grpc.host().equals(http.host())) {
            r.problems.add(PREFIX + "GRPC_ADDR and " + PREFIX + "HTTP_ADDR must differ");
        }
        r.throwIfInvalid();
        return new ServiceConfig(db, maxConns, brokers, clientId, grpc, http, reflection, level, format, catalog,
                catalogTimeout, outboxInterval, batch, maxBackoff, publishTimeout, retention, health, shutdown, bonus);
    }

    /** Only ORDER_ESCROW_DATABASE_URL (the migrate command needs nothing else). */
    public static DatabaseUrl loadDatabase(Map<String, String> env, String variable) {
        Reader r = new Reader(env);
        DatabaseUrl db = r.required(variable, DatabaseUrl::parse);
        r.throwIfInvalid();
        return db;
    }

    /** Only ORDER_ESCROW_HTTP_ADDR (the healthcheck command must not need the rest). */
    public static HostPort loadHttpAddr(Map<String, String> env) {
        Reader r = new Reader(env);
        HostPort http = r.value("HTTP_ADDR", ":8081", HostPort::parse);
        r.throwIfInvalid();
        return http;
    }

    /** Spring Boot properties derived from the configuration (the lowest-precedence defaults). */
    public Map<String, Object> springProperties() {
        Map<String, Object> p = new LinkedHashMap<>();
        p.put("spring.datasource.url", database.jdbcUrl());
        p.put("spring.datasource.username", database.username());
        p.put("spring.datasource.password", database.password());
        p.put("spring.datasource.hikari.maximum-pool-size", databaseMaxConns);
        if (!httpAddr.host().isEmpty()) {
            p.put("server.address", httpAddr.host());
        }
        p.put("server.port", httpAddr.port());
        p.put("spring.lifecycle.timeout-per-shutdown-phase", shutdownTimeout.toMillis() + "ms");
        // The service's own loggers follow LOG_LEVEL; libraries stay at INFO or above.
        p.put("logging.level.uz.agrogram", logLevel);
        p.put("logging.level.root", logLevel.equals("DEBUG") ? "INFO" : logLevel);
        if (logFormat.equals("json")) {
            p.put("logging.structured.format.console", "ecs");
        }
        return p;
    }

    private static String logLevel(String s) {
        switch (s.toLowerCase(Locale.ROOT)) {
            case "debug":
                return "DEBUG";
            case "info":
                return "INFO";
            case "warn":
            case "warning":
                return "WARN";
            case "error":
                return "ERROR";
            default:
                throw new IllegalArgumentException("must be debug, info, warn or error, got \"" + s + "\"");
        }
    }

    private static String logFormat(String s) {
        String f = s.toLowerCase(Locale.ROOT);
        if (!f.equals("json") && !f.equals("text")) {
            throw new IllegalArgumentException("must be json or text, got \"" + s + "\"");
        }
        return f;
    }

    /** Reads variables and collects problems instead of failing on the first. */
    private static final class Reader {
        private final Map<String, String> env;
        private final List<String> problems = new ArrayList<>();

        Reader(Map<String, String> env) {
            this.env = env;
        }

        private String raw(String name) {
            String v = env.get(PREFIX + name);
            return v == null ? null : v.strip();
        }

        <T> T required(String name, Function<String, T> parse) {
            String v = raw(name);
            if (v == null || v.isEmpty()) {
                problems.add(PREFIX + name + " is required");
                return null;
            }
            return parse(name, v, parse);
        }

        <T> T value(String name, String def, Function<String, T> parse) {
            String v = raw(name);
            return parse(name, v == null || v.isEmpty() ? def : v, parse);
        }

        private <T> T parse(String name, String v, Function<String, T> parse) {
            try {
                return parse.apply(v);
            } catch (IllegalArgumentException e) {
                problems.add(PREFIX + name + " " + e.getMessage());
                return null;
            }
        }

        String string(String name, String def) {
            String v = raw(name);
            return v == null || v.isEmpty() ? def : v;
        }

        List<String> list(String name) {
            String v = raw(name);
            List<String> out = new ArrayList<>();
            if (v != null) {
                for (String part : v.split(",")) {
                    if (!part.isBlank()) {
                        out.add(part.strip());
                    }
                }
            }
            return out;
        }

        boolean bool(String name, boolean def) {
            String v = raw(name);
            if (v == null || v.isEmpty()) {
                return def;
            }
            // strconv.ParseBool
            switch (v) {
                case "1": case "t": case "T": case "TRUE": case "true": case "True":
                    return true;
                case "0": case "f": case "F": case "FALSE": case "false": case "False":
                    return false;
                default:
                    problems.add(PREFIX + name + " must be a boolean, got \"" + v + "\"");
                    return def;
            }
        }

        int integer(String name, int def, int min) {
            String v = raw(name);
            if (v == null || v.isEmpty()) {
                return def;
            }
            try {
                int n = Integer.parseInt(v);
                if (n < min) {
                    problems.add(PREFIX + name + " must be at least " + min + ", got " + n);
                }
                return n;
            } catch (NumberFormatException e) {
                problems.add(PREFIX + name + " must be an integer, got \"" + v + "\"");
                return def;
            }
        }

        Duration positive(String name, String def) {
            Duration d = value(name, def, GoDuration::parse);
            if (d != null && (d.isZero() || d.isNegative())) {
                problems.add(PREFIX + name + " must be positive, got " + raw(name));
            }
            return d;
        }

        Duration nonNegative(String name, String def) {
            Duration d = value(name, def, GoDuration::parse);
            if (d != null && d.isNegative()) {
                problems.add(PREFIX + name + " must not be negative (0 turns it off)");
            }
            return d;
        }

        void throwIfInvalid() {
            if (!problems.isEmpty()) {
                throw new ConfigException(problems);
            }
        }
    }
}
