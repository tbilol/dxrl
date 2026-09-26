package uz.agrogram.orderescrow.service;

import com.google.protobuf.InvalidProtocolBufferException;
import com.google.protobuf.Message;
import com.google.protobuf.Parser;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Clock;
import java.time.Instant;
import java.time.temporal.ChronoUnit;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.EnumSet;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Optional;
import java.util.Set;
import java.util.UUID;
import java.util.function.Supplier;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.Actor;
import uz.agrogram.common.v1.Address;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.ProductType;
import uz.agrogram.common.v1.Region;
import uz.agrogram.common.v1.Role;
import uz.agrogram.common.v1.UnitPrice;
import uz.agrogram.events.v1.BonusAwarded;
import uz.agrogram.events.v1.ListingStatus;
import uz.agrogram.events.v1.PaymentHeld;
import uz.agrogram.events.v1.PayoutReleased;
import uz.agrogram.events.v1.ProductHandover;
import uz.agrogram.events.v1.ReceiptConfirmed;
import uz.agrogram.events.v1.ReceiptSubmitted;
import uz.agrogram.events.v1.TransactionCreated;
import uz.agrogram.events.v1.TransactionState;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.auth.Uuids;
import uz.agrogram.orderescrow.catalog.ListingLookup;
import uz.agrogram.orderescrow.domain.Amounts;
import uz.agrogram.orderescrow.domain.Command;
import uz.agrogram.orderescrow.domain.EscrowState;
import uz.agrogram.orderescrow.domain.EscrowTransaction;
import uz.agrogram.orderescrow.domain.MoneyMath;
import uz.agrogram.orderescrow.domain.Party;
import uz.agrogram.orderescrow.domain.PaymentRecord;
import uz.agrogram.orderescrow.domain.PayoutRecord;
import uz.agrogram.orderescrow.domain.Protos;
import uz.agrogram.orderescrow.domain.ReceiptRecord;
import uz.agrogram.orderescrow.domain.StateChange;
import uz.agrogram.orderescrow.domain.StateMachine;
import uz.agrogram.orderescrow.domain.StateMachine.StepActor;
import uz.agrogram.orderescrow.domain.TaxPolicy;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.events.OutboxMessage;
import uz.agrogram.orderescrow.events.TransactionEvents;
import uz.agrogram.orderescrow.events.TransactionEvents.Origin;
import uz.agrogram.orderescrow.store.ConflictException;
import uz.agrogram.orderescrow.store.EscrowStore;
import uz.agrogram.orderescrow.store.EscrowTx;
import uz.agrogram.orderescrow.store.ListQuery;
import uz.agrogram.orderescrow.store.ProcessedRequest;
import uz.agrogram.orderescrow.v1.ConfirmHandoverRequest;
import uz.agrogram.orderescrow.v1.ConfirmHandoverResponse;
import uz.agrogram.orderescrow.v1.ConfirmReceiptRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptResponse;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.CreateTransactionResponse;
import uz.agrogram.orderescrow.v1.GetTransactionRequest;
import uz.agrogram.orderescrow.v1.GetTransactionResponse;
import uz.agrogram.orderescrow.v1.ListTransactionsRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsResponse;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.PayInResponse;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptRequest;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptResponse;
import uz.agrogram.orderescrow.v1.Transaction;

/**
 * OrderEscrowService's use cases: the escrow state machine end to end.
 *
 * <p><b>Atomicity.</b> Every command is one database transaction: the state
 * change, the payment/receipt/payout record, the history entry, the outbox
 * events and the idempotency record commit together or not at all.
 *
 * <p><b>Idempotency.</b> Commands are idempotent on (caller, request_id): the
 * first successful call records its response; a retry returns that response
 * without side effects. The same request_id used for a different operation,
 * transaction or payload is VALIDATION_FAILED on request_id. Failed calls are
 * not recorded (they had no side effects), so they can be retried.
 *
 * <p><b>Concurrency.</b> A command locks the transaction row first (SELECT ...
 * FOR UPDATE), then reads the idempotency record, so concurrent commands on one
 * transaction run one after the other and a concurrent duplicate sees the
 * winner's record. The row is written with a conditional update on the version
 * it was read at; any lost race (stale version, unique key, deadlock,
 * serialization failure) rolls back and retries the whole unit, which then
 * resolves to the winner's result. Creation serializes per listing with an
 * advisory lock.
 *
 * <p><b>Payout.</b> With the stub provider the payout is immediate, so
 * ConfirmReceipt completes the transaction in the same database transaction:
 * HANDED_OVER -> RECEIVED (ReceiptConfirmed) -> COMPLETED (PayoutReleased) and
 * then BonusAwarded, all or nothing. No transaction can be stranded in
 * RECEIVED, and a transaction gets at most one payout (payouts.transaction_id
 * is UNIQUE, and COMPLETED has no way out). TODO(payments): with a real
 * provider the payout is asynchronous; RECEIVED then becomes a real waiting
 * state and a worker must finish RECEIVED transactions (idempotently, keyed by
 * the payouts row).
 */
