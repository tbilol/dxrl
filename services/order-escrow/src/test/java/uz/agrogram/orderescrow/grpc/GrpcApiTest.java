package uz.agrogram.orderescrow.grpc;

import static org.assertj.core.api.Assertions.assertThat;
import static uz.agrogram.orderescrow.testing.Fixtures.rid;
import static uz.agrogram.orderescrow.testing.Fixtures.tonnes;

import io.grpc.BindableService;
import io.grpc.ManagedChannel;
import io.grpc.Metadata;
import io.grpc.Server;
import io.grpc.Status;
import io.grpc.StatusRuntimeException;
import io.grpc.health.v1.HealthCheckRequest;
import io.grpc.health.v1.HealthCheckResponse.ServingStatus;
import io.grpc.health.v1.HealthGrpc;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.protobuf.StatusProto;
import io.grpc.protobuf.services.HealthStatusManager;
import io.grpc.stub.MetadataUtils;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import uz.agrogram.catalog.v1.Listing;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.FieldViolation;
import uz.agrogram.events.v1.TransactionState;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.testing.Fixtures;
import uz.agrogram.orderescrow.testing.Fixtures.Harness;
import uz.agrogram.orderescrow.v1.ConfirmHandoverRequest;
import uz.agrogram.orderescrow.v1.ConfirmReceiptRequest;
import uz.agrogram.orderescrow.v1.CreateTransactionRequest;
import uz.agrogram.orderescrow.v1.GetTransactionRequest;
import uz.agrogram.orderescrow.v1.ListTransactionsRequest;
import uz.agrogram.orderescrow.v1.OrderEscrowServiceGrpc;
import uz.agrogram.orderescrow.v1.OrderEscrowServiceGrpc.OrderEscrowServiceBlockingStub;
import uz.agrogram.orderescrow.v1.PayInRequest;
import uz.agrogram.orderescrow.v1.SubmitWeighbridgeReceiptRequest;
import uz.agrogram.orderescrow.v1.Transaction;

/**
 * The gRPC surface with the production interceptor chain (GrpcServer.configure)
 * on an in-process server: x-agrogram-* metadata, validation, the error model
 * (status + exactly one common.v1.Error detail) and x-request-id echoing.
 */
class GrpcApiTest {

    private final Harness h = new Harness();
    private final HealthStatusManager health = new HealthStatusManager();
    private final UUID farmerId = Fixtures.id();
    private final UUID resellerId = Fixtures.id();
    private final Listing offer = Fixtures.offer(farmerId).build();
    private Server server;
    private ManagedChannel channel;

    @BeforeEach
    void setUp() throws Exception {
        h.catalog.put(offer);
        String name = InProcessServerBuilder.generateName();
        List<BindableService> services = List.of(new OrderEscrowGrpcService(h.service), health.getHealthService());
        server = GrpcServer.configure(InProcessServerBuilder.forName(name), services, Fixtures.VALIDATOR)
                .build().start();
        channel = InProcessChannelBuilder.forName(name).build();
    }

    @AfterEach
    void tearDown() {
        channel.shutdownNow();
        server.shutdownNow();
    }

    private static Metadata headers(UUID user, String roles, String requestId) {
        Metadata md = new Metadata();
        if (user != null) {
            md.put(key("x-agrogram-user-id"), user.toString());
        }
        if (roles != null) {
            md.put(key("x-agrogram-roles"), roles);
        }
        if (requestId != null) {
            md.put(key("x-request-id"), requestId);
        }
        md.put(key("traceparent"), Fixtures.TRACEPARENT);
        return md;
    }

    private static Metadata.Key<String> key(String name) {
        return Metadata.Key.of(name, Metadata.ASCII_STRING_MARSHALLER);
    }

