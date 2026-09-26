package uz.agrogram.orderescrow.store.jdbc;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static uz.agrogram.orderescrow.testing.Fixtures.id;
import static uz.agrogram.orderescrow.testing.Fixtures.rid;
import static uz.agrogram.orderescrow.testing.Fixtures.tonnes;

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.Statement;
import java.time.Clock;
import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.Callable;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import org.apache.kafka.clients.producer.MockProducer;
import org.apache.kafka.clients.producer.ProducerRecord;
import org.apache.kafka.common.KafkaException;
import org.apache.kafka.common.serialization.ByteArraySerializer;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestInstance;
import org.springframework.dao.DataIntegrityViolationException;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.Mass;
import uz.agrogram.events.v1.TransactionState;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.auth.Uuids;
import uz.agrogram.orderescrow.config.DatabaseUrl;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.PaymentRecord;
import uz.agrogram.orderescrow.domain.TaxPolicy;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.events.OutboxRelay;
import uz.agrogram.orderescrow.events.OutboxStore;
import uz.agrogram.orderescrow.events.TransactionEvents;
import uz.agrogram.orderescrow.migrate.Migrator;
import uz.agrogram.orderescrow.service.EscrowService;
import uz.agrogram.orderescrow.store.ConflictException;
import uz.agrogram.orderescrow.testing.Fixtures;
import uz.agrogram.orderescrow.testing.Fixtures.FakeCatalog;
import uz.agrogram.orderescrow.v1.ConfirmHandoverRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptResponse;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsResponse;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.PayInResponse;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptRequest;
import uz.agrogram.orderescrow.v1.Transaction;

/**
 * The service and the JDBC store against a real Postgres, in a temporary schema
 * that is created, migrated with the real migrations and dropped again; nothing
 * else in the database is touched.
 *
 * <pre>
 *   ORDER_ESCROW_TEST_DATABASE_URL=postgres://order_escrow:...@127.0.0.1:15432/order_escrow?sslmode=disable \
 *     ./gradlew integrationTest        (or: make test-integration, which loads ./.env)
 * </pre>
 */
@Tag("integration")
@TestInstance(TestInstance.Lifecycle.PER_CLASS)
class JdbcEscrowStoreIT {

    private static final String ENV = "ORDER_ESCROW_TEST_DATABASE_URL";

    private DatabaseUrl base;
    private final List<String> schemas = new ArrayList<>();
    private HikariDataSource dataSource;
    private JdbcClient jdbc;
    private JdbcEscrowStore store;
    private final FakeCatalog catalog = new FakeCatalog();
    private EscrowService service;

    @BeforeAll
    void setUp() throws Exception {
        String url = System.getenv(ENV);
        Assumptions.assumeTrue(url != null && !url.isBlank(), ENV + " is not set");
        base = DatabaseUrl.parse(url);
        String schema = newSchema();
        new Migrator(Migrator.flyway(base, schema), System.out).up();

        DatabaseUrl db = base.withSchema(schema);
        HikariConfig hc = new HikariConfig();
        hc.setJdbcUrl(db.jdbcUrl());
        hc.setUsername(db.username());
        hc.setPassword(db.password());
        hc.setMaximumPoolSize(24);
        hc.setPoolName("order-escrow-it");
        dataSource = new HikariDataSource(hc);
        jdbc = JdbcClient.create(dataSource);
        TransactionTemplate tt = new TransactionTemplate(new DataSourceTransactionManager(dataSource));
        tt.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        store = new JdbcEscrowStore(jdbc, tt, Clock.systemUTC());
        service = new EscrowService(store, catalog, new TransactionEvents(Fixtures.VALIDATOR, Uuids::newV7),
                Clock.systemUTC(), TaxPolicy.NONE, Uuids::newV7, 1);
    }

    @AfterAll
    void tearDown() throws Exception {
        if (dataSource != null) {
            dataSource.close();
        }
        for (String schema : schemas) {
            exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE");
        }
    }

    private String newSchema() throws Exception {
        String schema = "order_escrow_it_" + UUID.randomUUID().toString().replace("-", "").substring(0, 12);
        exec("CREATE SCHEMA " + schema);
        schemas.add(schema);
        return schema;
    }

    private void exec(String sql) throws Exception {
        try (Connection c = DriverManager.getConnection(base.jdbcUrl(), base.username(), base.password());
                Statement s = c.createStatement()) {
            s.execute(sql);
        }
    }

    private long count(String sql, UUID id) {
        return jdbc.sql(sql).param("id", id).query(Long.class).single();
    }

    private Listing listing(UUID farmer, double tonnes) {
        Listing l = Fixtures.offer(farmer).setQuantity(Fixtures.tonnes(tonnes)).build();
        catalog.put(l);
        return l;
    }