public final class EscrowService {

    private static final Logger log = LoggerFactory.getLogger(EscrowService.class);

    /** Attempts of a unit of work that loses races before CONFLICT (ABORTED) is returned. */
    static final int MAX_ATTEMPTS = 4;

    static final int DEFAULT_PAGE_SIZE = 20;
    static final int MAX_PAGE_SIZE = 100;

    static final String OP_CREATE = "CreateTransaction";

    private final EscrowStore store;
    private final ListingLookup catalog;
    private final TransactionEvents events;
    private final Clock clock;
    private final TaxPolicy taxPolicy;
    private final Supplier<UUID> ids;
    private final long bonusPlaceholderPoints;

    /**
     * @param bonusPlaceholderPoints STUB: TODO(bonus) points credited to the
     *     reseller on completion; 0 publishes no BonusAwarded
     */
    public EscrowService(EscrowStore store, ListingLookup catalog, TransactionEvents events, Clock clock,
            TaxPolicy taxPolicy, Supplier<UUID> ids, long bonusPlaceholderPoints) {
        this.store = Objects.requireNonNull(store);
        this.catalog = Objects.requireNonNull(catalog);
        this.events = Objects.requireNonNull(events);
        this.clock = Objects.requireNonNull(clock);
        this.taxPolicy = Objects.requireNonNull(taxPolicy);
        this.ids = Objects.requireNonNull(ids);
        if (bonusPlaceholderPoints < 0) {
            throw new IllegalArgumentException("bonus points must not be negative");
        }
        this.bonusPlaceholderPoints = bonusPlaceholderPoints;
    }

    /** The production wiring: UUIDv7 ids, no tax (TODO(tax)). */
    public EscrowService(EscrowStore store, ListingLookup catalog, TransactionEvents events, Clock clock,
            long bonusPlaceholderPoints) {
        this(store, catalog, events, clock, TaxPolicy.NONE, Uuids::newV7, bonusPlaceholderPoints);
    }

    // =====================================================================================
    // CreateTransaction
    // =====================================================================================

    /**
     * Starts a transaction on a listing, or returns the caller's open one on it.
     *
     * <p>Flow: (1) under the listing lock, a retry returns its recorded result and
     * an open transaction of the caller on the listing is returned as is, without
     * calling catalog; (2) otherwise the listing is read from catalog, outside any
     * database transaction; (3) under the listing lock again, the checks of (1)
     * are repeated, the quantity still available is computed, and the
     * transaction is created with its TransactionCreated event.
     */
    public CreateTransactionResponse createTransaction(Principal p, CreateTransactionRequest req) {
        UUID caller = p.requireUser();
        UUID requestId = requireId(req.getRequestId(), "request_id");
        UUID listingId = requireId(req.getListingId(), "listing_id");
        byte[] hash = requestHash(req);
        Origin origin = Origin.of(p);

        Terms terms = null;
        for (int attempt = 1;; attempt++) {
            final Terms known = terms;
            CreateOutcome outcome;
            try {
                outcome = store.inTransaction(tx -> create(tx, p, caller, requestId, listingId, hash, known, origin));
            } catch (ConflictException e) {
                if (attempt >= MAX_ATTEMPTS) {
                    throw conflict(e);
                }
                log.atDebug().addKeyValue("attempt", attempt).addKeyValue("error", e.getMessage())
                        .log("create transaction lost a race; retrying");
                continue;
            }
            if (outcome.response() != null) {
                return outcome.response();
            }
            if (terms != null) {
                throw new IllegalStateException("listing terms requested twice");
            }
            terms = fetchTerms(p, caller, listingId, req);
            attempt--; // fetching the listing is not a lost race
        }
    }

    /** Either a response, or a request for the listing terms (response null). */
    private record CreateOutcome(CreateTransactionResponse response) {}

