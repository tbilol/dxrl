package uz.agrogram.orderescrow.catalog;

import static org.assertj.core.api.Assertions.assertThat;

import io.grpc.ManagedChannel;
import io.grpc.Metadata;
import io.grpc.Server;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.ServerInterceptors;
import io.grpc.Status;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import java.time.Duration;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.BiConsumer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;
import uz.agrogram.catalog.v1.CatalogServiceGrpc;
import uz.agrogram.catalog.v1.GetListingRequest;
import uz.agrogram.catalog.v1.GetListingResponse;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.testing.Fixtures;

/** CatalogClient against an in-process fake catalog: metadata, timeouts and every failure mode. */
class CatalogClientTest {

    private final UUID userId = Fixtures.id();
    private final Principal principal = Fixtures.farmer(userId);
    private final Listing listing = Fixtures.offer(Fixtures.id()).build();
    private final AtomicReference<Metadata> received = new AtomicReference<>();
    private Server server;
    private ManagedChannel channel;

    @AfterEach
    void tearDown() {
        if (channel != null) {
            channel.shutdownNow();
        }
        if (server != null) {
            server.shutdownNow();
        }
    }

    /** A catalog whose GetListing runs {@code behaviour}. */
    private CatalogClient catalog(BiConsumer<GetListingRequest, StreamObserver<GetListingResponse>> behaviour,
            Duration timeout) throws Exception {
        String name = InProcessServerBuilder.generateName();
        CatalogServiceGrpc.CatalogServiceImplBase impl = new CatalogServiceGrpc.CatalogServiceImplBase() {
            @Override
            public void getListing(GetListingRequest request, StreamObserver<GetListingResponse> observer) {
                behaviour.accept(request, observer);
            }
        };
        ServerInterceptor capture = new ServerInterceptor() {
            @Override
            public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(ServerCall<ReqT, RespT> call,
                    Metadata headers, ServerCallHandler<ReqT, RespT> next) {
                received.set(headers);
                return next.startCall(call, headers);
            }
        };
        server = InProcessServerBuilder.forName(name).addService(ServerInterceptors.intercept(impl, capture))
                .build().start();
        channel = InProcessChannelBuilder.forName(name).build();
        return CatalogClient.of(CatalogServiceGrpc.newBlockingStub(channel), timeout);
    }

    private static AppException catchApp(Runnable r) {
        try {
            r.run();
        } catch (AppException e) {
            return e;
        }
        throw new AssertionError("expected an AppException");
    }

    private static String header(Metadata md, String name) {
        return md.get(Metadata.Key.of(name, Metadata.ASCII_STRING_MARSHALLER));
    }

    @Test
    void getsTheListingOnTheUsersBehalfAsOrderEscrow() throws Exception {
        CatalogClient client = catalog((req, obs) -> {
            obs.onNext(GetListingResponse.newBuilder().setListing(listing).build());
            obs.onCompleted();
        }, Duration.ofSeconds(2));

        Listing got = client.getListing(principal, UUID.fromString(listing.getId()));

        assertThat(got).isEqualTo(listing);
        Metadata md = received.get();
        assertThat(header(md, "x-agrogram-caller")).isEqualTo("order-escrow");
        assertThat(header(md, "x-request-id")).isEqualTo(principal.requestId());
        assertThat(header(md, "x-agrogram-user-id")).isEqualTo(userId.toString());
        assertThat(header(md, "x-agrogram-roles")).isEqualTo("RESELLER,FARMER");
        assertThat(header(md, "x-agrogram-premium")).isEqualTo("false");
        assertThat(header(md, "x-agrogram-session-id")).isEqualTo(principal.sessionId());
        assertThat(header(md, "traceparent")).isEqualTo(Fixtures.TRACEPARENT);
        assertThat(header(md, "tracestate")).isEqualTo("vendor=1");
        assertThat(header(md, "accept-language")).isEqualTo("uz");
    }

    @Test
    void notFoundIsListingNotFound() throws Exception {
        CatalogClient client = catalog((req, obs) -> obs.onError(Status.NOT_FOUND.asRuntimeException()),
                Duration.ofSeconds(2));
        AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_FOUND);
        assertThat(e.code()).isEqualTo(Status.Code.NOT_FOUND);
        assertThat(e.metadata()).containsEntry("listing_id", listing.getId());
    }

    @Test
    void unavailableIsDependencyUnavailable() throws Exception {
        CatalogClient client = catalog((req, obs) -> obs.onError(Status.UNAVAILABLE.asRuntimeException()),
                Duration.ofSeconds(2));
        AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
        assertThat(e.code()).isEqualTo(Status.Code.UNAVAILABLE);
        assertThat(e.metadata()).containsEntry("dependency", "catalog");
    }

    @Test
    void aSlowCatalogTimesOutAsDependencyUnavailable() throws Exception {
        CatalogClient client = catalog((req, obs) -> {
            try {
                Thread.sleep(2_000);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }, Duration.ofMillis(100));
        long start = System.nanoTime();
        AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
        assertThat(Duration.ofNanos(System.nanoTime() - start)).isLessThan(Duration.ofSeconds(2));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
    }

    @Test
    void anUnimplementedCatalogIsDependencyUnavailable() throws Exception {
        String name = InProcessServerBuilder.generateName();
        server = InProcessServerBuilder.forName(name).build().start(); // no CatalogService registered
        channel = InProcessChannelBuilder.forName(name).build();
        CatalogClient client = CatalogClient.of(CatalogServiceGrpc.newBlockingStub(channel), Duration.ofSeconds(2));
        AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
    }

    @Test
    void catalogsOwnErrorsKeepTheirCode() throws Exception {
        CatalogClient client = catalog((req, obs) -> obs.onError(
                Status.PERMISSION_DENIED.withDescription("no").asRuntimeException()), Duration.ofSeconds(2));
        AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
        assertThat(e.code()).isEqualTo(Status.Code.PERMISSION_DENIED);
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_PERMISSION_DENIED);
    }

    @Test
    void aRefusedConnectionIsDependencyUnavailable() {
        try (CatalogClient client = CatalogClient.connect("127.0.0.1:1", Duration.ofSeconds(2))) {
            AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
            assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
            assertThat(e.code()).isEqualTo(Status.Code.UNAVAILABLE);
        }
    }

    @Test
    void withoutAnAddressEveryCallIsDependencyUnavailable() {
        try (CatalogClient client = CatalogClient.connect("", Duration.ofSeconds(2))) {
            AppException e = catchApp(() -> client.getListing(principal, UUID.fromString(listing.getId())));
            assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
        }
    }
}
