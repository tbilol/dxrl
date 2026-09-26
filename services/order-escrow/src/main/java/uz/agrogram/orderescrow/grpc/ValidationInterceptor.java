package uz.agrogram.orderescrow.grpc;

import build.buf.protovalidate.Validator;
import com.google.protobuf.Message;
import io.grpc.ForwardingServerCallListener;
import io.grpc.Metadata;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.StatusRuntimeException;
import java.util.Optional;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.error.ProtoValidation;

/**
 * Enforces the protovalidate annotations of every request: INVALID_ARGUMENT
 * with ERROR_REASON_VALIDATION_FAILED and one field violation per problem.
 * The handler never sees an invalid request.
 */
final class ValidationInterceptor implements ServerInterceptor {

    private final Validator validator;

    ValidationInterceptor(Validator validator) {
        this.validator = validator;
    }

    @Override
    public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(ServerCall<ReqT, RespT> call, Metadata headers,
            ServerCallHandler<ReqT, RespT> next) {
        ServerCall.Listener<ReqT> delegate = next.startCall(call, headers);
        return new ForwardingServerCallListener.SimpleForwardingServerCallListener<>(delegate) {
            private boolean rejected;

            @Override
            public void onMessage(ReqT message) {
                if (rejected) {
                    return;
                }
                if (message instanceof Message m) {
                    Optional<AppException> invalid = ProtoValidation.check(validator, m);
                    if (invalid.isPresent()) {
                        rejected = true;
                        CallInfo.recordError(invalid.get());
                        StatusRuntimeException sre = invalid.get().toStatusException();
                        call.close(sre.getStatus(), sre.getTrailers() == null ? new Metadata() : sre.getTrailers());
                        return;
                    }
                }
                super.onMessage(message);
            }

            @Override
            public void onHalfClose() {
                if (!rejected) {
                    super.onHalfClose();
                }
            }

            @Override
            public void onReady() {
                if (!rejected) {
                    super.onReady();
                }
            }
        };
    }
}