    private CreateOutcome create(EscrowTx tx, Principal p, UUID caller, UUID requestId, UUID listingId, byte[] hash,
            Terms terms, Origin origin) {
        tx.lockListing(listingId);
        Optional<ProcessedRequest> done = tx.findProcessedRequest(caller, requestId);
        if (done.isPresent()) {
            return new CreateOutcome(replay(done.get(), OP_CREATE, null, hash, CreateTransactionResponse.parser()));
        }
        Instant now = now();
        Optional<EscrowTransaction> open = tx.findOpenByInitiator(listingId, caller);
        if (open.isPresent()) {
            EscrowTransaction t = open.get();
            CreateTransactionResponse response = CreateTransactionResponse.newBuilder()
                    .setTransaction(view(tx, t, caller, p))
                    .build();
            tx.insertProcessedRequest(
                    new ProcessedRequest(caller, requestId, OP_CREATE, t.id(), hash, response.toByteArray(), now));
            log.atInfo().addKeyValue("transaction_id", t.id()).addKeyValue("listing_id", listingId)
                    .log("create transaction: returning the caller's open transaction");
            return new CreateOutcome(response);
        }
        if (terms == null) {
            return new CreateOutcome(null);
        }

        // Oversell guard. The advisory lock serializes creations on this listing,
        // and this sum runs after it, so it sees every committed transaction.
        // Uses the listing's total quantity, not catalog's remaining_quantity:
        // catalog will subtract escrow commitments from this very stream
        // (TODO(phase3) there), and counting them twice would undersell.
        long committed = tx.committedGrams(listingId);
        long available = terms.listingGrams() - committed;
        long grams;
        if (terms.requestedGrams() == null) {
            if (available <= 0) {
                throw insufficient(listingId, 0, available);
            }
            grams = available;
        } else {
            if (terms.requestedGrams() > available) {
                throw insufficient(listingId, terms.requestedGrams(), available);
            }
            grams = terms.requestedGrams();
        }
        Amounts amounts = amounts(terms, grams);

        UUID id = ids.get();
        Actor actor = Principal.userActor(caller);
        EscrowTransaction t = EscrowTransaction.builder()
                .id(id)
                .listingId(listingId)
                .listingKind(terms.kind())
                .listingVersion(terms.listingVersion())
                .farmerId(terms.farmerId())
                .resellerId(terms.resellerId())
                .initiatorId(caller)
                .product(terms.product())
                .quantityGrams(grams)
                .quantityUnit(terms.quantityUnit())
                .unitPriceMinor(terms.unitPriceMinor())
                .unitPriceUnit(terms.unitPriceUnit())
                .address(terms.address())
                .amounts(amounts)
                .state(EscrowState.AWAITING_PAYMENT)
                .version(1)
                .createdAt(now)
                .updatedAt(now)
                .build();
        tx.insertTransaction(t);
        tx.appendStateChange(new StateChange(id, t.version(), t.state(), actor, now));
        tx.enqueueOutbox(events.build(t, TransactionCreated.getDefaultInstance(), actor, now, origin, null));

        CreateTransactionResponse response = CreateTransactionResponse.newBuilder()
                .setTransaction(view(tx, t, caller, p))
                .build();
        tx.insertProcessedRequest(
                new ProcessedRequest(caller, requestId, OP_CREATE, id, hash, response.toByteArray(), now));
        log.atInfo().addKeyValue("transaction_id", id).addKeyValue("listing_id", listingId)
                .addKeyValue("listing_kind", t.listingKind().name()).addKeyValue("grams", grams)
                .addKeyValue("total_minor", amounts.totalMinor()).log("transaction created");
        return new CreateOutcome(response);
    }

    /**
     * The agreed terms, from the listing (catalog) and the request.
     *
     * @param requestedGrams null: everything the listing still has
     */
    private record Terms(
            ListingKind kind,
            long listingVersion,
            UUID farmerId,
            UUID resellerId,
            ProductType product,
            long listingGrams,
            Long requestedGrams,
            MassUnit quantityUnit,
            long unitPriceMinor,
            MassUnit unitPriceUnit,
            Address address) {}

