package uz.agrogram.orderescrow.testing;

import java.time.Clock;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Comparator;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.locks.ReentrantLock;
import java.util.function.Function;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.StateChange;
import uz.agrogram.orderescrow.events.OutboxMessage;
import uz.agrogram.orderescrow.events.OutboxStore;
import uz.agrogram.orderescrow.store.ConflictException;
import uz.agrogram.orderescrow.store.EscrowStore;
import uz.agrogram.orderescrow.store.EscrowTx;
import uz.agrogram.orderescrow.store.ListQuery;
import uz.agrogram.orderescrow.store.ProcessedRequest;

/**
 * An in-memory {@link EscrowStore} and {@link OutboxStore} with the semantics
 * the service relies on: units of work are serialized (like the row and
 * advisory locks), all-or-nothing (a failed unit leaves no trace), and the
 * unique keys and version checks of the schema raise {@link ConflictException}.
 */
public final class InMemoryEscrowStore implements EscrowStore, OutboxStore {

    /** An outbox row. */
    public static final class OutboxRow {
        public final long seq;
        public final OutboxMessage message;
        public Instant publishedAt;
        public int attempts;
        public String lastError;

        OutboxRow(long seq, OutboxMessage message) {
            this.seq = seq;
            this.message = message;
        }

        OutboxRow copy() {
            OutboxRow r = new OutboxRow(seq, message);
            r.publishedAt = publishedAt;
            r.attempts = attempts;
            r.lastError = lastError;
            return r;
        }
    }

    private static final class State {
        final Map<UUID, EscrowTransaction> transactions = new LinkedHashMap<>();
        final Map<UUID, List<StateChange>> history = new HashMap<>();
        final Map<String, ProcessedRequest> processed = new HashMap<>();
        final List<OutboxRow> outbox = new ArrayList<>();
        long nextSeq = 1;

        State copy() {
            State s = new State();
            s.transactions.putAll(transactions);
            history.forEach((k, v) -> s.history.put(k, new ArrayList<>(v)));
            s.processed.putAll(processed);
            outbox.forEach(r -> s.outbox.add(r.copy()));
            s.nextSeq = nextSeq;
            return s;
        }
    }

    private final ReentrantLock lock = new ReentrantLock();
    private final Clock clock;
    private final AtomicInteger failCommits = new AtomicInteger();
    private final AtomicInteger units = new AtomicInteger();
    private volatile State state = new State();

    public InMemoryEscrowStore(Clock clock) {
        this.clock = clock;
    }

    /** The next {@code n} units of work run completely and then lose a race (ConflictException, rolled back). */
    public void failNextCommits(int n) {
        failCommits.set(n);
    }

    /** How many units of work ran (committed or not). */
    public int unitsOfWork() {
        return units.get();
    }

    @Override
    public <T> T inTransaction(Function<EscrowTx, T> work) {
        lock.lock();
        try {
            units.incrementAndGet();
            State working = state.copy();
            T result = work.apply(new Tx(working));
            if (failCommits.getAndUpdate(n -> Math.max(0, n - 1)) > 0) {
                throw new ConflictException("injected: a concurrent transaction won");
            }
            state = working;
            return result;
        } finally {
            lock.unlock();
        }
    }

    // ---- Inspection (committed state) -------------------------------------------

    public List<EscrowTransaction> transactions() {
        return List.copyOf(state.transactions.values());
    }

    public Optional<EscrowTransaction> transaction(UUID id) {
        return Optional.ofNullable(state.transactions.get(id));
    }

    public List<StateChange> history(UUID id) {
        return List.copyOf(state.history.getOrDefault(id, List.of()));
    }

    public int processedRequests() {
        return state.processed.size();
    }

    /** Every outbox message, in outbox order. */
    public List<OutboxMessage> outbox() {
        return state.outbox.stream().map(r -> r.message).toList();
    }

    public List<OutboxRow> outboxRows() {
        return state.outbox.stream().map(OutboxRow::copy).toList();
    }

    // ---- OutboxStore ---------------------------------------------------------------

    @Override
    public int processOutbox(int limit, Publisher publisher) {
        lock.lock();
        try {
            List<OutboxRow> batch = state.outbox.stream().filter(r -> r.publishedAt == null)
                    .sorted(Comparator.comparingLong(r -> r.seq)).limit(limit).toList();
            if (batch.isEmpty()) {
                return 0;
            }
            try {
                publisher.publish(batch.stream().map(r -> r.message).toList());
            } catch (Exception e) {
                for (OutboxRow r : batch) {
                    r.attempts++;
                    r.lastError = String.valueOf(e);
                }
                throw new PublishException(batch.size(), e);
            }
            Instant now = clock.instant();
            batch.forEach(r -> r.publishedAt = now);
            return batch.size();
        } finally {
            lock.unlock();
        }
    }

    @Override
    public long purgePublished(Instant before) {
        lock.lock();
        try {
            int size = state.outbox.size();
            state.outbox.removeIf(r -> r.publishedAt != null && r.publishedAt.isBefore(before));
            return size - state.outbox.size();
        } finally {
            lock.unlock();
        }
    }

