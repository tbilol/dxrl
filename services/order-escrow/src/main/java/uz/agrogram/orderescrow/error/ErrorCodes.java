package uz.agrogram.orderescrow.error;

import io.grpc.Status;
import java.util.EnumMap;
import java.util.Map;
import uz.agrogram.common.v1.ErrorReason;

/**
 * The gRPC status code of every {@link ErrorReason}, as the comments on
 * agrogram.common.v1.ErrorReason assign them (proto/common/v1/error.proto). The
 * same table as the Go services' apperr package.
 */
public final class ErrorCodes {

    private static final Map<ErrorReason, Status.Code> CODE_BY_REASON = new EnumMap<>(ErrorReason.class);
    private static final Map<Status.Code, ErrorReason> REASON_BY_CODE = new EnumMap<>(Status.Code.class);

    static {
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_VALIDATION_FAILED, Status.Code.INVALID_ARGUMENT);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_NOT_FOUND, Status.Code.NOT_FOUND);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_ALREADY_EXISTS, Status.Code.ALREADY_EXISTS);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_CONFLICT, Status.Code.ABORTED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_UNAUTHENTICATED, Status.Code.UNAUTHENTICATED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_PERMISSION_DENIED, Status.Code.PERMISSION_DENIED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_RATE_LIMITED, Status.Code.RESOURCE_EXHAUSTED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_NOT_IMPLEMENTED, Status.Code.UNIMPLEMENTED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE, Status.Code.UNAVAILABLE);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_INTERNAL, Status.Code.INTERNAL);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_FAILED_PRECONDITION, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_INVALID_CREDENTIALS, Status.Code.UNAUTHENTICATED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_TOKEN_EXPIRED, Status.Code.UNAUTHENTICATED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_TOKEN_INVALID, Status.Code.UNAUTHENTICATED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_TOKEN_REVOKED, Status.Code.UNAUTHENTICATED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_PHONE_ALREADY_REGISTERED, Status.Code.ALREADY_EXISTS);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_ACCOUNT_DISABLED, Status.Code.PERMISSION_DENIED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_PROVIDER_ALREADY_LINKED, Status.Code.ALREADY_EXISTS);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_LAST_SIGN_IN_METHOD, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_OTP_INVALID, Status.Code.INVALID_ARGUMENT);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_PREMIUM_REQUIRED, Status.Code.PERMISSION_DENIED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_PLAN_LIMIT_REACHED, Status.Code.RESOURCE_EXHAUSTED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_FARMER_VERIFICATION_REQUIRED, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_LEASE_CONTRACT_INVALID, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_LEASE_CONTRACT_ALREADY_CLAIMED, Status.Code.ALREADY_EXISTS);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_LISTING_NOT_ACTIVE, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_INSUFFICIENT_QUANTITY, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_INVALID_STATE_TRANSITION, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_NOT_TRANSACTION_PARTY, Status.Code.PERMISSION_DENIED);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_PAYMENT_FAILED, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_RECEIPT_REJECTED, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_SELF_TRADE_NOT_ALLOWED, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_SELF_ACTION_NOT_ALLOWED, Status.Code.INVALID_ARGUMENT);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_RATING_NOT_ALLOWED, Status.Code.FAILED_PRECONDITION);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_ALREADY_RATED, Status.Code.ALREADY_EXISTS);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_UNSUPPORTED_MEDIA_TYPE, Status.Code.INVALID_ARGUMENT);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_MEDIA_TOO_LARGE, Status.Code.INVALID_ARGUMENT);
        CODE_BY_REASON.put(ErrorReason.ERROR_REASON_MEDIA_PROCESSING_FAILED, Status.Code.FAILED_PRECONDITION);

        // The generic reason for statuses created outside AppException (for
        // example UNIMPLEMENTED from the generated service base class).
        REASON_BY_CODE.put(Status.Code.INVALID_ARGUMENT, ErrorReason.ERROR_REASON_VALIDATION_FAILED);
        REASON_BY_CODE.put(Status.Code.NOT_FOUND, ErrorReason.ERROR_REASON_NOT_FOUND);
        REASON_BY_CODE.put(Status.Code.ALREADY_EXISTS, ErrorReason.ERROR_REASON_ALREADY_EXISTS);
        REASON_BY_CODE.put(Status.Code.ABORTED, ErrorReason.ERROR_REASON_CONFLICT);
        REASON_BY_CODE.put(Status.Code.UNAUTHENTICATED, ErrorReason.ERROR_REASON_UNAUTHENTICATED);
        REASON_BY_CODE.put(Status.Code.PERMISSION_DENIED, ErrorReason.ERROR_REASON_PERMISSION_DENIED);
        REASON_BY_CODE.put(Status.Code.RESOURCE_EXHAUSTED, ErrorReason.ERROR_REASON_RATE_LIMITED);
        REASON_BY_CODE.put(Status.Code.UNIMPLEMENTED, ErrorReason.ERROR_REASON_NOT_IMPLEMENTED);
        REASON_BY_CODE.put(Status.Code.UNAVAILABLE, ErrorReason.ERROR_REASON_DEPENDENCY_UNAVAILABLE);
        REASON_BY_CODE.put(Status.Code.INTERNAL, ErrorReason.ERROR_REASON_INTERNAL);
        REASON_BY_CODE.put(Status.Code.FAILED_PRECONDITION, ErrorReason.ERROR_REASON_FAILED_PRECONDITION);
    }

    private ErrorCodes() {}

    /** The code the table assigns to {@code reason}; UNKNOWN for UNSPECIFIED or values added later. */
    public static Status.Code codeOf(ErrorReason reason) {
        return CODE_BY_REASON.getOrDefault(reason, Status.Code.UNKNOWN);
    }

    /** The generic reason for a status code; UNSPECIFIED when there is none (CANCELLED, ...). */
    public static ErrorReason reasonForCode(Status.Code code) {
        return REASON_BY_CODE.getOrDefault(code, ErrorReason.ERROR_REASON_UNSPECIFIED);
    }
}