    private Terms fetchTerms(Principal p, UUID caller, UUID listingId, CreateTransactionRequest req) {
        Listing listing = catalog.getListing(p, listingId);

        UUID owner = Uuids.parseCanonical(listing.getOwnerId())
                .orElseThrow(() -> badListing(listingId, "owner_id is not a UUID"));
        ListingKind kind = listing.getKind();
        if (kind != ListingKind.LISTING_KIND_OFFER && kind != ListingKind.LISTING_KIND_BUY_REQUEST) {
            throw badListing(listingId, "kind is " + kind);
        }
        if (owner.equals(caller)) {
            throw AppException.of(ErrorReason.ERROR_REASON_SELF_TRADE_NOT_ALLOWED,
                    "the listing is the caller's own; a party cannot trade with itself",
                    "listing_id", listingId.toString());
        }
        if (listing.getStatus() != ListingStatus.LISTING_STATUS_ACTIVE) {
            throw AppException.of(ErrorReason.ERROR_REASON_LISTING_NOT_ACTIVE, "the listing is not active",
                    "listing_id", listingId.toString(), "status", trim(listing.getStatus().name(), "LISTING_STATUS_"));
        }
        // The caller is the counterparty of the owner: a reseller on an offer, a
        // farmer on a buy-request (roles are the token-time snapshot).
        UUID farmerId;
        UUID resellerId;
        if (kind == ListingKind.LISTING_KIND_OFFER) {
            if (!p.hasRole(Role.ROLE_RESELLER)) {
                throw AppException.of(ErrorReason.ERROR_REASON_PERMISSION_DENIED,
                        "buying from an offer requires ROLE_RESELLER",
                        "listing_kind", "OFFER", "required_role", "RESELLER");
            }
            farmerId = owner;
            resellerId = caller;
        } else {
            if (!p.hasRole(Role.ROLE_FARMER)) {
                throw AppException.of(ErrorReason.ERROR_REASON_FARMER_VERIFICATION_REQUIRED,
                        "selling to a buy-request requires an e-ijara-verified farmer (ROLE_FARMER)",
                        "listing_kind", "BUY_REQUEST", "required_role", "FARMER");
            }
            farmerId = caller;
            resellerId = owner;
        }

        ProductType product = listing.getProduct();
        if (product == ProductType.PRODUCT_TYPE_UNSPECIFIED || product == ProductType.UNRECOGNIZED) {
            throw badListing(listingId, "product is " + product);
        }
        long listingGrams = listing.getQuantity().getGrams();
        if (listingGrams <= 0) {
            throw badListing(listingId, "quantity is not positive");
        }
        Address address = listing.getAddress();
        if (!listing.hasAddress() || address.getRegion() == Region.REGION_UNSPECIFIED
                || address.getRegion() == Region.UNRECOGNIZED) {
            throw badListing(listingId, "address has no region");
        }

        UnitPrice price;
        if (listing.hasPrice()) {
            if (req.hasUnitPrice()) {
                throw AppException.invalidField("unit_price",
                        "unit_price must be unset: the listing has a price, which is used",
                        "create_transaction.unit_price_not_allowed");
            }
            price = listing.getPrice();
            if (!Amounts.CURRENCY.equals(price.getAmount().getCurrencyCode()) || price.getAmount().getAmountMinor() <= 0
                    || !isConcreteUnit(price.getUnit())) {
                throw badListing(listingId, "price is not a positive UZS amount per kg or tonne");
            }
        } else {
            // TODO(negotiation): the owner has no step to accept this price; for
            // now they accept by carrying on with the transaction.
            if (!req.hasUnitPrice()) {
                throw AppException.invalidField("unit_price",
                        "unit_price is required: the listing has no price (negotiable)",
                        "create_transaction.unit_price_required");
            }
            price = req.getUnitPrice();
            if (!isConcreteUnit(price.getUnit())) {
                throw AppException.invalidField("unit_price.unit", "unit must be KILOGRAM or TONNE",
                        "enum.defined_only");
            }
        }

        Long requested = req.hasQuantity() ? req.getQuantity().getGrams() : null;
        MassUnit quantityUnit = req.hasQuantity() && isConcreteUnit(req.getQuantity().getDisplayUnit())
                ? req.getQuantity().getDisplayUnit()
                : listing.getQuantity().getDisplayUnit();
        if (quantityUnit == MassUnit.UNRECOGNIZED) {
            quantityUnit = MassUnit.MASS_UNIT_UNSPECIFIED;
        }
        return new Terms(kind, listing.getVersion(), farmerId, resellerId, product, listingGrams, requested,
                quantityUnit, price.getAmount().getAmountMinor(), price.getUnit(), address);
    }

