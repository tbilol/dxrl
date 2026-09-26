package uz.agrogram.orderescrow.config;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import java.time.Duration;
import java.util.HashMap;
import java.util.Map;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

class ServiceConfigTest {

    private static final String DB = "postgres://order_escrow:secret@127.0.0.1:15432/order_escrow?sslmode=disable";

    private static Map<String, String> env(String... kv) {
        Map<String, String> m = new HashMap<>();
        m.put("ORDER_ESCROW_DATABASE_URL", DB);
        for (int i = 0; i + 1 < kv.length; i += 2) {
            m.put("ORDER_ESCROW_" + kv[i], kv[i + 1]);
        }
        return m;
    }

    @Test
    void defaultsMatchTheGoServices() {
        ServiceConfig c = ServiceConfig.load(env());
        assertThat(c.databaseMaxConns()).isEqualTo(10);
        assertThat(c.kafkaBrokers()).isEmpty();
        assertThat(c.kafkaClientId()).isEqualTo("order-escrow");
        assertThat(c.grpcAddr()).isEqualTo(new HostPort("", 50051));
        assertThat(c.httpAddr()).isEqualTo(new HostPort("", 8081));
        assertThat(c.grpcReflection()).isFalse();
        assertThat(c.logLevel()).isEqualTo("INFO");
        assertThat(c.logFormat()).isEqualTo("json");
        assertThat(c.catalogAddr()).isEmpty();
        assertThat(c.catalogTimeout()).isEqualTo(Duration.ofSeconds(2));
        assertThat(c.outboxInterval()).isEqualTo(Duration.ofSeconds(1));
        assertThat(c.outboxBatchSize()).isEqualTo(100);
        assertThat(c.outboxMaxBackoff()).isEqualTo(Duration.ofSeconds(30));
        assertThat(c.outboxPublishTimeout()).isEqualTo(Duration.ofSeconds(15));
        assertThat(c.outboxRetention()).isEqualTo(Duration.ofHours(168));
        assertThat(c.healthInterval()).isEqualTo(Duration.ofSeconds(5));
        assertThat(c.shutdownTimeout()).isEqualTo(Duration.ofSeconds(20));
        assertThat(c.bonusPlaceholderPoints()).isEqualTo(1);
    }

    @Test
    void readsEveryVariable() {
        ServiceConfig c = ServiceConfig.load(env("KAFKA_BROKERS", "a:1, b:2 ,", "GRPC_ADDR", "127.0.0.1:15055",
                "HTTP_ADDR", "127.0.0.1:18055", "GRPC_REFLECTION", "true", "LOG_LEVEL", "debug", "LOG_FORMAT", "TEXT",
                "CATALOG_ADDR", "127.0.0.1:15054", "CATALOG_TIMEOUT", "1500ms", "OUTBOX_RETENTION", "0",
                "SHUTDOWN_TIMEOUT", "1m30s", "BONUS_PLACEHOLDER_POINTS", "0", "DATABASE_MAX_CONNS", "4"));
        assertThat(c.kafkaBrokers()).containsExactly("a:1", "b:2");
        assertThat(c.grpcAddr()).isEqualTo(new HostPort("127.0.0.1", 15055));
        assertThat(c.grpcReflection()).isTrue();
        assertThat(c.logLevel()).isEqualTo("DEBUG");
        assertThat(c.logFormat()).isEqualTo("text");
        assertThat(c.catalogTimeout()).isEqualTo(Duration.ofMillis(1500));
        assertThat(c.outboxRetention()).isZero();
        assertThat(c.shutdownTimeout()).isEqualTo(Duration.ofSeconds(90));
        assertThat(c.bonusPlaceholderPoints()).isZero();
        assertThat(c.databaseMaxConns()).isEqualTo(4);
    }

    @Test
    void everyProblemIsReportedAtOnce() {
        Map<String, String> e = env("DATABASE_URL", "", "LOG_LEVEL", "loud", "OUTBOX_INTERVAL", "0s",
                "OUTBOX_BATCH_SIZE", "0", "GRPC_REFLECTION", "yes", "HTTP_ADDR", ":50051");
        assertThatThrownBy(() -> ServiceConfig.load(e))
                .isInstanceOf(ServiceConfig.ConfigException.class)
                .hasMessageContaining("ORDER_ESCROW_DATABASE_URL is required")
                .hasMessageContaining("ORDER_ESCROW_LOG_LEVEL")
                .hasMessageContaining("ORDER_ESCROW_OUTBOX_INTERVAL must be positive")
                .hasMessageContaining("ORDER_ESCROW_OUTBOX_BATCH_SIZE must be at least 1")
                .hasMessageContaining("ORDER_ESCROW_GRPC_REFLECTION must be a boolean")
                .hasMessageContaining("must differ");
    }

