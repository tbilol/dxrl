package uz.agrogram.orderescrow.grpc;

import io.grpc.Context;
import io.grpc.Contexts;
import io.grpc.ForwardingServerCall;
import io.grpc.ForwardingServerCallListener;
import io.grpc.Metadata;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.Status;
import java.time.Duration;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.slf4j.MDC;
import org.slf4j.event.Level;
import org.slf4j.spi.LoggingEventBuilder;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.error.AppException;
import uz.agrogram.orderescrow.error.ErrorCodes;

/**
 * Logs one line per call (method, request id, user, caller, code, reason,
 * duration) and puts the request id into the MDC while the call's callbacks
 * run, so every log line of the handler carries it. Health checks log at DEBUG;
 * server faults at ERROR with the cause. Mirrors the Go services'
 * loggingInterceptor.
 */
final class LoggingInterceptor implements ServerInterceptor {

    private static final Logger log = LoggerFactory.getLogger(LoggingInterceptor.class);

    static final String MDC_REQUEST_ID = "request_id";

    @Override
    public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(ServerCall<ReqT, RespT> call, Metadata headers,
            ServerCallHandler<ReqT, RespT> next) {
        long start = System.nanoTime();
        Principal p = Principal.current();
        CallInfo info = new CallInfo();
        String method = call.getMethodDescriptor().getFullMethodName();
        ServerCall<ReqT, RespT> logged = new ForwardingServerCall.SimpleForwardingServerCall<>(call) {
            @Override
            public void close(Status status, Metadata trailers) {
                logCall(method, p, info, status, Duration.ofNanos(System.nanoTime() - start));
                super.close(status, trailers);
            }
        };
        Context ctx = Context.current().withValue(CallInfo.KEY, info);
        ServerCall.Listener<ReqT> listener = withMdc(p.requestId(), () -> Contexts.interceptCall(ctx, logged, headers,
                next));
        return new ForwardingServerCallListener.SimpleForwardingServerCallListener<>(listener) {
            @Override
            public void onMessage(ReqT message) {
                withMdc(p.requestId(), () -> {
                    super.onMessage(message);
                    return null;
                });
            }

            @Override
            public void onHalfClose() {
                withMdc(p.requestId(), () -> {
                    super.onHalfClose();
                    return null;
                });
            }

            @Override
            public void onCancel() {
                withMdc(p.requestId(), () -> {
                    super.onCancel();
                    return null;
                });
            }

            @Override
            public void onComplete() {
                withMdc(p.requestId(), () -> {
                    super.onComplete();
                    return null;
                });
            }

            @Override
            public void onReady() {
                withMdc(p.requestId(), () -> {
                    super.onReady();
                    return null;
                });
            }
        };
    }

    private static <T> T withMdc(String requestId, java.util.function.Supplier<T> body) {
        String previous = MDC.get(MDC_REQUEST_ID);
        MDC.put(MDC_REQUEST_ID, requestId);
        try {
            return body.get();
        } finally {
            if (previous == null) {
                MDC.remove(MDC_REQUEST_ID);
            } else {
                MDC.put(MDC_REQUEST_ID, previous);
            }
        }
    }

    private static void logCall(String method, Principal p, CallInfo info, Status status, Duration took) {
        AppException error = info.error();
        Level level = method.startsWith("grpc.health.v1.Health/") || method.startsWith("grpc.reflection.")
                ? Level.DEBUG : Level.INFO;
        if (!status.isOk() && serverFault(status.getCode())) {
            level = Level.ERROR;
        }
        LoggingEventBuilder b = log.atLevel(level)
                .addKeyValue("method", method)
                .addKeyValue("request_id", p.requestId())
                .addKeyValue("duration_ms", took.toNanos() / 1_000_000.0);
        if (p.authenticated()) {
            b = b.addKeyValue("user_id", p.userId());
        }
        if (p.internal()) {
            b = b.addKeyValue("caller", p.caller());
        }
        b = b.addKeyValue("code", status.getCode().name());
        if (!status.isOk()) {
            b = b.addKeyValue("reason", (error != null ? error.reason()
                    : ErrorCodes.reasonForCode(status.getCode())).name());
            if (error != null && error.getCause() != null) {
                b = b.addKeyValue("error", String.valueOf(error.getCause()));
                if (level == Level.ERROR) {
                    b = b.setCause(error.getCause());
                }
            } else if (status.getDescription() != null) {
                b = b.addKeyValue("error", status.getDescription());
            }
        }
        b.log("rpc");
    }

    private static boolean serverFault(Status.Code code) {
        switch (code) {
            case INTERNAL:
            case UNKNOWN:
            case DATA_LOSS:
            case UNAVAILABLE:
            case DEADLINE_EXCEEDED:
                return true;
            default:
                return false;
        }
    }
}