    private Amounts amounts(Terms terms, long grams) {
        long subtotal;
        try {
            subtotal = MoneyMath.lineTotal(terms.unitPriceMinor(), terms.unitPriceUnit(), grams);
        } catch (ArithmeticException e) {
            throw AppException.invalidField("quantity", "the transaction total is too large",
                    "create_transaction.total_too_large");
        }
        if (subtotal <= 0) {
            throw AppException.invalidField("quantity", "the transaction total rounds to zero tiyin",
                    "create_transaction.total_zero");
        }
        try {
            return Amounts.compute(terms.unitPriceMinor(), terms.unitPriceUnit(), grams, taxPolicy);
        } catch (ArithmeticException e) {
            throw AppException.invalidField("quantity", "the transaction total is too large",
                    "create_transaction.total_too_large");
        }
    }

    private static boolean isConcreteUnit(MassUnit unit) {
        return unit == MassUnit.MASS_UNIT_KILOGRAM || unit == MassUnit.MASS_UNIT_TONNE;
    }

    private static AppException insufficient(UUID listingId, long requested, long available) {
        return AppException.of(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY,
                "the requested quantity exceeds what the listing still offers",
                "listing_id", listingId.toString(),
                "requested_grams", Long.toString(requested),
                "available_grams", Long.toString(Math.max(available, 0)));
    }

    private static AppException badListing(UUID listingId, String problem) {
        return AppException.internal("catalog returned an unusable listing",
                new IllegalStateException("listing " + listingId + ": " + problem));
    }

    // =====================================================================================
    // Steps
    // =====================================================================================

    /** One step's changes inside the command's database transaction; returns the transaction after it. */
    @FunctionalInterface
    private interface Step {
        EscrowTransaction apply(EscrowTx tx, EscrowTransaction before, Instant now, Actor actor, Origin origin);
    }

    /**
     * Reseller: AWAITING_PAYMENT -> PAYMENT_HELD (PaymentHeld).
     *
     * <p>STUB: TODO(payments) no payment provider: the funds are recorded as held
     * at once and no money moves; return_url is ignored and checkout_url stays
     * empty. PAYMENT_FAILED cannot happen yet.
     */
    public PayInResponse payIn(Principal p, PayInRequest req) {
        return runStep(p, Command.PAY_IN, req.getRequestId(), req.getTransactionId(), req, PayInResponse.parser(),
                (tx, t, now, actor, origin) -> {
                    PaymentRecord held = PaymentRecord.stubHeld(ids.get(), t.amounts().totalMinor(), now);
                    EscrowTransaction after = t.payIn(held, now);
                    tx.updateTransaction(t, after);
                    tx.appendStateChange(new StateChange(after.id(), after.version(), after.state(), actor, now));
                    tx.enqueueOutbox(events.build(after,
                            PaymentHeld.newBuilder().setAmount(Amounts.money(held.amountMinor())).build(),
                            actor, now, origin, null));
                    return after;
                },
                view -> PayInResponse.newBuilder().setTransaction(view).build());
    }

    /**
     * Reseller: PAYMENT_HELD -> RECEIPT_SUBMITTED (ReceiptSubmitted).
     *
     * <p>STUB: TODO(weighbridge) the receipt is stored and accepted as VERIFIED at
     * once; RECEIPT_REJECTED cannot happen yet. TODO(media): photo_media_id is not
     * checked with media-pipeline.
     */
    public SubmitWeighbridgeReceiptResponse submitWeighbridgeReceipt(Principal p, SubmitWeighbridgeReceiptRequest req) {
        return runStep(p, Command.SUBMIT_WEIGHBRIDGE_RECEIPT, req.getRequestId(), req.getTransactionId(), req,
                SubmitWeighbridgeReceiptResponse.parser(),
                (tx, t, now, actor, origin) -> {
                    UUID photo = req.getPhotoMediaId().isEmpty() ? null : requireId(req.getPhotoMediaId(),
                            "photo_media_id");
                    long netGrams = req.hasNetMass() ? req.getNetMass().getGrams() : 0L;
                    MassUnit netUnit = req.hasNetMass() && req.getNetMass().getDisplayUnit() != MassUnit.UNRECOGNIZED
                            ? req.getNetMass().getDisplayUnit()
                            : MassUnit.MASS_UNIT_UNSPECIFIED;
                    ReceiptRecord receipt = ReceiptRecord.stubVerified(req.getTicketNumber(), photo, netGrams, netUnit,
                            t.resellerId(), now);
                    EscrowTransaction after = t.submitReceipt(receipt, now);
                    tx.updateTransaction(t, after);
                    tx.appendStateChange(new StateChange(after.id(), after.version(), after.state(), actor, now));
                    tx.enqueueOutbox(events.build(after,
                            ReceiptSubmitted.newBuilder().setReceipt(Protos.receipt(receipt)).build(),
                            actor, now, origin, null));
                    return after;
                },
                view -> SubmitWeighbridgeReceiptResponse.newBuilder().setTransaction(view).build());
    }

