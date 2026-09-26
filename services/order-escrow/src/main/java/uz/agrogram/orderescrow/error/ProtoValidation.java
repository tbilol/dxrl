package uz.agrogram.orderescrow.error;

import build.buf.protovalidate.ValidationResult;
import build.buf.protovalidate.Validator;
import build.buf.protovalidate.Violation;
import build.buf.protovalidate.exceptions.ValidationException;
import build.buf.validate.FieldPath;
import build.buf.validate.FieldPathElement;
import com.google.protobuf.Message;
import java.util.ArrayList;
import java.util.List;
import java.util.Optional;
import uz.agrogram.common.v1.FieldViolation;

/** Runs protovalidate and turns violations into VALIDATION_FAILED errors, like the Go services. */
public final class ProtoValidation {

    private ProtoValidation() {}

    /**
     * Validates a request. Empty when valid; otherwise INVALID_ARGUMENT /
     * VALIDATION_FAILED with one field violation per protovalidate violation
     * (field path, message, rule id). A validator failure (a rule that cannot be
     * evaluated) is INTERNAL.
     */
    public static Optional<AppException> check(Validator validator, Message message) {
        ValidationResult result;
        try {
            result = validator.validate(message);
        } catch (ValidationException e) {
            return Optional.of(AppException.internal("validate request", e));
        }
        if (result.isSuccess()) {
            return Optional.empty();
        }
        List<FieldViolation> violations = new ArrayList<>(result.getViolations().size());
        for (Violation v : result.getViolations()) {
            build.buf.validate.Violation p = v.toProto();
            violations.add(FieldViolation.newBuilder()
                    .setField(fieldPathString(p.getField()))
                    .setDescription(p.getMessage())
                    .setRule(p.getRuleId())
                    .build());
        }
        return Optional.of(AppException.validation(violations));
    }

    /**
     * Validates a message the service builds itself (an event before it goes
     * to the outbox). A violation is a bug: IllegalStateException.
     */
    public static void require(Validator validator, Message message) {
        ValidationResult result;
        try {
            result = validator.validate(message);
        } catch (ValidationException e) {
            throw new IllegalStateException("validate " + message.getDescriptorForType().getName(), e);
        }
        if (!result.isSuccess()) {
            throw new IllegalStateException("invalid " + message.getDescriptorForType().getName() + ": " + result);
        }
    }

    /** The same text as Go's protovalidate.FieldPathString, e.g. {@code items[2].media_id}. */
    public static String fieldPathString(FieldPath path) {
        StringBuilder sb = new StringBuilder();
        List<FieldPathElement> elements = path.getElementsList();
        for (int i = 0; i < elements.size(); i++) {
            FieldPathElement e = elements.get(i);
            if (i > 0) {
                sb.append('.');
            }
            sb.append(e.getFieldName());
            switch (e.getSubscriptCase()) {
                case INDEX:
                    sb.append('[').append(Long.toUnsignedString(e.getIndex())).append(']');
                    break;
                case BOOL_KEY:
                    sb.append('[').append(e.getBoolKey()).append(']');
                    break;
                case INT_KEY:
                    sb.append('[').append(e.getIntKey()).append(']');
                    break;
                case UINT_KEY:
                    sb.append('[').append(Long.toUnsignedString(e.getUintKey())).append(']');
                    break;
                case STRING_KEY:
                    sb.append('[').append(quote(e.getStringKey())).append(']');
                    break;
                default:
                    break;
            }
        }
        return sb.toString();
    }

    private static String quote(String s) {
        StringBuilder sb = new StringBuilder("\"");
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':
                    sb.append("\\\"");
                    break;
                case '\\':
                    sb.append("\\\\");
                    break;
                case '\n':
                    sb.append("\\n");
                    break;
                case '\t':
                    sb.append("\\t");
                    break;
                case '\r':
                    sb.append("\\r");
                    break;
                default:
                    if (c < 0x20 || c == 0x7f) {
                        sb.append(String.format("\\x%02x", (int) c));
                    } else {
                        sb.append(c);
                    }
            }
        }
        return sb.append('"').toString();
    }
}
