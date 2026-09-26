package uz.agrogram.orderescrow.testing;

import build.buf.protovalidate.Validator;
import build.buf.protovalidate.ValidatorFactory;
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.Address;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.ListingKind;
import uz.agrogram.common.v1.Mass;
import uz.agrogram.common.v1.MassUnit;
import uz.agrogram.common.v1.Money;
import uz.agrogram.common.v1.ProductType;
import uz.agrogram.common.v1.Region;
import uz.agrogram.common.v1.Role;
import uz.agrogram.common.v1.UnitPrice;
import uz.agrogram.events.v1.ListingStatus;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.auth.Uuids;
import uz.agrogram.orderescrow.catalog.ListingLookup;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.events.TransactionEvents;
import uz.agrogram.orderescrow.service.EscrowService;

/** Shared test data and wiring. */
public final class Fixtures {

    public static final Validator VALIDATOR = ValidatorFactory.newBuilder().build();
    public static final String TRACEPARENT = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";

    private Fixtures() {}

    /** A clock that only moves when told to. */
    public static final class MutableClock extends Clock {
        private volatile Instant now;

        public MutableClock(Instant start) {
            this.now = start;
        }

        public void advance(Duration d) {
            now = now.plus(d);
        }

        @Override
        public ZoneId getZone() {
            return ZoneOffset.UTC;
        }

        @Override
        public Clock withZone(ZoneId zone) {
            return this;
        }

        @Override
        public Instant instant() {
            return now;
        }
    }

    /** catalog: listings by id; counts calls; can be made to fail. */
    public static final class FakeCatalog implements ListingLookup {
        private final Map<UUID, Listing> listings = new ConcurrentHashMap<>();
        private final AtomicInteger calls = new AtomicInteger();
        private volatile AppException failure;

        public FakeCatalog put(Listing l) {
            listings.put(UUID.fromString(l.getId()), l);
            return this;
        }

        public void failWith(AppException e) {
            failure = e;
        }

        public int calls() {
            return calls.get();
        }

        @Override
        public Listing getListing(Principal principal, UUID listingId) {
            calls.incrementAndGet();
            if (failure != null) {
                throw failure;
            }
            Listing l = listings.get(listingId);
            if (l == null) {
                throw AppException.of(ErrorReason.ERROR_REASON_NOT_FOUND, "listing not found",
                        "listing_id", listingId.toString());
            }
            return l;
        }
    }

    /** The service over an in-memory store, a fake catalog and a fixed clock, with sequential ids. */
    public static final class Harness {
        public final MutableClock clock = new MutableClock(Instant.parse("2026-09-01T08:00:00.123456789Z"));
        public final InMemoryEscrowStore store = new InMemoryEscrowStore(clock);
        public final FakeCatalog catalog = new FakeCatalog();
        public final TransactionEvents events = new TransactionEvents(VALIDATOR, Uuids::newV7);
        public final EscrowService service;

        public Harness() {
            this(1);
        }

        public Harness(long bonusPoints) {
            service = new EscrowService(store, catalog, events, clock, uz.agrogram.orderescrow.domain.TaxPolicy.NONE,
                    Uuids::newV7, bonusPoints);
        }
    }

    public static UUID id() {
        return Uuids.newV7();
    }

    public static String rid() {
        return Uuids.newV7().toString();
    }

    /** A signed-in user with roles, a request id and trace context. */
    public static Principal user(UUID id, Role... roles) {
        return new Principal(id, List.of(roles), false, id().toString(), "", "req-" + rid(), TRACEPARENT,
                "vendor=1", "uz");
    }

    public static Principal reseller(UUID id) {
        return user(id, Role.ROLE_RESELLER);
    }

    public static Principal farmer(UUID id) {
        return user(id, Role.ROLE_RESELLER, Role.ROLE_FARMER);
    }

    public static Principal staff(UUID id) {
        return user(id, Role.ROLE_RESELLER, Role.ROLE_ADMIN);
    }

    public static Address address() {
        return Address.newBuilder()
                .setRegion(Region.REGION_SAMARQAND)
                .setDistrict("Urgut tumani")
                .setAddressLine("Qo'rg'oncha qishlog'i")
                .build();
    }

    public static UnitPrice pricePerKg(long minor) {
        return UnitPrice.newBuilder()
                .setAmount(Money.newBuilder().setAmountMinor(minor).setCurrencyCode("UZS"))
                .setUnit(MassUnit.MASS_UNIT_KILOGRAM)
                .build();
    }

    public static Mass tonnes(double t) {
        return Mass.newBuilder().setGrams(Math.round(t * 1_000_000)).setDisplayUnit(MassUnit.MASS_UNIT_TONNE).build();
    }

    /** An ACTIVE farmer's offer: 5 t of tomatoes at 6 000 so'm per kg. */
    public static Listing.Builder offer(UUID farmerId) {
        return Listing.newBuilder()
                .setId(id().toString())
                .setKind(ListingKind.LISTING_KIND_OFFER)
                .setOwnerId(farmerId.toString())
                .setProduct(ProductType.PRODUCT_TYPE_POMIDOR)
                .setQuantity(tonnes(5))
                .setRemainingQuantity(tonnes(5))
                .setPrice(pricePerKg(600_000))
                .setPhone("+998901234567")
                .setAddress(address())
                .setStatus(ListingStatus.LISTING_STATUS_ACTIVE)
                .setVersion(3);
    }

    /** An ACTIVE reseller's buy-request: 2 t of potatoes, negotiable price. */
    public static Listing.Builder buyRequest(UUID resellerId) {
        return Listing.newBuilder()
                .setId(id().toString())
                .setKind(ListingKind.LISTING_KIND_BUY_REQUEST)
                .setOwnerId(resellerId.toString())
                .setProduct(ProductType.PRODUCT_TYPE_KARTOSHKA)
                .setQuantity(tonnes(2))
                .setRemainingQuantity(tonnes(2))
                .setPhone("+998901234568")
                .setAddress(address())
                .setStatus(ListingStatus.LISTING_STATUS_ACTIVE)
                .setVersion(1);
    }
}
