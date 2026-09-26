package uz.agrogram.orderescrow.store.jdbc;

import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Types;
import java.time.Clock;
import java.time.Instant;
import java.time.OffsetDateTime;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.Collection;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Optional;
import java.util.UUID;
import java.util.function.Function;
import org.springframework.dao.ConcurrencyFailureException;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.jdbc.core.simple.JdbcClient;
import org.springframework.transaction.support.TransactionTemplate;
import tools.jackson.core.type.TypeReference;
import tools.jackson.databind.json.JsonMapper;
import uz.agrogram.common.v1.Actor;
import uz.agrogram.common.v1.Address;
import uz.agrogram.common.v1.GeoPoint;
import uz.agrogram.common.v1.ProductType;
import uz.agrogram.orderescrow.domain.Amounts;
import uz.agrogram.orderescrow.domain.EscrowState;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.PaymentRecord;
import uz.agrogram.orderescrow.domain.PayoutRecord;
import uz.agrogram.orderescrow.domain.ReceiptRecord;
import uz.agrogram.orderescrow.domain.StateChange;
import uz.agrogram.orderescrow.events.OutboxMessage;
import uz.agrogram.orderescrow.events.OutboxStore;
import uz.agrogram.orderescrow.store.ConflictException;
import uz.agrogram.orderescrow.store.EscrowStore;
import uz.agrogram.orderescrow.store.EscrowTx;
import uz.agrogram.orderescrow.store.ListQuery;
import uz.agrogram.orderescrow.store.ProcessedRequest;

/**
 * Postgres implementation of {@link EscrowStore} and {@link OutboxStore} with
 * Spring's JdbcClient: explicit SQL, conditional updates on the version, row
 * locks and SKIP LOCKED for the relay. Every unit of work is one READ COMMITTED
 * transaction ({@link TransactionTemplate}); a lost race surfaces as
 * {@link ConflictException}.
 */
public final class JdbcEscrowStore implements EscrowStore, OutboxStore {

    /** Namespace mixed into the advisory-lock key of a listing. */
    private static final long LISTING_LOCK_SALT = 0x6f72646572657363L; // "orderesc"

    private static final String TRANSACTION_COLUMNS = """
            t.id, t.listing_id, t.listing_kind, t.listing_version, t.farmer_id, t.reseller_id, t.initiator_id,
            t.product, t.quantity_grams, t.quantity_unit, t.unit_price_minor, t.unit_price_unit,
            t.subtotal_minor, t.tax_minor, t.platform_fee_minor, t.total_minor, t.payout_minor,
            t.address_region, t.address_district, t.address_line, t.address_latitude, t.address_longitude,
            t.state, t.bonus_points, t.version, t.created_at, t.updated_at, t.payment_held_at,
            t.receipt_submitted_at, t.handed_over_at, t.received_at, t.completed_at,
            p.id AS p_id, p.provider AS p_provider, p.provider_payment_id AS p_provider_payment_id,
            p.amount_minor AS p_amount_minor, p.held_at AS p_held_at, p.released_at AS p_released_at,
            r.ticket_number AS r_ticket_number, r.photo_media_id AS r_photo_media_id,
            r.net_mass_grams AS r_net_mass_grams, r.net_mass_unit AS r_net_mass_unit,
            r.verification_status AS r_verification_status, r.submitted_by AS r_submitted_by,
            r.submitted_at AS r_submitted_at, r.verified_at AS r_verified_at,
            o.id AS o_id, o.payment_id AS o_payment_id, o.recipient_id AS o_recipient_id,
            o.provider AS o_provider, o.provider_payout_id AS o_provider_payout_id,
            o.amount_minor AS o_amount_minor, o.released_at AS o_released_at
            FROM escrow_transactions t
            LEFT JOIN payments p ON p.transaction_id = t.id
            LEFT JOIN weighbridge_receipts r ON r.transaction_id = t.id
            LEFT JOIN payouts o ON o.transaction_id = t.id
            """;

    private final JdbcClient jdbc;
    private final TransactionTemplate transactions;
    private final Clock clock;
    private final JsonMapper json = JsonMapper.builder().build();