    private OrderEscrowServiceBlockingStub stub(Metadata md) {
        return OrderEscrowServiceGrpc.newBlockingStub(channel)
                .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(md));
    }

    private OrderEscrowServiceBlockingStub reseller() {
        return stub(headers(resellerId, "RESELLER", null));
    }

    private OrderEscrowServiceBlockingStub farmer() {
        return stub(headers(farmerId, "RESELLER,FARMER", null));
    }

    private static uz.agrogram.common.v1.Error detail(StatusRuntimeException e) throws Exception {
        com.google.rpc.Status st = StatusProto.fromThrowable(e);
        assertThat(st).isNotNull();
        assertThat(st.getDetailsCount()).isEqualTo(1);
        uz.agrogram.common.v1.Error err = st.getDetails(0).unpack(uz.agrogram.common.v1.Error.class);
        assertThat(err.getDomain()).isEqualTo("order-escrow");
        return err;
    }

    private static StatusRuntimeException fails(Runnable call) {
        try {
            call.run();
        } catch (StatusRuntimeException e) {
            return e;
        }
        throw new AssertionError("expected a StatusRuntimeException");
    }

    private Transaction create(OrderEscrowServiceBlockingStub stub) {
        return stub.createTransaction(CreateTransactionRequest.newBuilder().setRequestId(rid())
                .setListingId(offer.getId()).setQuantity(tonnes(1)).build()).getTransaction();
    }

    @Test
    void theWholeLifecycleWorksOverGrpc() {
        Transaction t = create(reseller());
        reseller().payIn(PayInRequest.newBuilder().setRequestId(rid()).setTransactionId(t.getId()).build());
        reseller().submitWeighbridgeReceipt(SubmitWeighbridgeReceiptRequest.newBuilder().setRequestId(rid())
                .setTransactionId(t.getId()).setTicketNumber("TB-1").build());
        farmer().confirmHandover(ConfirmHandoverRequest.newBuilder().setRequestId(rid())
                .setTransactionId(t.getId()).build());
        Transaction done = reseller().confirmReceipt(ConfirmReceiptRequest.newBuilder().setRequestId(rid())
                .setTransactionId(t.getId()).build()).getTransaction();
        assertThat(done.getState()).isEqualTo(TransactionState.TRANSACTION_STATE_COMPLETED);
        assertThat(farmer().getTransaction(GetTransactionRequest.newBuilder().setTransactionId(t.getId()).build())
                .getTransaction().getState()).isEqualTo(TransactionState.TRANSACTION_STATE_COMPLETED);
        assertThat(farmer().listTransactions(ListTransactionsRequest.getDefaultInstance()).getTransactionsCount())
                .isEqualTo(1);
        // The events carry the edge's request id and trace context.
        assertThat(h.store.outbox()).hasSize(7).allSatisfy(m ->
                assertThat(m.headers()).containsEntry("traceparent", Fixtures.TRACEPARENT));
    }

    @Test
    void theRequestIdIsEchoedAndPropagated() {
        AtomicReference<Metadata> responseHeaders = new AtomicReference<>();
        AtomicReference<Metadata> trailers = new AtomicReference<>();
        OrderEscrowServiceBlockingStub stub = stub(headers(resellerId, "RESELLER", "edge-req-42"))
                .withInterceptors(MetadataUtils.newCaptureMetadataInterceptor(responseHeaders, trailers));
        create(stub);
        assertThat(responseHeaders.get().get(key("x-request-id"))).isEqualTo("edge-req-42");
        assertThat(h.store.outbox().get(0).headers()).containsEntry("x-request-id", "edge-req-42");

        // Without one, a fresh id is generated and returned (in the trailers of an error, too).
        OrderEscrowServiceBlockingStub anonymous = stub(headers(null, null, null))
                .withInterceptors(MetadataUtils.newCaptureMetadataInterceptor(responseHeaders, trailers));
        fails(() -> anonymous.getTransaction(GetTransactionRequest.newBuilder()
                .setTransactionId(Fixtures.rid()).build()));
        assertThat(trailers.get().get(key("x-request-id"))).isNotBlank();
    }

    @Test
    void anonymousCallsAreUnauthenticated() throws Exception {
        StatusRuntimeException e = fails(() -> stub(headers(null, null, null)).listTransactions(
                ListTransactionsRequest.getDefaultInstance()));
        assertThat(e.getStatus().getCode()).isEqualTo(Status.Code.UNAUTHENTICATED);
        assertThat(detail(e).getReason()).isEqualTo(ErrorReason.ERROR_REASON_UNAUTHENTICATED);
    }

    @Test
    void aMalformedUserIdIsUnauthenticatedBeforeAnythingRuns() throws Exception {
        Metadata md = headers(null, "RESELLER", null);
        md.put(key("x-agrogram-user-id"), "not-a-uuid");
        StatusRuntimeException e = fails(() -> stub(md).createTransaction(CreateTransactionRequest.newBuilder()
                .setRequestId(rid()).setListingId(offer.getId()).build()));
        assertThat(e.getStatus().getCode()).isEqualTo(Status.Code.UNAUTHENTICATED);
        assertThat(detail(e).getMessage()).contains("x-agrogram-user-id");
        assertThat(h.catalog.calls()).isZero();
    }

    @Test
    void invalidRequestsGetFieldViolationsAndNeverReachTheService() throws Exception {
        StatusRuntimeException e = fails(() -> reseller().payIn(PayInRequest.newBuilder()
                .setRequestId("nope").setTransactionId(Fixtures.rid()).setReturnUrl("not a uri").build()));
        assertThat(e.getStatus().getCode()).isEqualTo(Status.Code.INVALID_ARGUMENT);
        uz.agrogram.common.v1.Error err = detail(e);
        assertThat(err.getReason()).isEqualTo(ErrorReason.ERROR_REASON_VALIDATION_FAILED);
        assertThat(err.getFieldViolationsList()).extracting(FieldViolation::getField)
                .containsExactlyInAnyOrder("request_id", "return_url");
        assertThat(err.getFieldViolationsList()).extracting(FieldViolation::getRule)
                .contains("string.uuid", "string.uri");

        StatusRuntimeException receipt = fails(() -> reseller().submitWeighbridgeReceipt(
                SubmitWeighbridgeReceiptRequest.newBuilder().setRequestId(rid()).setTransactionId(rid()).build()));
        assertThat(detail(receipt).getFieldViolationsList()).extracting(FieldViolation::getRule)
                .containsExactly("submit_weighbridge_receipt.ticket_or_photo");

        StatusRuntimeException negativeQuantity = fails(() -> reseller().createTransaction(
                CreateTransactionRequest.newBuilder().setRequestId(rid()).setListingId(offer.getId())
                        .setQuantity(uz.agrogram.common.v1.Mass.newBuilder().setGrams(0)).build()));
        assertThat(detail(negativeQuantity).getFieldViolationsList()).extracting(FieldViolation::getRule)
                .contains("create_transaction.quantity_positive");
        assertThat(h.catalog.calls()).isZero();
        assertThat(h.store.unitsOfWork()).isZero();
    }

    @Test
    void domainErrorsTravelWithTheirReasonAndMetadata() throws Exception {
        Transaction t = create(reseller());
        StatusRuntimeException wrongParty = fails(() -> farmer().payIn(PayInRequest.newBuilder()
                .setRequestId(rid()).setTransactionId(t.getId()).build()));
        assertThat(wrongParty.getStatus().getCode()).isEqualTo(Status.Code.PERMISSION_DENIED);
        assertThat(detail(wrongParty).getReason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY);

        StatusRuntimeException wrongState = fails(() -> farmer().confirmHandover(ConfirmHandoverRequest.newBuilder()
                .setRequestId(rid()).setTransactionId(t.getId()).build()));
        uz.agrogram.common.v1.Error err = detail(wrongState);
        assertThat(wrongState.getStatus().getCode()).isEqualTo(Status.Code.FAILED_PRECONDITION);
        assertThat(err.getReason()).isEqualTo(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION);
        assertThat(err.getMetadataMap()).containsEntry("state", "AWAITING_PAYMENT")
                .containsEntry("command", "ConfirmHandover");

        StatusRuntimeException stranger = fails(() -> stub(headers(Fixtures.id(), "RESELLER", null))
                .getTransaction(GetTransactionRequest.newBuilder().setTransactionId(t.getId()).build()));
        assertThat(stranger.getStatus().getCode()).isEqualTo(Status.Code.NOT_FOUND);
    }

    @Test
    void unexpectedFailuresAreInternalWithoutDetailsLeaking() throws Exception {
        Harness broken = new Harness();
        broken.catalog.put(offer);
        String name = InProcessServerBuilder.generateName();
        Server s = GrpcServer.configure(InProcessServerBuilder.forName(name), List.of(new OrderEscrowGrpcService(
                new uz.agrogram.orderescrow.service.EscrowService(broken.store, (p, id) -> {
                    throw new IllegalStateException("secret stack detail");
                }, broken.events, broken.clock, 1))), Fixtures.VALIDATOR).build().start();
        ManagedChannel c = InProcessChannelBuilder.forName(name).build();
        try {
            StatusRuntimeException e = fails(() -> OrderEscrowServiceGrpc.newBlockingStub(c)
                    .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(headers(resellerId, "RESELLER", null)))
                    .createTransaction(CreateTransactionRequest.newBuilder().setRequestId(rid())
                            .setListingId(offer.getId()).build()));
            assertThat(e.getStatus().getCode()).isEqualTo(Status.Code.INTERNAL);
            assertThat(e.getStatus().getDescription()).isEqualTo("internal error");
            assertThat(detail(e).getReason()).isEqualTo(ErrorReason.ERROR_REASON_INTERNAL);
            assertThat(detail(e).getMessage()).doesNotContain("secret");
        } finally {
            c.shutdownNow();
            s.shutdownNow();
        }
    }

    @Test
    void theHealthServiceReportsReadiness() {
        HealthGrpc.HealthBlockingStub healthStub = HealthGrpc.newBlockingStub(channel);
        health.setStatus("", ServingStatus.NOT_SERVING);
        assertThat(healthStub.check(HealthCheckRequest.getDefaultInstance()).getStatus())
                .isEqualTo(ServingStatus.NOT_SERVING);
        health.setStatus(OrderEscrowServiceGrpc.SERVICE_NAME, ServingStatus.SERVING);
        assertThat(healthStub.check(HealthCheckRequest.newBuilder().setService(OrderEscrowServiceGrpc.SERVICE_NAME)
                .build()).getStatus()).isEqualTo(ServingStatus.SERVING);
    }

    @Test
    void errorsConvertToStatusesExactlyLikeTheGoServices() throws Exception {
        AppException e = AppException.of(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY, "too much",
                "available_grams", "0");
        StatusRuntimeException sre = e.toStatusException();
        assertThat(sre.getStatus().getCode()).isEqualTo(Status.Code.FAILED_PRECONDITION);
        assertThat(sre.getStatus().getDescription()).isEqualTo("too much");
        uz.agrogram.common.v1.Error err = detail(sre);
        assertThat(err.getReason()).isEqualTo(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY);
        assertThat(err.getMetadataMap()).containsExactlyEntriesOf(java.util.Map.of("available_grams", "0"));
        assertThat(StatusProto.fromThrowable(sre).getDetails(0).getTypeUrl())
                .isEqualTo("type.googleapis.com/agrogram.common.v1.Error");
    }
}
