package uz.agrogram.orderescrow.grpc;

import io.grpc.Context;
import io.grpc.Contexts;
import io.grpc.ForwardingServerCall;
import io.grpc.Metadata;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.StatusRuntimeException;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import uz.agrogram.orderescrow.auth.Principal;
import uz.agrogram.orderescrow.error.AppException;

/**
 * Decodes the x-agrogram-* metadata into a {@link Principal} in the call's
 * io.grpc.Context and echoes x-request-id in the response metadata. Bad auth
 * metadata ends the call with UNAUTHENTICATED (logged here, like the Go
 * services' principalInterceptor).
 */
final class PrincipalInterceptor implements ServerInterceptor {

    private static final Logger log = LoggerFactory.getLogger(PrincipalInterceptor.class);

    static final Metadata.Key<String> REQUEST_ID =
            Metadata.Key.of(Principal.HEADER_REQUEST_ID, Metadata.ASCII_STRING_MARSHALLER);

    @Override
    public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(ServerCall<ReqT, RespT> call, Metadata headers,
            ServerCallHandler<ReqT, RespT> next) {
        Principal.Decoded decoded = Principal.decode(headers);
        Principal p = decoded.principal();
        ServerCall<ReqT, RespT> echoing = new RequestIdCall<>(call, p.requestId());
        if (decoded.error().isPresent()) {
            AppException e = decoded.error().get();
            log.atWarn().addKeyValue("method", call.getMethodDescriptor().getFullMethodName())
                    .addKeyValue("request_id", p.requestId()).addKeyValue("code", e.code().name())
                    .addKeyValue("error", e.getMessage()).log("rpc rejected: bad auth metadata");
            StatusRuntimeException sre = e.toStatusException();
            Metadata trailers = sre.getTrailers() == null ? new Metadata() : sre.getTrailers();
            echoing.close(sre.getStatus(), trailers);
            return new ServerCall.Listener<>() {};
        }
        Context ctx = Context.current().withValue(Principal.CONTEXT_KEY, p);
        return Contexts.interceptCall(ctx, echoing, headers, next);
    }

    /** Adds x-request-id to the response headers, or to the trailers of a trailers-only response. */
    private static final class RequestIdCall<ReqT, RespT>
            extends ForwardingServerCall.SimpleForwardingServerCall<ReqT, RespT> {
        private final String requestId;
        private volatile boolean headersSent;

        RequestIdCall(ServerCall<ReqT, RespT> delegate, String requestId) {
            super(delegate);
            this.requestId = requestId;
        }

        @Override
        public void sendHeaders(Metadata headers) {
            headers.put(REQUEST_ID, requestId);
            headersSent = true;
            super.sendHeaders(headers);
        }

        @Override
        public void close(io.grpc.Status status, Metadata trailers) {
            if (!headersSent && !trailers.containsKey(REQUEST_ID)) {
                trailers.put(REQUEST_ID, requestId);
            }
            super.close(status, trailers);
        }
    }
}