    /**
     * @param transactions a template on the service's DataSource; its isolation
     *     should be READ COMMITTED (the Postgres default)
     */
    public JdbcEscrowStore(JdbcClient jdbc, TransactionTemplate transactions, Clock clock) {
        this.jdbc = Objects.requireNonNull(jdbc);
        this.transactions = Objects.requireNonNull(transactions);
        this.clock = Objects.requireNonNull(clock);
    }

    @Override
    public <T> T inTransaction(Function<EscrowTx, T> work) {
        try {
            return transactions.execute(status -> work.apply(new Tx()));
        } catch (DuplicateKeyException e) {
            throw new ConflictException("unique key taken by a concurrent transaction", e);
        } catch (ConcurrencyFailureException e) {
            throw new ConflictException("concurrent transaction: " + e.getMostSpecificCause().getMessage(), e);
        }
    }

    // =====================================================================================
    // EscrowTx
    // =====================================================================================

    private final class Tx implements EscrowTx {

        @Override
        public Optional<ProcessedRequest> findProcessedRequest(UUID callerId, UUID requestId) {
            return jdbc.sql("""
                    SELECT caller_id, request_id, operation, transaction_id, request_hash, response, created_at
                    FROM processed_requests WHERE caller_id = :caller_id AND request_id = :request_id
                    """)
                    .param("caller_id", callerId)
                    .param("request_id", requestId)
                    .query((rs, i) -> new ProcessedRequest(
                            uuid(rs, "caller_id"),
                            uuid(rs, "request_id"),
                            rs.getString("operation"),
                            uuid(rs, "transaction_id"),
                            rs.getBytes("request_hash"),
                            rs.getBytes("response"),
                            instant(rs, "created_at")))
                    .optional();
        }

