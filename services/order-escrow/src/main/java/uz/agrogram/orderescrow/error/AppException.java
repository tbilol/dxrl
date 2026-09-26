package uz.agrogram.orderescrow.error;

import com.google.protobuf.Any;
import io.grpc.Status;
import io.grpc.StatusException;
import io.grpc.StatusRuntimeException;
import io.grpc.protobuf.StatusProto;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import uz.agrogram.common.v1.ErrorReason;
import uz.agrogram.common.v1.FieldViolation;

/**
 * order-escrow's error model. Every failure that leaves the service is a gRPC
 * status whose details hold exactly one {@code agrogram.common.v1.Error}; the
 * status code follows the ErrorReason table ({@link ErrorCodes}). The cause is
 * for logs only and never leaves the service.
 *
 * <p>Mirrors the Go services' apperr package (services/catalog/internal/apperr).
 */
public final class AppException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    /** Error.domain of every error order-escrow produces. */
    public static final String DOMAIN = "order-escrow";

    private final transient Status.Code code;
    private final transient ErrorReason reason;
    private final transient Map<String, String> metadata;
    private final transient List<FieldViolation> violations;

    private AppException(Status.Code code, ErrorReason reason, String message, Map<String, String> metadata,
            List<FieldViolation> violations, Throwable cause) {
        super(message, cause);
        this.code = code;
        this.reason = reason;
        this.metadata = Collections.unmodifiableMap(new LinkedHashMap<>(metadata));
        this.violations = List.copyOf(violations);
    }

    /**
     * An error for {@code reason} with the code the ErrorReason table assigns.
     * {@code kv} are metadata key/value pairs, e.g. {@code "state", "PAYMENT_HELD"}.
     */
    public static AppException of(ErrorReason reason, String message, String... kv) {
        Map<String, String> md = new LinkedHashMap<>();
        for (int i = 0; i + 1 < kv.length; i += 2) {
            md.put(kv[i], kv[i + 1]);
        }
        return new AppException(ErrorCodes.codeOf(reason), reason, message, md, List.of(), null);
    }

    /** INVALID_ARGUMENT / VALIDATION_FAILED with the given field violations. */
    public static AppException validation(List<FieldViolation> violations) {
        return new AppException(Status.Code.INVALID_ARGUMENT, ErrorReason.ERROR_REASON_VALIDATION_FAILED,
                "request failed validation", Map.of(), violations, null);
    }

    /** VALIDATION_FAILED for a single field. */
    public static AppException invalidField(String field, String description, String rule) {
        return validation(List.of(FieldViolation.newBuilder()
                .setField(field)
                .setDescription(description)
                .setRule(rule)
                .build()));
    }

    /** Wraps an unexpected failure. The client sees a generic message. */
    public static AppException internal(Throwable cause) {
        return new AppException(Status.Code.INTERNAL, ErrorReason.ERROR_REASON_INTERNAL, "internal error",
                Map.of(), List.of(), cause);
    }

    /** INTERNAL with a developer message (still generic enough to show a client). */
    public static AppException internal(String message, Throwable cause) {
        return new AppException(Status.Code.INTERNAL, ErrorReason.ERROR_REASON_INTERNAL, message,
                Map.of(), List.of(), cause);
    }

    /** Returns a copy of this error that keeps {@code cause} for the logs. */
    public AppException withCause(Throwable cause) {
        return new AppException(code, reason, getMessage(), metadata, violations, cause);
    }

    /**
     * Converts any failure into an AppException: an AppException in the cause
     * chain is returned as is, cancellation and deadlines keep their gRPC codes,
     * other gRPC statuses keep code and message, and anything else becomes
     * INTERNAL (the cause stays in the logs).
     */
    public static AppException from(Throwable t) {
        for (Throwable c = t; c != null; c = c.getCause()) {
            if (c instanceof AppException ae) {
                return ae;
            }
        }
        Status status = null;
        if (t instanceof StatusRuntimeException sre) {
            status = sre.getStatus();
        } else if (t instanceof StatusException se) {
            status = se.getStatus();
        }
        if (status != null) {
            switch (status.getCode()) {
                case CANCELLED:
                    return new AppException(Status.Code.CANCELLED, ErrorReason.ERROR_REASON_UNSPECIFIED,
                            "request canceled", Map.of(), List.of(), t);
                case DEADLINE_EXCEEDED:
                    return new AppException(Status.Code.DEADLINE_EXCEEDED, ErrorReason.ERROR_REASON_UNSPECIFIED,
                            "deadline exceeded", Map.of(), List.of(), t);
                case OK:
                case UNKNOWN:
                    break;
                default:
                    String msg = status.getDescription() == null ? status.getCode().name() : status.getDescription();
                    return new AppException(status.getCode(), ErrorCodes.reasonForCode(status.getCode()), msg,
                            Map.of(), List.of(), t);
            }
        }
        return internal(t);
    }

    public Status.Code code() {
        return code;
    }

    public ErrorReason reason() {
        return reason;
    }

    public Map<String, String> metadata() {
        return metadata;
    }

    public List<FieldViolation> violations() {
        return violations;
    }

    /** The agrogram.common.v1.Error carried in the status details. */
    public uz.agrogram.common.v1.Error toProto() {
        return uz.agrogram.common.v1.Error.newBuilder()
                .setReason(reason)
                .setMessage(getMessage() == null ? "" : getMessage())
                .setDomain(DOMAIN)
                .putAllMetadata(metadata)
                .addAllFieldViolations(violations)
                .build();
    }

    /** The google.rpc.Status: code, message and exactly one packed Error detail. */
    public com.google.rpc.Status toRpcStatus() {
        return com.google.rpc.Status.newBuilder()
                .setCode(code.value())
                .setMessage(getMessage() == null ? "" : getMessage())
                .addDetails(Any.pack(toProto()))
                .build();
    }

    /** The exception a gRPC handler returns (grpc-status-details-bin carries the Error). */
    public StatusRuntimeException toStatusException() {
        return StatusProto.toStatusRuntimeException(toRpcStatus());
    }

    @Override
    public String toString() {
        StringBuilder sb = new StringBuilder()
                .append(reason.name()).append(" (").append(code.name()).append("): ").append(getMessage());
        if (getCause() != null) {
            sb.append(": ").append(getCause());
        }
        return sb.toString();
    }
}