    private Transaction create(Principal p, Listing l, Mass quantity) {
        return service.createTransaction(p, CreateTransactionRequest.newBuilder().setRequestId(rid())
                .setListingId(l.getId()).setQuantity(quantity).build()).getTransaction();
    }

    private void payAndDeliver(Principal reseller, Principal farmer, Transaction t) {
        service.payIn(reseller, PayInRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId()).build());
        service.submitWeighbridgeReceipt(reseller, SubmitWeighbridgeReceiptRequest.newBuilder().setRequestId(rid())
                .setTransactionId(t.getId()).setTicketNumber("TB-1").setPhotoMediaId(rid()).build());
        service.confirmHandover(farmer, ConfirmHandoverRequest.newBuilder().setRequestId(rid())
                .setTransactionId(t.getId()).build());
    }

    @Test
    void theWholeLifecycleIsStoredConsistently() {
        UUID farmerId = id();
        Principal farmer = Fixtures.farmer(farmerId);
        Principal reseller = Fixtures.reseller(id());
        Listing l = listing(farmerId, 5);
        Transaction t = create(reseller, l, tonnes(2));
        UUID txId = UUID.fromString(t.getId());
        payAndDeliver(reseller, farmer, t);
        Transaction done = service.confirmReceipt(reseller, ConfirmReceiptRequest.newBuilder().setRequestId(rid())
                .setTransactionId(t.getId()).build()).getTransaction();

        assertThat(done.getState()).isEqualTo(TransactionState.TRANSACTION_STATE_COMPLETED);
        assertThat(done.getVersion()).isEqualTo(7);
        EscrowTransaction stored = store.inTransaction(tx -> tx.findTransaction(txId, false)).orElseThrow();
        assertThat(stored.version()).isEqualTo(7);
        assertThat(stored.payment().orElseThrow().released()).isTrue();
        assertThat(stored.payout().orElseThrow().amountMinor()).isEqualTo(stored.amounts().payoutMinor());
        assertThat(stored.receipt().orElseThrow().ticketNumber()).isEqualTo("TB-1");
        assertThat(count("SELECT count(*) FROM payments WHERE transaction_id = :id", txId)).isEqualTo(1);
        assertThat(count("SELECT count(*) FROM payouts WHERE transaction_id = :id", txId)).isEqualTo(1);
        assertThat(count("SELECT count(*) FROM transaction_state_changes WHERE transaction_id = :id", txId))
                .isEqualTo(6);
        assertThat(count("SELECT count(*) FROM processed_requests WHERE transaction_id = :id", txId)).isEqualTo(5);
        List<String> types = jdbc.sql("SELECT event_type FROM outbox WHERE record_key = :key ORDER BY seq")
                .param("key", t.getId()).query(String.class).list();
        assertThat(types).containsExactly("agrogram.events.v1.TransactionCreated", "agrogram.events.v1.PaymentHeld",
                "agrogram.events.v1.ReceiptSubmitted", "agrogram.events.v1.ProductHandover",
                "agrogram.events.v1.ReceiptConfirmed", "agrogram.events.v1.PayoutReleased",
                "agrogram.events.v1.BonusAwarded");
        // What escrow held is exactly what left it.
        long held = count("SELECT amount_minor FROM payments WHERE transaction_id = :id", txId);
        long paidOut = count("SELECT amount_minor FROM payouts WHERE transaction_id = :id", txId);
        assertThat(held).isEqualTo(paidOut + stored.amounts().taxMinor() + stored.amounts().platformFeeMinor());
    }

    @Test
    void retriesAndConcurrentDuplicatesResolveToOneResult() throws Exception {
        UUID farmerId = id();
        Principal reseller = Fixtures.reseller(id());
        Transaction t = create(reseller, listing(farmerId, 5), tonnes(1));
        PayInRequest payIn = PayInRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId()).build();

        List<Object> results = race(12, () -> service.payIn(reseller, payIn));

        assertThat(results).allSatisfy(r -> assertThat(r).isInstanceOf(PayInResponse.class).isEqualTo(results.get(0)));
        assertThat(service.payIn(reseller, payIn)).isEqualTo(results.get(0));
        UUID txId = UUID.fromString(t.getId());
        assertThat(count("SELECT count(*) FROM payments WHERE transaction_id = :id", txId)).isEqualTo(1);
        assertThat(jdbc.sql("SELECT count(*) FROM outbox WHERE record_key = :key AND event_type LIKE '%PaymentHeld'")
                .param("key", t.getId()).query(Long.class).single()).isEqualTo(1);
    }

    @Test
    void concurrentConfirmationsPayOutExactlyOnce() throws Exception {
        UUID farmerId = id();
        Principal farmer = Fixtures.farmer(farmerId);
        Principal reseller = Fixtures.reseller(id());
        Transaction t = create(reseller, listing(farmerId, 5), tonnes(1));
        payAndDeliver(reseller, farmer, t);

        List<Object> results = race(12, () -> service.confirmReceipt(reseller, ConfirmReceiptRequest.newBuilder()
                .setRequestId(rid()).setTransactionId(t.getId()).build()));

        assertThat(results).filteredOn(r -> r instanceof ConfirmReceiptResponse).hasSize(1);
        assertThat(results).filteredOn(r -> r instanceof AppException).allSatisfy(r -> assertThat(
                ((AppException) r).reason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION));
        assertThat(count("SELECT count(*) FROM payouts WHERE transaction_id = :id", UUID.fromString(t.getId())))
                .isEqualTo(1);
    }

    @Test
    void concurrentCreationsNeverOversell() throws Exception {
        Listing l = listing(id(), 5);
        List<Object> results = race(20, () -> create(Fixtures.reseller(id()), l, tonnes(1)));
        assertThat(results).filteredOn(r -> r instanceof Transaction).hasSize(5);
        assertThat(results).filteredOn(r -> r instanceof AppException).allSatisfy(r -> assertThat(
                ((AppException) r).reason()).isEqualTo(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY));
        assertThat(jdbc.sql("SELECT sum(quantity_grams) FROM escrow_transactions WHERE listing_id = :id")
                .param("id", UUID.fromString(l.getId())).query(Long.class).single()).isEqualTo(5_000_000);
    }

    @Test
    void theCallersOpenTransactionIsUniquePerListing() throws Exception {
        Listing l = listing(id(), 5);
        Principal reseller = Fixtures.reseller(id());
        List<Object> results = race(8, () -> create(reseller, l, tonnes(1)));
        Set<String> ids = new HashSet<>();
        results.forEach(r -> ids.add(((Transaction) r).getId()));
        assertThat(ids).hasSize(1);

        EscrowTransaction existing = store.inTransaction(tx -> tx.findTransaction(UUID.fromString(ids.iterator()
                .next()), false)).orElseThrow();
        EscrowTransaction duplicate = existing.toBuilder().id(id()).build();
        assertThatThrownBy(() -> store.inTransaction(tx -> {
            tx.insertTransaction(duplicate);
            return null;
        })).isInstanceOf(ConflictException.class);
    }

    @Test
    void theSchemaRefusesUnbalancedMoneyAndStaleVersions() {
        Principal reseller = Fixtures.reseller(id());
        Transaction t = create(reseller, listing(id(), 5), tonnes(1));
        UUID txId = UUID.fromString(t.getId());
        assertThatThrownBy(() -> jdbc.sql("UPDATE escrow_transactions SET payout_minor = payout_minor + 1 "
                + "WHERE id = :id").param("id", txId).update()).isInstanceOf(DataIntegrityViolationException.class);
        assertThatThrownBy(() -> jdbc.sql("UPDATE escrow_transactions SET state = 'PAYMENT_HELD' WHERE id = :id")
                .param("id", txId).update()).isInstanceOf(DataIntegrityViolationException.class);

        // A writer that read an older version loses: the conditional update matches no row.
        EscrowTransaction stored = store.inTransaction(tx -> tx.findTransaction(txId, true)).orElseThrow();
        EscrowTransaction stale = stored.toBuilder().version(stored.version() + 2).build();
        java.time.Instant now = java.time.Instant.now().truncatedTo(java.time.temporal.ChronoUnit.MICROS);
        EscrowTransaction paid = stale.payIn(PaymentRecord.stubHeld(id(), stale.amounts().totalMinor(), now), now);
        assertThatThrownBy(() -> store.inTransaction(tx -> {
            tx.updateTransaction(stale, paid);
            return null;
        })).isInstanceOf(ConflictException.class);
        assertThat(count("SELECT count(*) FROM payments WHERE transaction_id = :id", txId)).isZero();
    }

    @Test
    void listTransactionsPagesWithKeysetCursors() {
        UUID farmerId = id();
        Principal reseller = Fixtures.reseller(id());
        List<String> created = new ArrayList<>();
        for (int i = 0; i < 5; i++) {
            created.add(0, create(reseller, listing(farmerId, 1), tonnes(1)).getId());
        }
        List<String> seen = new ArrayList<>();
        String token = "";
        do {
            ListTransactionsResponse page = service.listTransactions(reseller, ListTransactionsRequest.newBuilder()
                    .setPage(uz.agrogram.common.v1.PageRequest.newBuilder().setPageSize(2).setPageToken(token))
                    .build());
            page.getTransactionsList().forEach(x -> seen.add(x.getId()));
            token = page.getPage().getNextPageToken();
        } while (!token.isEmpty());
        assertThat(seen).containsExactlyElementsOf(created);
    }

    @Test
    void relaysPublishEveryRowOnceEvenWhenRacing() throws Exception {
        Principal reseller = Fixtures.reseller(id());
        for (int i = 0; i < 10; i++) {
            create(Fixtures.reseller(id()), listing(id(), 1), tonnes(1));
        }
        create(reseller, listing(id(), 1), tonnes(1));
        MockProducer<byte[], byte[]> a = new MockProducer<>(true, null, new ByteArraySerializer(),
                new ByteArraySerializer());
        MockProducer<byte[], byte[]> b = new MockProducer<>(true, null, new ByteArraySerializer(),
                new ByteArraySerializer());
        OutboxRelay.Config cfg = new OutboxRelay.Config(java.time.Duration.ofMillis(10), 3,
                java.time.Duration.ofMillis(50), java.time.Duration.ofSeconds(5), java.time.Duration.ZERO);
        OutboxRelay ra = new OutboxRelay(store, () -> a, cfg, Clock.systemUTC());
        OutboxRelay rb = new OutboxRelay(store, () -> b, cfg, Clock.systemUTC());
        List<OutboxRelay> relays = List.of(ra, rb);
        java.util.concurrent.atomic.AtomicInteger next = new java.util.concurrent.atomic.AtomicInteger();
        race(2, () -> {
            OutboxRelay r = relays.get(next.getAndIncrement());
            while (r.runOnce() > 0) {
                Thread.yield();
            }
            return null;
        });
        while (ra.runOnce() > 0) {
            Thread.yield(); // rows the other relay held when this one looked
        }
        List<String> published = new ArrayList<>();
        for (ProducerRecord<byte[], byte[]> r : a.history()) {
            published.add(new String(r.headers().lastHeader("event-id").value(), StandardCharsets.UTF_8));
        }
        for (ProducerRecord<byte[], byte[]> r : b.history()) {
            published.add(new String(r.headers().lastHeader("event-id").value(), StandardCharsets.UTF_8));
        }
        assertThat(published).doesNotHaveDuplicates();
        assertThat(jdbc.sql("SELECT count(*) FROM outbox WHERE published_at IS NULL").query(Long.class).single())
                .isZero();
    }

    @Test
    void aFailedPublishKeepsTheRowsAndCountsTheAttempt() {
        create(Fixtures.reseller(id()), listing(id(), 1), tonnes(1));
        MockProducer<byte[], byte[]> down = new MockProducer<>(true, null, new ByteArraySerializer(),
                new ByteArraySerializer());
        down.sendException = new KafkaException("broker not available");
        OutboxRelay relay = new OutboxRelay(store, () -> down, new OutboxRelay.Config(java.time.Duration.ofSeconds(1),
                1000, java.time.Duration.ofSeconds(1), java.time.Duration.ofSeconds(2), java.time.Duration.ZERO),
                Clock.systemUTC());
        assertThatThrownBy(relay::runOnce).isInstanceOf(OutboxStore.PublishException.class);
        assertThat(jdbc.sql("SELECT count(*) FROM outbox WHERE published_at IS NULL AND attempts > 0 "
                + "AND last_error LIKE '%broker not available%'").query(Long.class).single()).isPositive();
    }

    @Test
    void migrationsGoDownAndUpAgain() throws Exception {
        String schema = newSchema();
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        Migrator m = new Migrator(Migrator.flyway(base, schema), new PrintStream(out, true, StandardCharsets.UTF_8));
        m.up();
        m.status();
        m.down();
        m.down();
        m.up();
        String log = out.toString(StandardCharsets.UTF_8);
        assertThat(log).contains("1 migration(s) applied").contains("rolled back version 1")
                .contains("nothing to roll back");
        try (Connection c = DriverManager.getConnection(base.jdbcUrl(), base.username(), base.password());
                Statement s = c.createStatement()) {
            var rs = s.executeQuery("SELECT count(*) FROM information_schema.tables WHERE table_schema = '"
                    + schema + "' AND table_name = 'escrow_transactions'");
            rs.next();
            assertThat(rs.getInt(1)).isEqualTo(1);
        }
    }

    /** Runs {@code n} copies of {@code call} at once; returns each result or AppException. */
    private static List<Object> race(int n, Callable<Object> call) throws Exception {
        ExecutorService pool = Executors.newFixedThreadPool(n);
        try {
            CountDownLatch start = new CountDownLatch(1);
            List<Future<Object>> futures = new ArrayList<>();
            for (int i = 0; i < n; i++) {
                futures.add(pool.submit(() -> {
                    start.await();
                    try {
                        return call.call();
                    } catch (AppException e) {
                        return e;
                    }
                }));
            }
            start.countDown();
            List<Object> out = new ArrayList<>();
            for (Future<Object> f : futures) {
                out.add(f.get());
            }
            return out;
        } finally {
            pool.shutdownNow();
        }
    }
}
