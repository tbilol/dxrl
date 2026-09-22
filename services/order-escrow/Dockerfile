# syntax=docker/dockerfile:1
#
# order-escrow: PLACEHOLDER. Phase 0 is contract only; there is no service code yet.
#
# The image builds and runs but only prints a notice, so compose files and CI
# can reference it before the service exists. The real build lands with the
# implementation. Intended shape (Java; gRPC on :50051, health/metrics on :8081):
#
#   FROM eclipse-temurin:25-jdk AS build
#   WORKDIR /src
#   COPY gen/java/ gen/java/
#   COPY services/order-escrow/ services/order-escrow/
#   RUN cd services/order-escrow && ./gradlew --no-daemon bootJar
#
#   FROM eclipse-temurin:25-jre
#   COPY --from=build /src/services/order-escrow/build/libs/order-escrow.jar /app/app.jar
#   USER 10001
#   EXPOSE 50051 8081
#   ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75", "-jar", "/app/app.jar"]
#
# Build context is back/, because the service compiles against gen/ from
# `make proto-gen`:
#   docker build -f services/order-escrow/Dockerfile -t agrogram/order-escrow:dev .

FROM busybox:1.37

LABEL org.opencontainers.image.title="agrogram-order-escrow" \
      org.opencontainers.image.description="AGROGRAM order-escrow (Java): placeholder, not implemented yet"

CMD ["echo", "agrogram order-escrow: not implemented yet (Phase 0 scaffold, contract only)"]
