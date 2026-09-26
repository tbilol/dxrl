# syntax=docker/dockerfile:1
#
# order-escrow (Java 17, Spring Boot): gRPC on :50051, /healthz and /readyz on :8081.
#
# The build context is back/, because the service compiles against the
# generated code in gen/java (run `make gen` in services/order-escrow first).
# Dockerfile.dockerignore next to this file keeps the context to gen/java and
# services/order-escrow.
#
#   cd back && docker build -f services/order-escrow/Dockerfile -t agrogram/order-escrow:dev .
#
# The image runs `serve` by default (gRPC, HTTP health, the outbox relay of the
# transaction stream); `migrate up|down|status` and `healthcheck` are commands
# of the same jar, like the Go services' binaries.

FROM eclipse-temurin:17-jdk AS build
WORKDIR /src

# The Gradle wrapper downloads the pinned Gradle (distributionSha256Sum) and the
# dependencies; the cache mount keeps both between builds.
COPY gen/java/ gen/java/
COPY services/order-escrow/ services/order-escrow/
RUN --mount=type=cache,target=/root/.gradle \
    cd services/order-escrow && \
    ./gradlew --no-daemon --console=plain bootJar

FROM eclipse-temurin:17-jre

LABEL org.opencontainers.image.title="agrogram-order-escrow" \
      org.opencontainers.image.description="AGROGRAM order-escrow: the escrow transaction (tranzaksiya) service"

RUN groupadd --system --gid 10001 agrogram && \
    useradd --system --uid 10001 --gid agrogram --home-dir /app --shell /usr/sbin/nologin agrogram
WORKDIR /app
COPY --from=build /src/services/order-escrow/build/libs/order-escrow.jar /app/order-escrow.jar
USER 10001:10001
EXPOSE 50051 8081
ENV ORDER_ESCROW_GRPC_ADDR=:50051 \
    ORDER_ESCROW_HTTP_ADDR=:8081

# GET /readyz on ORDER_ESCROW_HTTP_ADDR; ready = Postgres reachable. The probe
# is the jar's own `healthcheck` command (a small JVM, no Spring), so the image
# needs no curl.
HEALTHCHECK --interval=15s --timeout=5s --start-period=40s --retries=3 \
    CMD ["java", "-XX:TieredStopAtLevel=1", "-XX:+UseSerialGC", "-Xmx32m", "-jar", "/app/order-escrow.jar", "healthcheck"]

# Exec form: java is PID 1 and receives SIGTERM, which starts the graceful
# shutdown (readiness off, gRPC drain, relay stop) within ORDER_ESCROW_SHUTDOWN_TIMEOUT.
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75", "-XX:+ExitOnOutOfMemoryError", "-jar", "/app/order-escrow.jar"]
CMD ["serve"]