        @Override
        public void insertProcessedRequest(ProcessedRequest r) {
            try {
                jdbc.sql("""
                        INSERT INTO processed_requests
                            (caller_id, request_id, operation, transaction_id, request_hash, response, created_at)
                        VALUES (:caller_id, :request_id, :operation, :transaction_id, :request_hash, :response,
                                :created_at)
                        """)
                        .param("caller_id", r.callerId())
                        .param("request_id", r.requestId())
                        .param("operation", r.operation())
                        .param("transaction_id", r.transactionId())
                        .param("request_hash", r.requestHash())
                        .param("response", r.response())
                        .param("created_at", ts(r.createdAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .update();
            } catch (DuplicateKeyException e) {
                throw new ConflictException("request " + r.requestId() + " was recorded concurrently", e);
            }
        }

        @Override
        public void lockListing(UUID listingId) {
            long key = listingId.getMostSignificantBits() ^ listingId.getLeastSignificantBits() ^ LISTING_LOCK_SALT;
            jdbc.sql("SELECT 1 FROM pg_advisory_xact_lock(:key)").param("key", key).query(Integer.class).single();
        }

        @Override
        public Optional<EscrowTransaction> findOpenByInitiator(UUID listingId, UUID initiatorId) {
            // The same predicate as the partial unique index (EscrowState.isOpen).
            return jdbc.sql("SELECT " + TRANSACTION_COLUMNS + """
                    WHERE t.listing_id = :listing_id AND t.initiator_id = :initiator_id AND t.state <> 'COMPLETED'
                    """)
                    .param("listing_id", listingId)
                    .param("initiator_id", initiatorId)
                    .query(JdbcEscrowStore::mapTransaction)
                    .optional();
        }

        @Override
        public long committedGrams(UUID listingId) {
            // TODO(cancel): exclude cancelled transactions once the state exists.
            return jdbc.sql("""
                    SELECT COALESCE(SUM(quantity_grams), 0)::bigint FROM escrow_transactions
                    WHERE listing_id = :listing_id
                    """)
                    .param("listing_id", listingId)
                    .query(Long.class)
                    .single();
        }

        @Override
        public void insertTransaction(EscrowTransaction t) {
            try {
                jdbc.sql("""
                        INSERT INTO escrow_transactions (
                            id, listing_id, listing_kind, listing_version, farmer_id, reseller_id, initiator_id,
                            product, quantity_grams, quantity_unit, unit_price_minor, unit_price_unit, currency_code,
                            subtotal_minor, tax_minor, platform_fee_minor, total_minor, payout_minor,
                            address_region, address_district, address_line, address_latitude, address_longitude,
                            state, bonus_points, version, created_at, updated_at, payment_held_at,
                            receipt_submitted_at, handed_over_at, received_at, completed_at)
                        VALUES (
                            :id, :listing_id, :listing_kind, :listing_version, :farmer_id, :reseller_id,
                            :initiator_id, :product, :quantity_grams, :quantity_unit, :unit_price_minor,
                            :unit_price_unit, :currency_code, :subtotal_minor, :tax_minor, :platform_fee_minor,
                            :total_minor, :payout_minor, :address_region, :address_district, :address_line,
                            :address_latitude, :address_longitude, :state, :bonus_points, :version, :created_at,
                            :updated_at, :payment_held_at, :receipt_submitted_at, :handed_over_at, :received_at,
                            :completed_at)
                        """)
                        .param("id", t.id())
                        .param("listing_id", t.listingId())
                        .param("listing_kind", DbEnums.kind(t.listingKind()))
                        .param("listing_version", t.listingVersion())
                        .param("farmer_id", t.farmerId())
                        .param("reseller_id", t.resellerId())
                        .param("initiator_id", t.initiatorId())
                        .param("product", t.product().getNumber())
                        .param("quantity_grams", t.quantityGrams())
                        .param("quantity_unit", DbEnums.unit(t.quantityUnit()))
                        .param("unit_price_minor", t.unitPriceMinor())
                        .param("unit_price_unit", DbEnums.unit(t.unitPriceUnit()))
                        .param("currency_code", Amounts.CURRENCY)
                        .param("subtotal_minor", t.amounts().subtotalMinor())
                        .param("tax_minor", t.amounts().taxMinor())
                        .param("platform_fee_minor", t.amounts().platformFeeMinor())
                        .param("total_minor", t.amounts().totalMinor())
                        .param("payout_minor", t.amounts().payoutMinor())
                        .param("address_region", t.address().getRegionValue())
                        .param("address_district", t.address().getDistrict())
                        .param("address_line", t.address().getAddressLine())
                        .param("address_latitude", t.address().hasLocation() ? t.address().getLocation().getLatitude()
                                : null, Types.DOUBLE)
                        .param("address_longitude", t.address().hasLocation()
                                ? t.address().getLocation().getLongitude() : null, Types.DOUBLE)
                        .param("state", t.state().name())
                        .param("bonus_points", t.bonusPoints())
                        .param("version", t.version())
                        .param("created_at", ts(t.createdAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("updated_at", ts(t.updatedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("payment_held_at", ts(t.paymentHeldAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("receipt_submitted_at", ts(t.receiptSubmittedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("handed_over_at", ts(t.handedOverAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("received_at", ts(t.receivedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("completed_at", ts(t.completedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .update();
            } catch (DuplicateKeyException e) {
                throw new ConflictException("the initiator already has an open transaction on listing "
                        + t.listingId(), e);
            }
            insertChildren(null, t);
        }

        @Override
        public Optional<EscrowTransaction> findTransaction(UUID id, boolean forUpdate) {
            return jdbc.sql("SELECT " + TRANSACTION_COLUMNS + "WHERE t.id = :id" + (forUpdate ? " FOR UPDATE OF t" : ""))
                    .param("id", id)
                    .query(JdbcEscrowStore::mapTransaction)
                    .optional();
        }

        @Override
        public void updateTransaction(EscrowTransaction before, EscrowTransaction after) {
            if (!before.id().equals(after.id()) || after.version() <= before.version()
                    || !before.amounts().equals(after.amounts()) || before.quantityGrams() != after.quantityGrams()
                    || !before.farmerId().equals(after.farmerId()) || !before.resellerId().equals(after.resellerId())) {
                throw new IllegalArgumentException("an update may only move a transaction forward, not change its terms");
            }
            int rows = jdbc.sql("""
                    UPDATE escrow_transactions SET
                        state = :state, bonus_points = :bonus_points, version = :version, updated_at = :updated_at,
                        payment_held_at = :payment_held_at, receipt_submitted_at = :receipt_submitted_at,
                        handed_over_at = :handed_over_at, received_at = :received_at, completed_at = :completed_at
                    WHERE id = :id AND version = :expected_version
                    """)
                    .param("state", after.state().name())
                    .param("bonus_points", after.bonusPoints())
                    .param("version", after.version())
                    .param("updated_at", ts(after.updatedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("payment_held_at", ts(after.paymentHeldAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("receipt_submitted_at", ts(after.receiptSubmittedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("handed_over_at", ts(after.handedOverAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("received_at", ts(after.receivedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("completed_at", ts(after.completedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("id", after.id())
                    .param("expected_version", before.version())
                    .update();
            if (rows != 1) {
                throw new ConflictException("transaction " + after.id() + " is no longer at version "
                        + before.version());
            }
            insertChildren(before, after);
        }

        /** Inserts the records {@code after} has and {@code before} had not; releases the payment's hold. */
        private void insertChildren(EscrowTransaction before, EscrowTransaction after) {
            UUID id = after.id();
            Optional<PaymentRecord> oldPayment = before == null ? Optional.empty() : before.payment();
            after.payment().ifPresent(p -> {
                if (oldPayment.isEmpty()) {
                    insertPayment(id, p);
                } else if (p.released() && !oldPayment.get().released()) {
                    int rows = jdbc.sql("""
                            UPDATE payments SET released_at = :released_at
                            WHERE id = :id AND transaction_id = :transaction_id AND released_at IS NULL
                            """)
                            .param("released_at", ts(p.releasedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                            .param("id", p.id())
                            .param("transaction_id", id)
                            .update();
                    if (rows != 1) {
                        throw new ConflictException("payment " + p.id() + " was released concurrently");
                    }
                }
            });
            if (after.receipt().isPresent() && (before == null || before.receipt().isEmpty())) {
                insertReceipt(id, after.receipt().get());
            }
            if (after.payout().isPresent() && (before == null || before.payout().isEmpty())) {
                insertPayout(id, after.payout().get());
            }
        }

        private void insertPayment(UUID transactionId, PaymentRecord p) {
            try {
                jdbc.sql("""
                        INSERT INTO payments (id, transaction_id, provider, provider_payment_id, amount_minor,
                                              currency_code, held_at, released_at)
                        VALUES (:id, :transaction_id, :provider, :provider_payment_id, :amount_minor, :currency_code,
                                :held_at, :released_at)
                        """)
                        .param("id", p.id())
                        .param("transaction_id", transactionId)
                        .param("provider", p.provider())
                        .param("provider_payment_id", p.providerPaymentId())
                        .param("amount_minor", p.amountMinor())
                        .param("currency_code", Amounts.CURRENCY)
                        .param("held_at", ts(p.heldAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("released_at", ts(p.releasedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .update();
            } catch (DuplicateKeyException e) {
                throw new ConflictException("transaction " + transactionId + " already has a payment", e);
            }
        }

        private void insertReceipt(UUID transactionId, ReceiptRecord r) {
            try {
                jdbc.sql("""
                        INSERT INTO weighbridge_receipts (transaction_id, ticket_number, photo_media_id,
                            net_mass_grams, net_mass_unit, verification_status, submitted_by, submitted_at,
                            verified_at)
                        VALUES (:transaction_id, :ticket_number, :photo_media_id, :net_mass_grams, :net_mass_unit,
                                :verification_status, :submitted_by, :submitted_at, :verified_at)
                        """)
                        .param("transaction_id", transactionId)
                        .param("ticket_number", r.ticketNumber())
                        .param("photo_media_id", r.photoMediaId(), Types.OTHER)
                        .param("net_mass_grams", r.netMassGrams() > 0 ? r.netMassGrams() : null, Types.BIGINT)
                        .param("net_mass_unit", DbEnums.unit(r.netMassUnit()))
                        .param("verification_status", DbEnums.verification(r.verificationStatus()))
                        .param("submitted_by", r.submittedBy())
                        .param("submitted_at", ts(r.submittedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("verified_at", ts(r.verifiedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .update();
            } catch (DuplicateKeyException e) {
                throw new ConflictException("transaction " + transactionId + " already has a receipt", e);
            }
        }

        private void insertPayout(UUID transactionId, PayoutRecord p) {
            try {
                jdbc.sql("""
                        INSERT INTO payouts (id, transaction_id, payment_id, recipient_id, provider,
                                             provider_payout_id, amount_minor, currency_code, released_at)
                        VALUES (:id, :transaction_id, :payment_id, :recipient_id, :provider, :provider_payout_id,
                                :amount_minor, :currency_code, :released_at)
                        """)
                        .param("id", p.id())
                        .param("transaction_id", transactionId)
                        .param("payment_id", p.paymentId())
                        .param("recipient_id", p.recipientId())
                        .param("provider", p.provider())
                        .param("provider_payout_id", p.providerPayoutId())
                        .param("amount_minor", p.amountMinor())
                        .param("currency_code", Amounts.CURRENCY)
                        .param("released_at", ts(p.releasedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .update();
            } catch (DuplicateKeyException e) {
                throw new ConflictException("transaction " + transactionId + " is already paid out", e);
            }
        }

        @Override
        public void appendStateChange(StateChange c) {
            boolean user = c.actor().hasUserId();
            jdbc.sql("""
                    INSERT INTO transaction_state_changes
                        (transaction_id, version, state, actor_user_id, actor_service, changed_at)
                    VALUES (:transaction_id, :version, :state, :actor_user_id, :actor_service, :changed_at)
                    """)
                    .param("transaction_id", c.transactionId())
                    .param("version", c.version())
                    .param("state", c.state().name())
                    .param("actor_user_id", user ? UUID.fromString(c.actor().getUserId()) : null, Types.OTHER)
                    .param("actor_service", user ? null : c.actor().getService(), Types.VARCHAR)
                    .param("changed_at", ts(c.changedAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .update();
        }

        @Override
        public List<StateChange> history(UUID transactionId) {
            return jdbc.sql("""
                    SELECT transaction_id, version, state, actor_user_id, actor_service, changed_at
                    FROM transaction_state_changes WHERE transaction_id = :id ORDER BY version
                    """)
                    .param("id", transactionId)
                    .query(JdbcEscrowStore::mapStateChange)
                    .list();
        }

        @Override
        public Map<UUID, List<StateChange>> histories(Collection<UUID> transactionIds) {
            Map<UUID, List<StateChange>> out = new LinkedHashMap<>();
            if (transactionIds.isEmpty()) {
                return out;
            }
            List<StateChange> rows = jdbc.sql("""
                    SELECT transaction_id, version, state, actor_user_id, actor_service, changed_at
                    FROM transaction_state_changes WHERE transaction_id IN (:ids) ORDER BY transaction_id, version
                    """)
                    .param("ids", List.copyOf(transactionIds))
                    .query(JdbcEscrowStore::mapStateChange)
                    .list();
            for (StateChange c : rows) {
                out.computeIfAbsent(c.transactionId(), k -> new ArrayList<>()).add(c);
            }
            return out;
        }

        @Override
        public List<EscrowTransaction> listTransactions(ListQuery q) {
            StringBuilder where = new StringBuilder();
            Map<String, Object> params = new LinkedHashMap<>();
            params.put("me", q.callerId());
            if (q.side() == null) {
                where.append("(t.farmer_id = :me OR t.reseller_id = :me)");
            } else {
                where.append(q.side() == uz.agrogram.orderescrow.domain.Party.FARMER
                        ? "t.farmer_id = :me" : "t.reseller_id = :me");
            }
            if (!q.states().isEmpty()) {
                where.append(" AND t.state IN (:states)");
                params.put("states", q.states().stream().map(EscrowState::name).sorted().toList());
            }
            if (q.listingId() != null) {
                where.append(" AND t.listing_id = :listing_id");
                params.put("listing_id", q.listingId());
            }
            JdbcClient.StatementSpec spec;
            if (q.after() != null) {
                where.append(" AND (t.created_at, t.id) < (:after_created_at, :after_id)");
            }
            spec = jdbc.sql("SELECT " + TRANSACTION_COLUMNS + "WHERE " + where
                    + " ORDER BY t.created_at DESC, t.id DESC LIMIT :limit");
            for (Map.Entry<String, Object> e : params.entrySet()) {
                spec = spec.param(e.getKey(), e.getValue());
            }
            if (q.after() != null) {
                spec = spec.param("after_created_at", ts(q.after().createdAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                        .param("after_id", q.after().id());
            }
            return spec.param("limit", q.limit()).query(JdbcEscrowStore::mapTransaction).list();
        }

        @Override
        public void enqueueOutbox(OutboxMessage m) {
            jdbc.sql("""
                    INSERT INTO outbox (id, topic, record_key, payload, headers, event_type, created_at)
                    VALUES (:id, :topic, :record_key, :payload, CAST(:headers AS jsonb), :event_type, :created_at)
                    """)
                    .param("id", m.id())
                    .param("topic", m.topic())
                    .param("record_key", m.key())
                    .param("payload", m.value())
                    .param("headers", json.writeValueAsString(m.headers()))
                    .param("event_type", m.eventType())
                    .param("created_at", ts(m.createdAt()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .update();
        }
    }

    // =====================================================================================
    // OutboxStore
    // =====================================================================================

    @Override
    public int processOutbox(int limit, Publisher publisher) {
        int[] handed = {0};
        Exception[] failure = {null};
        transactions.executeWithoutResult(status -> {
            List<OutboxMessage> batch = jdbc.sql("""
                    SELECT id, topic, record_key, payload, headers::text AS headers, event_type, created_at
                    FROM outbox
                    WHERE published_at IS NULL
                    ORDER BY seq
                    LIMIT :limit
                    FOR UPDATE SKIP LOCKED
                    """)
                    .param("limit", limit)
                    .query((rs, i) -> new OutboxMessage(
                            uuid(rs, "id"),
                            rs.getString("topic"),
                            rs.getString("record_key"),
                            rs.getBytes("payload"),
                            json.readValue(rs.getString("headers"), new TypeReference<Map<String, String>>() {}),
                            rs.getString("event_type"),
                            instant(rs, "created_at")))
                    .list();
            if (batch.isEmpty()) {
                return;
            }
            handed[0] = batch.size();
            List<UUID> ids = batch.stream().map(OutboxMessage::id).toList();
            try {
                // The rows stay locked while publishing, so concurrent relays
                // never publish the same batch.
                publisher.publish(batch);
            } catch (Exception e) {
                if (e instanceof InterruptedException) {
                    Thread.currentThread().interrupt();
                }
                failure[0] = e;
                jdbc.sql("UPDATE outbox SET attempts = attempts + 1, last_error = left(:error, 1000) WHERE id IN (:ids)")
                        .param("error", String.valueOf(e))
                        .param("ids", ids)
                        .update();
                return;
            }
            jdbc.sql("UPDATE outbox SET published_at = :now WHERE id IN (:ids)")
                    .param("now", ts(clock.instant()), Types.TIMESTAMP_WITH_TIMEZONE)
                    .param("ids", ids)
                    .update();
        });
        if (failure[0] != null) {
            throw new PublishException(handed[0], failure[0]);
        }
        return handed[0];
    }

    @Override
    public long purgePublished(Instant before) {
        return jdbc.sql("DELETE FROM outbox WHERE published_at < :before")
                .param("before", ts(before), Types.TIMESTAMP_WITH_TIMEZONE)
                .update();
    }

    // =====================================================================================
    // Mapping
    // =====================================================================================

    private static EscrowTransaction mapTransaction(ResultSet rs, int rowNum) throws SQLException {
        UUID id = uuid(rs, "id");
        Address.Builder address = Address.newBuilder()
                .setRegionValue(rs.getInt("address_region"))
                .setDistrict(rs.getString("address_district"))
                .setAddressLine(rs.getString("address_line"));
        double lat = rs.getDouble("address_latitude");
        if (!rs.wasNull()) {
            address.setLocation(GeoPoint.newBuilder().setLatitude(lat).setLongitude(rs.getDouble("address_longitude")));
        }
        EscrowTransaction.Builder b = EscrowTransaction.builder()
                .id(id)
                .listingId(uuid(rs, "listing_id"))
                .listingKind(DbEnums.kind(rs.getString("listing_kind")))
                .listingVersion(rs.getLong("listing_version"))
                .farmerId(uuid(rs, "farmer_id"))
                .resellerId(uuid(rs, "reseller_id"))
                .initiatorId(uuid(rs, "initiator_id"))
                .product(productOf(rs.getInt("product")))
                .quantityGrams(rs.getLong("quantity_grams"))
                .quantityUnit(DbEnums.unit(rs.getString("quantity_unit")))
                .unitPriceMinor(rs.getLong("unit_price_minor"))
                .unitPriceUnit(DbEnums.unit(rs.getString("unit_price_unit")))
                .amounts(new Amounts(rs.getLong("subtotal_minor"), rs.getLong("tax_minor"),
                        rs.getLong("platform_fee_minor"), rs.getLong("total_minor"), rs.getLong("payout_minor")))
                .address(address.build())
                .state(EscrowState.valueOf(rs.getString("state")))
                .bonusPoints(rs.getLong("bonus_points"))
                .version(rs.getLong("version"))
                .createdAt(instant(rs, "created_at"))
                .updatedAt(instant(rs, "updated_at"))
                .paymentHeldAt(instant(rs, "payment_held_at"))
                .receiptSubmittedAt(instant(rs, "receipt_submitted_at"))
                .handedOverAt(instant(rs, "handed_over_at"))
                .receivedAt(instant(rs, "received_at"))
                .completedAt(instant(rs, "completed_at"));
        UUID paymentId = uuid(rs, "p_id");
        if (paymentId != null) {
            b.payment(new PaymentRecord(paymentId, rs.getString("p_provider"), rs.getString("p_provider_payment_id"),
                    rs.getLong("p_amount_minor"), instant(rs, "p_held_at"), instant(rs, "p_released_at")));
        }
        Instant submittedAt = instant(rs, "r_submitted_at");
        if (submittedAt != null) {
            long net = rs.getLong("r_net_mass_grams");
            b.receipt(new ReceiptRecord(rs.getString("r_ticket_number"), uuid(rs, "r_photo_media_id"),
                    rs.wasNull() ? 0L : net, DbEnums.unit(rs.getString("r_net_mass_unit")),
                    DbEnums.verification(rs.getString("r_verification_status")), uuid(rs, "r_submitted_by"),
                    submittedAt, instant(rs, "r_verified_at")));
        }
        UUID payoutId = uuid(rs, "o_id");
        if (payoutId != null) {
            b.payout(new PayoutRecord(payoutId, uuid(rs, "o_payment_id"), uuid(rs, "o_recipient_id"),
                    rs.getString("o_provider"), rs.getString("o_provider_payout_id"), rs.getLong("o_amount_minor"),
                    instant(rs, "o_released_at")));
        }
        return b.build();
    }

    private static StateChange mapStateChange(ResultSet rs, int rowNum) throws SQLException {
        UUID user = uuid(rs, "actor_user_id");
        Actor actor = user != null
                ? Actor.newBuilder().setUserId(user.toString()).build()
                : Actor.newBuilder().setService(rs.getString("actor_service")).build();
        return new StateChange(uuid(rs, "transaction_id"), rs.getLong("version"),
                EscrowState.valueOf(rs.getString("state")), actor, instant(rs, "changed_at"));
    }

    private static ProductType productOf(int number) {
        ProductType p = ProductType.forNumber(number);
        return p == null ? ProductType.UNRECOGNIZED : p;
    }

    private static UUID uuid(ResultSet rs, String column) throws SQLException {
        return rs.getObject(column, UUID.class);
    }

    private static Instant instant(ResultSet rs, String column) throws SQLException {
        OffsetDateTime t = rs.getObject(column, OffsetDateTime.class);
        return t == null ? null : t.toInstant();
    }

    private static OffsetDateTime ts(Instant i) {
        return i == null ? null : OffsetDateTime.ofInstant(i, ZoneOffset.UTC);
    }
}