    /** Farmer: RECEIPT_SUBMITTED -> HANDED_OVER (ProductHandover). */
    public ConfirmHandoverResponse confirmHandover(Principal p, ConfirmHandoverRequest req) {
        return runStep(p, Command.CONFIRM_HANDOVER, req.getRequestId(), req.getTransactionId(), req,
                ConfirmHandoverResponse.parser(),
                (tx, t, now, actor, origin) -> {
                    EscrowTransaction after = t.confirmHandover(now);
                    tx.updateTransaction(t, after);
                    tx.appendStateChange(new StateChange(after.id(), after.version(), after.state(), actor, now));
                    tx.enqueueOutbox(events.build(after, ProductHandover.getDefaultInstance(), actor, now, origin,
                            null));
                    return after;
                },
                view -> ConfirmHandoverResponse.newBuilder().setTransaction(view).build());
    }

    /**
     * Reseller: HANDED_OVER -> RECEIVED (ReceiptConfirmed), then, as order-escrow
     * in the same database transaction, the payout RECEIVED -> COMPLETED
     * (PayoutReleased) and the bonus (BonusAwarded). The three events share the
     * command's correlation id; PayoutReleased's causation id is ReceiptConfirmed,
     * BonusAwarded's is PayoutReleased.
     *
     * <p>STUB: TODO(payments) the payout is recorded as released at once and no
     * money moves. STUB: TODO(bonus) the bonus rule is not designed: a fixed
     * placeholder number of points (configuration) goes to the reseller.
     */
    public ConfirmReceiptResponse confirmReceipt(Principal p, ConfirmReceiptRequest req) {
        return runStep(p, Command.CONFIRM_RECEIPT, req.getRequestId(), req.getTransactionId(), req,
                ConfirmReceiptResponse.parser(),
                (tx, t, now, actor, origin) -> {
                    EscrowTransaction received = t.confirmReceipt(now);
                    tx.appendStateChange(
                            new StateChange(received.id(), received.version(), received.state(), actor, now));
                    OutboxMessage confirmed = events.build(received, ReceiptConfirmed.getDefaultInstance(), actor,
                            now, origin, null);
                    tx.enqueueOutbox(confirmed);

                    Released released = releasePayout(tx, received, now, origin, confirmed.id());
                    EscrowTransaction last = awardBonus(tx, released.transaction(), now, origin, released.eventId());
                    tx.updateTransaction(t, last);
                    log.atInfo().addKeyValue("transaction_id", last.id())
                            .addKeyValue("payout_minor", last.amounts().payoutMinor())
                            .addKeyValue("bonus_points", last.bonusPoints()).log("transaction completed");
                    return last;
                },
                view -> ConfirmReceiptResponse.newBuilder().setTransaction(view).build());
    }

    /** The completed transaction and the event_id of its PayoutReleased. */
    private record Released(EscrowTransaction transaction, UUID eventId) {}

    /** System step RECEIVED -> COMPLETED. STUB: TODO(payments). */
    private Released releasePayout(EscrowTx tx, EscrowTransaction received, Instant now, Origin origin,
            UUID causationId) {
        StateMachine.next(Command.RELEASE_PAYOUT, received.state(), StepActor.SYSTEM);
        PaymentRecord payment = received.payment().orElseThrow();
        PayoutRecord payout = PayoutRecord.stubReleased(ids.get(), payment.id(), received.farmerId(),
                received.amounts().payoutMinor(), now);
        EscrowTransaction completed = received.releasePayout(payout, now);
        Actor system = Principal.serviceActor();
        tx.appendStateChange(new StateChange(completed.id(), completed.version(), completed.state(), system, now));
        OutboxMessage released = events.build(completed,
                PayoutReleased.newBuilder().setAmount(Amounts.money(payout.amountMinor())).build(),
                system, now, origin, causationId);
        tx.enqueueOutbox(released);
        return new Released(completed, released.id());
    }

    /**
     * STUB: TODO(bonus) credits the placeholder points to the reseller. BonusAwarded
     * changes the snapshot (bonus_points), so it gets its own version. Consumers
     * must not treat the points as real until the bonus rule exists.
     */
    private EscrowTransaction awardBonus(EscrowTx tx, EscrowTransaction completed, Instant now, Origin origin,
            UUID causationId) {
        if (bonusPlaceholderPoints <= 0) {
            return completed;
        }
        EscrowTransaction awarded = completed.awardBonus(bonusPlaceholderPoints, now);
        tx.enqueueOutbox(events.build(awarded, BonusAwarded.newBuilder()
                .setRecipientId(awarded.resellerId().toString())
                .setPoints(bonusPlaceholderPoints)
                .build(), Principal.serviceActor(), now, origin, causationId));
        return awarded;
    }

