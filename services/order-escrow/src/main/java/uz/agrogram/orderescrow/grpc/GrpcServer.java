package uz.agrogram.orderescrow.grpc;

import build.buf.protovalidate.Validator;
import io.grpc.BindableService;
import io.grpc.Server;
import io.grpc.ServerBuilder;
import io.grpc.ServerInterceptor;
import io.grpc.netty.shaded.io.grpc.netty.NettyServerBuilder;
import io.grpc.protobuf.services.ProtoReflectionService;
import io.grpc.protobuf.services.ProtoReflectionServiceV1;
import java.io.IOException;
import java.io.UncheckedIOException;
import java.net.InetSocketAddress;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.context.SmartLifecycle;
import uz.agrogram.orderescrow.config.HostPort;

/**
 * The gRPC server (grpc-java on Netty) as a Spring lifecycle bean: it starts
 * after the context is ready and, on shutdown, stops accepting calls and lets
 * running ones finish for up to the shutdown timeout before cancelling them.
 *
 * <p>Registered: OrderEscrowService, the standard grpc.health.v1 service and,
 * when enabled, server reflection (v1 and v1alpha, like grpc-go's
 * reflection.Register). Interceptors, outermost first: request context
 * (Principal), logging and error reporting, protovalidate.
 */
public final class GrpcServer implements SmartLifecycle {

    private static final Logger log = LoggerFactory.getLogger(GrpcServer.class);

    /** Stops before the outbox relay and after readiness turned NOT_SERVING. */
    public static final int PHASE = Integer.MAX_VALUE - 100;

    private final HostPort address;
    private final List<BindableService> services;
    private final Validator validator;
    private final Duration shutdownTimeout;
    private volatile Server server;

    /**
     * @param services OrderEscrowService, the health service, ...
     * @param reflection register server reflection (grpcurl); local development only
     */
    public GrpcServer(HostPort address, List<BindableService> services, Validator validator, boolean reflection,
            Duration shutdownTimeout) {
        this.address = address;
        List<BindableService> all = new ArrayList<>(services);
        if (reflection) {
            all.add(ProtoReflectionServiceV1.newInstance());
            all.add(reflectionV1Alpha());
        }
        this.services = List.copyOf(all);
        this.validator = validator;
        this.shutdownTimeout = shutdownTimeout;
    }

    /**
     * Registers {@code services} on {@code builder} behind the interceptor chain,
     * outermost first: request context (Principal), logging and error
     * reporting, protovalidate. Also used by the tests with an in-process server.
     */
    public static <T extends ServerBuilder<T>> T configure(T builder, List<BindableService> services,
            Validator validator) {
        services.forEach(builder::addService);
        List<ServerInterceptor> outermostFirst = List.of(new PrincipalInterceptor(), new LoggingInterceptor(),
                new ValidationInterceptor(validator));
        // ServerBuilder.intercept: the interceptor added last runs first.
        for (int i = outermostFirst.size() - 1; i >= 0; i--) {
            builder.intercept(outermostFirst.get(i));
        }
        return builder;
    }

    @SuppressWarnings("deprecation")
    private static BindableService reflectionV1Alpha() {
        return ProtoReflectionService.newInstance();
    }

    @Override
    public void start() {
        InetSocketAddress bind = address.allInterfaces()
                ? new InetSocketAddress(address.port())
                : new InetSocketAddress(address.host(), address.port());
        NettyServerBuilder builder = configure(NettyServerBuilder.forAddress(bind), services, validator);
        try {
            server = builder.build().start();
        } catch (IOException e) {
            throw new UncheckedIOException("gRPC server cannot listen on " + address, e);
        }
        log.atInfo().addKeyValue("addr", address.host() + ":" + server.getPort()).log("grpc server listening");
    }

    @Override
    public void stop(Runnable callback) {
        Server s = server;
        if (s == null) {
            callback.run();
            return;
        }
        Thread t = new Thread(() -> {
            try {
                s.shutdown();
                if (!s.awaitTermination(shutdownTimeout.toMillis(), TimeUnit.MILLISECONDS)) {
                    log.warn("grpc server: calls still running after the shutdown timeout; cancelling them");
                    s.shutdownNow();
                    s.awaitTermination(5, TimeUnit.SECONDS);
                }
            } catch (InterruptedException e) {
                s.shutdownNow();
                Thread.currentThread().interrupt();
            } finally {
                server = null;
                log.info("grpc server stopped");
                callback.run();
            }
        }, "grpc-server-stop");
        t.start();
    }

    @Override
    public void stop() {
        stop(() -> {});
    }

    @Override
    public boolean isRunning() {
        return server != null;
    }

    @Override
    public int getPhase() {
        return PHASE;
    }

    /** The bound port (useful with port 0). */
    public int port() {
        Server s = server;
        if (s == null) {
            throw new IllegalStateException("gRPC server is not running");
        }
        return s.getPort();
    }
}