    @Test
    void theDatabaseUrlBecomesAJdbcUrlWithoutTheCredentials() {
        DatabaseUrl db = DatabaseUrl.parse("postgres://order%5Fescrow:p%40ss%3Aw%2Fo+rd@db.local:5432/order_escrow"
                + "?sslmode=disable&application_name=oe&connect_timeout=5");
        assertThat(db.jdbcUrl()).isEqualTo("jdbc:postgresql://db.local:5432/order_escrow?sslmode=disable"
                + "&ApplicationName=oe&connectTimeout=5");
        assertThat(db.username()).isEqualTo("order_escrow");
        assertThat(db.password()).isEqualTo("p@ss:w/o+rd");
        assertThat(db.toString()).doesNotContain("p@ss");
        assertThat(db.withSchema("it_1").jdbcUrl()).endsWith("&currentSchema=it_1");
        assertThat(DatabaseUrl.parse("postgresql://u@h/d").jdbcUrl()).isEqualTo("jdbc:postgresql://h/d");
    }

    @ParameterizedTest
    @CsvSource({"mysql://u:p@h/d", "postgres://u:p@h", "postgres://:p@h/d", "postgres:///d", "''"})
    void badDatabaseUrlsAreRejectedWithoutEchoingThePassword(String url) {
        assertThatThrownBy(() -> DatabaseUrl.parse(url)).isInstanceOf(IllegalArgumentException.class)
                .satisfies(e -> assertThat(e.getMessage()).doesNotContain(":p@"));
    }

    @Test
    void springPropertiesCarryTheDerivedSettings() {
        Map<String, Object> p = ServiceConfig.load(env("HTTP_ADDR", "127.0.0.1:18055", "LOG_LEVEL", "debug",
                "LOG_FORMAT", "json", "SHUTDOWN_TIMEOUT", "25s")).springProperties();
        assertThat(p).containsEntry("spring.datasource.url",
                "jdbc:postgresql://127.0.0.1:15432/order_escrow?sslmode=disable")
                .containsEntry("spring.datasource.username", "order_escrow")
                .containsEntry("server.address", "127.0.0.1")
                .containsEntry("server.port", 18055)
                .containsEntry("logging.level.uz.agrogram", "DEBUG")
                .containsEntry("logging.level.root", "INFO")
                .containsEntry("logging.structured.format.console", "ecs")
                .containsEntry("spring.lifecycle.timeout-per-shutdown-phase", "25000ms");
        assertThat(ServiceConfig.load(env("LOG_FORMAT", "text")).springProperties())
                .doesNotContainKey("logging.structured.format.console").doesNotContainKey("server.address");
    }

    @ParameterizedTest
    @CsvSource({"1s, 1000", "500ms, 500", "1h30m, 5400000", "1.5s, 1500", "168h, 604800000", "0, 0",
        "2m3s, 123000", "100us, 0"})
    void durationsUseGoSyntax(String text, long millis) {
        assertThat(GoDuration.parse(text).toMillis()).isEqualTo(millis);
    }

    @ParameterizedTest
    @CsvSource({"1", "s", "1x", "1 s", "1s2", "''"})
    void badDurationsAreRejected(String text) {
        assertThatThrownBy(() -> GoDuration.parse(text)).isInstanceOf(IllegalArgumentException.class);
    }

    @Test
    void listenAddressesUseGoSyntax() {
        assertThat(HostPort.parse(":8081")).isEqualTo(new HostPort("", 8081));
        assertThat(HostPort.parse("[::1]:15055")).isEqualTo(new HostPort("::1", 15055));
        assertThat(HostPort.parse(":8081").probeHost()).isEqualTo("127.0.0.1");
        assertThat(HostPort.parse("0.0.0.0:8081").allInterfaces()).isTrue();
        assertThatThrownBy(() -> HostPort.parse("8081")).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> HostPort.parse("::1:80")).isInstanceOf(IllegalArgumentException.class);
        assertThatThrownBy(() -> HostPort.parse("h:70000")).isInstanceOf(IllegalArgumentException.class);
    }
}