    /**
     * Runs one step: lock the transaction, hide it from non-parties, replay a
     * retry, check the step with the state machine, apply it, record the result.
     */
    private <R extends Message> R runStep(Principal p, Command command, String rawRequestId, String rawTransactionId,
            Message request, Parser<R> parser, Step step, java.util.function.Function<Transaction, R> respond) {
        UUID caller = p.requireUser();
        UUID requestId = requireId(rawRequestId, "request_id");
        UUID transactionId = requireId(rawTransactionId, "transaction_id");
        byte[] hash = requestHash(request);
        Origin origin = Origin.of(p);
        return retrying(() -> store.inTransaction(tx -> {
            EscrowTransaction t = tx.findTransaction(transactionId, true)
                    .orElseThrow(() -> notFound(transactionId));
            Optional<Party> party = t.partyOf(caller);
            if (party.isEmpty() && !p.staff()) {
                throw notFound(transactionId); // do not reveal it exists
            }
            Optional<ProcessedRequest> done = tx.findProcessedRequest(caller, requestId);
            if (done.isPresent()) {
                return replay(done.get(), command.rpcName(), transactionId, hash, parser);
            }
            StateMachine.next(command, t.state(), party.map(StepActor::of).orElse(StepActor.STAFF));
            Instant now = now();
            EscrowTransaction after = step.apply(tx, t, now, Principal.userActor(caller), origin);
            R response = respond.apply(
                    Protos.transaction(after, party.orElseThrow().role(), tx.history(transactionId)));
            tx.insertProcessedRequest(new ProcessedRequest(caller, requestId, command.rpcName(), transactionId, hash,
                    response.toByteArray(), now));
            log.atInfo().addKeyValue("transaction_id", transactionId).addKeyValue("command", command.rpcName())
                    .addKeyValue("state", after.state().name()).addKeyValue("version", after.version())
                    .log("transaction step");
            return response;
        }));
    }

    // =====================================================================================
    // Queries
    // =====================================================================================

    /** A transaction, for its two parties and staff; NOT_FOUND for everyone else. */
    public GetTransactionResponse getTransaction(Principal p, GetTransactionRequest req) {
        UUID caller = p.requireUser();
        UUID id = requireId(req.getTransactionId(), "transaction_id");
        return store.inTransaction(tx -> {
            EscrowTransaction t = tx.findTransaction(id, false).orElseThrow(() -> notFound(id));
            if (t.partyOf(caller).isEmpty() && !p.staff()) {
                throw notFound(id);
            }
            return GetTransactionResponse.newBuilder().setTransaction(view(tx, t, caller, p)).build();
        });
    }

    /** The caller's transactions, newest first, with a cursor for the next page. */
    public ListTransactionsResponse listTransactions(Principal p, ListTransactionsRequest req) {
        UUID caller = p.requireUser();
        Party side;
        switch (req.getRole()) {
            case ROLE_FARMER:
                side = Party.FARMER;
                break;
            case ROLE_RESELLER:
                side = Party.RESELLER;
                break;
            case ROLE_UNSPECIFIED:
                side = null;
                break;
            default:
                throw AppException.invalidField("role", "role must be ROLE_FARMER, ROLE_RESELLER or unspecified",
                        "enum.in");
        }
        Set<EscrowState> states = EnumSet.noneOf(EscrowState.class);
        for (TransactionState s : req.getStatesList()) {
            try {
                states.add(EscrowState.fromProto(s));
            } catch (IllegalArgumentException e) {
                throw AppException.invalidField("states", "unknown state " + s, "enum.defined_only");
            }
        }
        UUID listingId = req.getListingId().isEmpty() ? null : requireId(req.getListingId(), "listing_id");
        int requested = req.getPage().getPageSize();
        int size = requested <= 0 ? DEFAULT_PAGE_SIZE : Math.min(requested, MAX_PAGE_SIZE);
        String fingerprint = PageTokens.fingerprint(caller, side, states, listingId);
        String token = req.getPage().getPageToken();
        ListQuery.Cursor after = token.isEmpty() ? null : PageTokens.decode(token, fingerprint);

        return store.inTransaction(tx -> {
            List<EscrowTransaction> rows = tx.listTransactions(
                    new ListQuery(caller, side, states, listingId, after, size + 1));
            boolean more = rows.size() > size;
            List<EscrowTransaction> page = more ? rows.subList(0, size) : rows;
            List<UUID> pageIds = new ArrayList<>(page.size());
            for (EscrowTransaction t : page) {
                pageIds.add(t.id());
            }
            Map<UUID, List<StateChange>> histories = tx.histories(pageIds);
            ListTransactionsResponse.Builder b = ListTransactionsResponse.newBuilder();
            for (EscrowTransaction t : page) {
                b.addTransactions(Protos.transaction(t, myRole(t, caller, p), histories.getOrDefault(t.id(), List.of())));
            }
            uz.agrogram.common.v1.PageResponse.Builder pr = uz.agrogram.common.v1.PageResponse.newBuilder();
            if (more) {
                EscrowTransaction last = page.get(page.size() - 1);
                pr.setNextPageToken(PageTokens.encode(fingerprint, new ListQuery.Cursor(last.createdAt(), last.id())));
            }
            return b.setPage(pr).build();
        });
    }

