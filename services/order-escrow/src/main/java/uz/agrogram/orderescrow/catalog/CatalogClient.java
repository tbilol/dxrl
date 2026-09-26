package uz.agrogram.orderescrow.catalog;

import io.grpc.ManagedChannel;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.netty.shaded.io.grpc.netty.NettyChannelBuilder;
import io.grpc.stub.MetadataUtils;
import java.time.Duration;
import java.util.Objects;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import uz.agrogram.catalog.v1.CatalogServiceGrpc;
import uz.agrogram.catalog.v1.GetListingRequest;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.error.AppException;

/**
 * order-escrow's client for agrogram.catalog.v1.CatalogService, used for
 * GetListing only: CreateTransaction snapshots the listing.
 *
 * <p>Every call is made on the user's behalf, like profile-social's catalog
 * client: it carries {@code x-agrogram-caller: order-escrow} (catalog then
 * shows listings in any status), the incoming x-request-id, trace context and
 * accept-language, and forwards the user headers unchanged. Each call has its
 * own timeout; the deadline of the incoming call still applies (grpc-java
 * propagates it through io.grpc.Context).
 *
 * <p>When catalog cannot answer (UNAVAILABLE, including connection refused and
 * name resolution failures, DEADLINE_EXCEEDED, UNIMPLEMENTED, or no address
 * configured) the error is DEPENDENCY_UNAVAILABLE (UNAVAILABLE) with metadata
 * {"dependency": "catalog"}. catalog's NOT_FOUND becomes "listing not found";
 * its other errors keep their code.
 *
 * <p>TODO(security): the connection is plaintext inside the Docker network and
 * x-agrogram-caller is trusted as is; service-to-service TLS or a mesh identity
 * belongs here once the platform has one.
 */
public final class CatalogClient implements ListingLookup, AutoCloseable {

    private final ManagedChannel channel;
    private final CatalogServiceGrpc.CatalogServiceBlockingStub stub;
    private final Duration timeout;

    private CatalogClient(ManagedChannel channel, CatalogServiceGrpc.CatalogServiceBlockingStub stub,
            Duration timeout) {
        this.channel = channel;
        this.stub = stub;
        this.timeout = timeout.isNegative() || timeout.isZero() ? Duration.ofSeconds(2) : timeout;
    }

    /**
     * A client for {@code address} (host:port). The connection is lazy, so catalog
     * being down does not stop order-escrow from starting. An empty address
     * gives a client whose calls fail with DEPENDENCY_UNAVAILABLE.
     */
    public static CatalogClient connect(String address, Duration timeout) {
        if (address == null || address.isBlank()) {
            return new CatalogClient(null, null, timeout);
        }
        ManagedChannel channel = NettyChannelBuilder.forTarget(address).usePlaintext().build();
        return new CatalogClient(channel, CatalogServiceGrpc.newBlockingStub(channel), timeout);
    }

    /** Wraps an existing stub (tests, or a shared channel). */
    public static CatalogClient of(CatalogServiceGrpc.CatalogServiceBlockingStub stub, Duration timeout) {
        return new CatalogClient(null, Objects.requireNonNull(stub), timeout);
    }

    @Override
    public Listing getListing(Principal principal, UUID listingId) {
        if (stub == null) {
            throw unavailable("catalog address is not configured (ORDER_ESCROW_CATALOG_ADDR)", null);
        }
        try {
            return stub
                    .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(principal.outgoingMetadata()))
                    .withDeadlineAfter(timeout.toNanos(), TimeUnit.NANOSECONDS)
                    .getListing(GetListingRequest.newBuilder().setListingId(listingId.toString()).build())
                    .getListing();
        } catch (StatusRuntimeException e) {
            throw wrap(listingId, e);
        } catch (RuntimeException e) {
            throw unavailable("catalog is unavailable", e);
        }
    }

    private static AppException wrap(UUID listingId, StatusRuntimeException e) {
        Status.Code code = e.getStatus().getCode();
        switch (code) {
            case UNAVAILABLE:
            case DEADLINE_EXCEEDED:
            case UNIMPLEMENTED:
                return unavailable("catalog is unavailable", e);
            case NOT_FOUND:
                return AppException.of(ErrorReason.ERROR_REASON_NOT_FOUND, "listing not found",
                        "listing_id", listingId.toString()).withCause(e);
            default:
                return AppException.from(e);
        }
    }

    private static AppException unavailable(String message, Throwable cause) {
        return AppException.of(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE, message, "dependency", "catalog")
                .withCause(cause);
    }

    @Override
    public void close() {
        if (channel != null) {
            channel.shutdown();
            try {
                if (!channel.awaitTermination(5, TimeUnit.SECONDS)) {
                    channel.shutdownNow();
                }
            } catch (InterruptedException e) {
                channel.shutdownNow();
                Thread.currentThread().interrupt();
            }
        }
    }
}