    // ---- EscrowTx --------------------------------------------------------------------

    private final class Tx implements EscrowTx {
        private final State s;

        Tx(State s) {
            this.s = s;
        }

        @Override
        public Optional<ProcessedRequest> findProcessedRequest(UUID callerId, UUID requestId) {
            return Optional.ofNullable(s.processed.get(callerId + "|" + requestId));
        }

        @Override
        public void insertProcessedRequest(ProcessedRequest request) {
            String key = request.callerId() + "|" + request.requestId();
            if (s.processed.containsKey(key)) {
                throw new ConflictException("processed_requests_pkey");
            }
            if (!s.transactions.containsKey(request.transactionId())) {
                throw new IllegalStateException("processed request references a missing transaction");
            }
            s.processed.put(key, request);
        }

        @Override
        public void lockListing(UUID listingId) {
            // Units of work are serialized already.
        }

        @Override
        public Optional<EscrowTransaction> findOpenByInitiator(UUID listingId, UUID initiatorId) {
            return s.transactions.values().stream()
                    .filter(t -> t.listingId().equals(listingId) && t.initiatorId().equals(initiatorId)
                            && t.state().isOpen())
                    .findFirst();
        }

        @Override
        public long committedGrams(UUID listingId) {
            return s.transactions.values().stream().filter(t -> t.listingId().equals(listingId))
                    .mapToLong(EscrowTransaction::quantityGrams).sum();
        }

        @Override
        public void insertTransaction(EscrowTransaction t) {
            if (s.transactions.containsKey(t.id())) {
                throw new ConflictException("escrow_transactions_pkey");
            }
            if (findOpenByInitiator(t.listingId(), t.initiatorId()).isPresent()) {
                throw new ConflictException("escrow_transactions_open_initiator_key");
            }
            s.transactions.put(t.id(), t);
        }

        @Override
        public Optional<EscrowTransaction> findTransaction(UUID id, boolean forUpdate) {
            return Optional.ofNullable(s.transactions.get(id));
        }

        @Override
        public void updateTransaction(EscrowTransaction before, EscrowTransaction after) {
            EscrowTransaction stored = s.transactions.get(after.id());
            if (stored == null || stored.version() != before.version()) {
                throw new ConflictException("stale version");
            }
            if (after.version() <= before.version() || !before.amounts().equals(after.amounts())) {
                throw new IllegalArgumentException("an update may only move a transaction forward");
            }
            if (before.payout().isPresent() && after.payout().isPresent()
                    && !before.payout().get().equals(after.payout().get())) {
                throw new ConflictException("payouts_transaction_id_key");
            }
            s.transactions.put(after.id(), after);
        }

        @Override
        public void appendStateChange(StateChange change) {
            List<StateChange> h = s.history.computeIfAbsent(change.transactionId(), k -> new ArrayList<>());
            if (h.stream().anyMatch(c -> c.version() == change.version())) {
                throw new ConflictException("transaction_state_changes_pkey");
            }
            h.add(change);
        }

        @Override
        public List<StateChange> history(UUID transactionId) {
            return s.history.getOrDefault(transactionId, List.of()).stream()
                    .sorted(Comparator.comparingLong(StateChange::version)).toList();
        }

        @Override
        public Map<UUID, List<StateChange>> histories(Collection<UUID> transactionIds) {
            Map<UUID, List<StateChange>> out = new LinkedHashMap<>();
            for (UUID id : transactionIds) {
                List<StateChange> h = history(id);
                if (!h.isEmpty()) {
                    out.put(id, h);
                }
            }
            return out;
        }

        @Override
        public List<EscrowTransaction> listTransactions(ListQuery q) {
            Comparator<EscrowTransaction> newestFirst = Comparator.comparing(EscrowTransaction::createdAt)
                    .thenComparing(EscrowTransaction::id).reversed();
            return s.transactions.values().stream()
                    .filter(t -> q.side() == null ? t.partyOf(q.callerId()).isPresent()
                            : t.partyOf(q.callerId()).filter(p -> p == q.side()).isPresent())
                    .filter(t -> q.states().isEmpty() || q.states().contains(t.state()))
                    .filter(t -> q.listingId() == null || q.listingId().equals(t.listingId()))
                    .filter(t -> q.after() == null || t.createdAt().isBefore(q.after().createdAt())
                            || (t.createdAt().equals(q.after().createdAt()) && t.id().compareTo(q.after().id()) < 0))
                    .sorted(newestFirst)
                    .limit(q.limit())
                    .toList();
        }

        @Override
        public void enqueueOutbox(OutboxMessage message) {
            if (s.outbox.stream().anyMatch(r -> r.message.id().equals(message.id()))) {
                throw new ConflictException("outbox_pkey");
            }
            s.outbox.add(new OutboxRow(s.nextSeq++, message));
        }
    }
}
