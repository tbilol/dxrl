package uz.agrogram.orderescrow.error;

import static org.assertj.core.api.Assertions.assertThat;

import io.grpc.Status;
import java.util.Map;
import org.junit.jupiter.api.Test;
import uz.agrogram.common.v1.ErrorReason;

/** The ErrorReason -> gRPC code table and the conversions of the Go services' apperr. */
class ErrorModelTest {

    /** Status codes as the comments in proto/common/v1/error.proto assign them. */
    private static final Map<ErrorReason, Status.Code> TABLE = Map.ofEntries(
            Map.entry(ErrorReason.ERROR_REASON_VALIDATION_FAILED, Status.Code.INVALID_ARGUMENT),
            Map.entry(ErrorReason.ERROR_REASON_NOT_FOUND, Status.Code.NOT_FOUND),
            Map.entry(ErrorReason.ERROR_REASON_ALREADY_EXISTS, Status.Code.ALREADY_EXISTS),
            Map.entry(ErrorReason.ERROR_REASON_CONFLICT, Status.Code.ABORTED),
            Map.entry(ErrorReason.ERROR_REASON_UNAUTHENTICATED, Status.Code.UNAUTHENTICATED),
            Map.entry(ErrorReason.ERROR_REASON_PERMISSION_DENIED, Status.Code.PERMISSION_DENIED),
            Map.entry(ErrorReason.ERROR_REASON_RATE_LIMITED, Status.Code.RESOURCE_EXHAUSTED),
            Map.entry(ErrorReason.ERROR_REASON_NOT_IMPLEMENTED, Status.Code.UNIMPLEMENTED),
            Map.entry(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE, Status.Code.UNAVAILABLE),
            Map.entry(ErrorReason.ERROR_REASON_INTERNAL, Status.Code.INTERNAL),
            Map.entry(ErrorReason.ERROR_REASON_FAILED_PRECONDITION, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_INVALID_CREDENTIALS, Status.Code.UNAUTHENTICATED),
            Map.entry(ErrorReason.ERROR_REASON_TOKEN_EXPIRED, Status.Code.UNAUTHENTICATED),
            Map.entry(ErrorReason.ERROR_REASON_TOKEN_INVALID, Status.Code.UNAUTHENTICATED),
            Map.entry(ErrorReason.ERROR_REASON_TOKEN_REVOKED, Status.Code.UNAUTHENTICATED),
            Map.entry(ErrorReason.ERROR_REASON_PHONE_ALREADY_REGISTERED, Status.Code.ALREADY_EXISTS),
            Map.entry(ErrorReason.ERROR_REASON_ACCOUNT_DISABLED, Status.Code.PERMISSION_DENIED),
            Map.entry(ErrorReason.ERROR_REASON_PROVIDER_ALREADY_LINKED, Status.Code.ALREADY_EXISTS),
            Map.entry(ErrorReason.ERROR_REASON_LAST_SIGN_IN_METHOD, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_OTP_INVALID, Status.Code.INVALID_ARGUMENT),
            Map.entry(ErrorReason.ERROR_REASON_PREMIUM_REQUIRED, Status.Code.PERMISSION_DENIED),
            Map.entry(ErrorReason.ERROR_REASON_PLAN_LIMIT_REACHED, Status.Code.RESOURCE_EXHAUSTED),
            Map.entry(ErrorReason.ERROR_REASON_FARMER_VERIFICATION_REQUIRED, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_LEASE_CONTRACT_INVALID, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED, Status.Code.ALREADY_EXISTS),
            Map.entry(ErrorReason.ERROR_REASON_LISTING_NOT_ACTIVE, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY, Status.Code.PERMISSION_DENIED),
            Map.entry(ErrorReason.ERROR_REASON_PAYMENT_FAILED, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_RECEIPT_REJECTED, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_SELF_TRADE_NOT_ALLOWED, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_SELF_ACTION_NOT_ALLOWED, Status.Code.INVALID_ARGUMENT),
            Map.entry(ErrorReason.ERROR_REASON_RATING_NOT_ALLOWED, Status.Code.FAILED_PRECONDITION),
            Map.entry(ErrorReason.ERROR_REASON_ALREADY_RATED, Status.Code.ALREADY_EXISTS),
            Map.entry(ErrorReason.ERROR_REASON_UNSUPPORTED_MEDIA_TYPE, Status.Code.INVALID_ARGUMENT),
            Map.entry(ErrorReason.ERROR_REASON_MEDIA_TOO_LARGE, Status.Code.INVALID_ARGUMENT),
            Map.entry(ErrorReason.ERROR_REASON_MEDIA_PROCESSING_FAILED, Status.Code.FAILED_PRECONDITION));

    @Test
    void everyDefinedReasonHasItsCode() {
        for (ErrorReason r : ErrorReason.values()) {
            if (r == ErrorReason.UNRECOGNIZED || r == ErrorReason.ERROR_REASON_UNSPECIFIED) {
                assertThat(ErrorCodes.codeOf(r)).isEqualTo(Status.Code.UNKNOWN);
                continue;
            }
            assertThat(TABLE).as("error.proto gained %s; add it to the table", r).containsKey(r);
            assertThat(ErrorCodes.codeOf(r)).as(r.name()).isEqualTo(TABLE.get(r));
        }
    }

    @Test
    void foreignStatusesKeepTheirCodeAndGetTheGenericReason() {
        AppException e = AppException.from(Status.UNIMPLEMENTED.withDescription("nope").asRuntimeException());
        assertThat(e.code()).isEqualTo(Status.Code.UNIMPLEMENTED);
        assertThat(e.reason()).isEqualTo(ErrorReason.ERROR_REASON_NOT_IMPLEMENTED);
        assertThat(e.getMessage()).isEqualTo("nope");
        assertThat(AppException.from(Status.CANCELLED.asRuntimeException()).code()).isEqualTo(Status.Code.CANCELLED);
        assertThat(AppException.from(Status.DEADLINE_EXCEEDED.asRuntimeException()).reason())
                .isEqualTo(ErrorReason.ERROR_REASON_UNSPECIFIED);
        assertThat(AppException.from(Status.UNKNOWN.asRuntimeException()).code()).isEqualTo(Status.Code.INTERNAL);
    }

    @Test
    void anythingElseIsInternalAndKeepsTheCauseForLogsOnly() {
        RuntimeException boom = new RuntimeException("db password in a message");
        AppException e = AppException.from(boom);
        assertThat(e.code()).isEqualTo(Status.Code.INTERNAL);
        assertThat(e.getMessage()).isEqualTo("internal error");
        assertThat(e.getCause()).isSameAs(boom);
        assertThat(e.toProto().getMessage()).isEqualTo("internal error");
        AppException inner = AppException.of(ErrorReason.ERROR_REASON_NOT_FOUND, "x");
        assertThat(AppException.from(new IllegalStateException(inner))).isSameAs(inner);
    }
}
