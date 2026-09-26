package uz.agrogram.orderescrow.app;

import build.buf.protovalidate.Validator;
import build.buf.protovalidate.ValidatorFactory;
import io.grpc.protobuf.services.HealthStatusManager;
import java.time.Clock;
import java.util.List;
import javax.sql.DataSource;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;
import uz.agrogram.orderescrow.auth.Uuids;
import uz.agrogram.orderescrow.catalog.CatalogClient;
import uz.agrogram.orderescrow.config.ServiceConfig;
import uz.agrogram.orderescrow.events.OutboxRelay;
import uz.agrogram.orderescrow.events.TransactionEvents;
import uz.agrogram.orderescrow.grpc.GrpcServer;
import uz.agrogram.orderescrow.grpc.OrderEscrowGrpcService;
import uz.agrogram.orderescrow.health.ReadinessMonitor;
import uz.agrogram.orderescrow.service.EscrowService;
import uz.agrogram.orderescrow.store.jdbc.JdbcEscrowStore;
import uz.agrogram.orderescrow.v1.OrderEscrowServiceGrpc;

/**
 * Wires the service. Spring Boot provides the DataSource (HikariCP), JdbcClient,
 * the transaction manager, the HTTP server and logging; the rest is plain Java
 * built here. {@link ServiceConfig} is registered by the main class.
 *
 * <p>Lifecycle (SmartLifecycle phases): on start the outbox relay, then the gRPC
 * server, then readiness; on shutdown (SIGTERM) readiness drops to NOT_SERVING
 * first, the gRPC server drains running calls (ORDER_ESCROW_SHUTDOWN_TIMEOUT),
 * the relay finishes its batch and closes the Kafka producer, and the pool
 * closes last.
 */
@Configuration(proxyBeanMethods = false)
public class ServeConfiguration {

    private static final Logger log = LoggerFactory.getLogger(ServeConfiguration.class);

    @Bean
    Clock clock() {
        return Clock.systemUTC();
    }

    @Bean
    Validator protoValidator() {
        return ValidatorFactory.newBuilder().build();
    }

    /** READ COMMITTED, stated explicitly: the locking design relies on it (see EscrowService). */
    @Bean
    TransactionTemplate transactionTemplate(PlatformTransactionManager transactionManager) {
        TransactionTemplate t = new TransactionTemplate(transactionManager);
        t.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        return t;
    }

    @Bean
    JdbcEscrowStore escrowStore(JdbcClient jdbc, TransactionTemplate transactionTemplate, Clock clock) {
        return new JdbcEscrowStore(jdbc, transactionTemplate, clock);
    }

    @Bean(destroyMethod = "close")
    CatalogClient catalogClient(ServiceConfig config) {
        if (config.catalogAddr().isEmpty()) {
            log.warn("ORDER_ESCROW_CATALOG_ADDR is empty: CreateTransaction answers DEPENDENCY_UNAVAILABLE");
        }
        return CatalogClient.connect(config.catalogAddr(), config.catalogTimeout());
    }

    @Bean
    TransactionEvents transactionEvents(Validator protoValidator) {
        return new TransactionEvents(protoValidator, Uuids::newV7);
    }

    @Bean
    EscrowService escrowService(JdbcEscrowStore escrowStore, CatalogClient catalogClient,
            TransactionEvents transactionEvents, Clock clock, ServiceConfig config) {
        if (config.bonusPlaceholderPoints() > 0) {
            log.atWarn().addKeyValue("points", config.bonusPlaceholderPoints())
                    .log("STUB TODO(bonus): completed transactions award placeholder bonus points");
        }
        return new EscrowService(escrowStore, catalogClient, transactionEvents, clock,
                config.bonusPlaceholderPoints());
    }

    @Bean
    HealthStatusManager healthStatusManager() {
        return new HealthStatusManager();
    }

    @Bean
    ReadinessMonitor readinessMonitor(DataSource dataSource, HealthStatusManager healthStatusManager,
            ServiceConfig config) {
        return new ReadinessMonitor(dataSource, healthStatusManager, config.healthInterval(),
                List.of(HealthStatusManager.SERVICE_NAME_ALL_SERVICES, OrderEscrowServiceGrpc.SERVICE_NAME));
    }

    @Bean
    GrpcServer grpcServer(ServiceConfig config, EscrowService escrowService, HealthStatusManager healthStatusManager,
            Validator protoValidator) {
        if (config.grpcReflection()) {
            log.info("gRPC server reflection is on (ORDER_ESCROW_GRPC_REFLECTION): local development only");
        }
        return new GrpcServer(config.grpcAddr(),
                List.of(new OrderEscrowGrpcService(escrowService), healthStatusManager.getHealthService()),
                protoValidator, config.grpcReflection(), config.shutdownTimeout());
    }

    @Bean
    OutboxRelay outboxRelay(JdbcEscrowStore escrowStore, ServiceConfig config, Clock clock) {
        OutboxRelay.Config relay = new OutboxRelay.Config(config.outboxInterval(), config.outboxBatchSize(),
                config.outboxMaxBackoff(), config.outboxPublishTimeout(), config.outboxRetention());
        return new OutboxRelay(escrowStore,
                config.kafkaBrokers().isEmpty() ? null
                        : OutboxRelay.kafkaProducerFactory(config.kafkaBrokers(), config.kafkaClientId(),
                                config.outboxPublishTimeout()),
                relay, clock);
    }
}
