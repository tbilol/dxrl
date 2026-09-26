package uz.agrogram.orderescrow.grpc;

import io.grpc.Context;
import uz.agrogram.orderescrow.error.AppException;

/** Per-call state the logging interceptor reports: the error a handler or interceptor returned. */
final class CallInfo {

    static final Context.Key<CallInfo> KEY = Context.key("agrogram-call-info");

    private volatile AppException error;

    /** Records the error of the current call, if the logging interceptor runs. */
    static void recordError(AppException e) {
        CallInfo info = KEY.get();
        if (info != null) {
            info.error = e;
        }
    }

    AppException error() {
        return error;
    }
}