    // =====================================================================================
    // Helpers
    // =====================================================================================

    private Transaction view(EscrowTx tx, EscrowTransaction t, UUID caller, Principal p) {
        return Protos.transaction(t, myRole(t, caller, p), tx.history(t.id()));
    }

    /** The caller's side of the transaction, or ROLE_ADMIN for staff who are not a party. */
    private static Role myRole(EscrowTransaction t, UUID caller, Principal p) {
        return t.partyOf(caller).map(Party::role).orElse(p.staff() ? Role.ROLE_ADMIN : Role.ROLE_UNSPECIFIED);
    }

    /** "now" at the database's precision (microseconds), so stored and returned times agree. */
    private Instant now() {
        return clock.instant().truncatedTo(ChronoUnit.MICROS);
    }

    private <T> T retrying(Supplier<T> unit) {
        for (int attempt = 1;; attempt++) {
            try {
                return unit.get();
            } catch (ConflictException e) {
                if (attempt >= MAX_ATTEMPTS) {
                    throw conflict(e);
                }
                log.atDebug().addKeyValue("attempt", attempt).addKeyValue("error", e.getMessage())
                        .log("unit of work lost a race; retrying");
            }
        }
    }

    private static AppException conflict(ConflictException e) {
        return AppException.of(ErrorReason.ERROR_REASON_CONFLICT,
                "the transaction changed concurrently; retry the request with the same request_id").withCause(e);
    }

    /**
     * The recorded response of a retry. The request_id must have been used for
     * the same operation, transaction (when known) and payload.
     */
    private static <R extends Message> R replay(ProcessedRequest done, String operation, UUID transactionId,
            byte[] hash, Parser<R> parser) {
        boolean same = done.operation().equals(operation)
                && (transactionId == null || done.transactionId().equals(transactionId))
                && Arrays.equals(done.requestHash(), hash);
        if (!same) {
            throw AppException.invalidField("request_id",
                    "request_id was already used for a different request; use a new request_id",
                    "request_id.reused");
        }
        try {
            return parser.parseFrom(done.response());
        } catch (InvalidProtocolBufferException e) {
            throw new IllegalStateException("stored response of request " + done.requestId() + " is corrupt", e);
        }
    }

    /**
     * SHA-256 of the request without its request_id: the payload fingerprint the
     * idempotency record compares. The message type is part of it.
     */
    static byte[] requestHash(Message request) {
        Message.Builder b = request.toBuilder();
        b.clearField(request.getDescriptorForType().findFieldByName("request_id"));
        try {
            MessageDigest sha = MessageDigest.getInstance("SHA-256");
            sha.update(request.getDescriptorForType().getFullName().getBytes(StandardCharsets.UTF_8));
            sha.update((byte) 0);
            sha.update(b.build().toByteArray());
            return sha.digest();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 is not available", e);
        }
    }

    private static UUID requireId(String raw, String field) {
        return Uuids.parseCanonical(raw)
                .orElseThrow(() -> AppException.invalidField(field, "value must be a valid UUID", "string.uuid"));
    }

    private static AppException notFound(UUID id) {
        return AppException.of(ErrorReason.ERROR_REASON_NOT_FOUND, "transaction not found",
                "transaction_id", id.toString());
    }

    private static String trim(String name, String prefix) {
        return name.startsWith(prefix) ? name.substring(prefix.length()) : name;
    }
}
