# syntax=docker/dockerfile:1
#
# catalog (Go): gRPC on :50051, /healthz and /readyz on :8081.
#
# The build context is back/, because the service compiles against the
# generated code in gen/go (run `make gen` in services/catalog first; it
# also writes gen/go/go.mod). Dockerfile.dockerignore next to this file keeps
# the context to gen/go and services/catalog.
#
#   cd back && docker build -f services/catalog/Dockerfile -t agrogram/catalog:dev .
#
# The image runs `serve` by default (gRPC, HTTP health, the outbox relay of
# the listing stream and the view counter); `migrate up|down|status` (schema
# and product seed) and `healthcheck` are subcommands of the same binary
# (distroless has no shell).

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

# Dependencies first, for layer caching. The replace directive in
# services/catalog/go.mod points at ../../gen/go, so its go.mod comes along.
COPY gen/go/go.mod gen/go/
COPY services/catalog/go.mod services/catalog/go.sum services/catalog/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd services/catalog && go mod download

COPY gen/go/ gen/go/
COPY services/catalog/ services/catalog/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd services/catalog && \
    go build -trimpath -ldflags="-s -w" -o /out/catalog ./cmd/catalog

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="agrogram-catalog" \
      org.opencontainers.image.description="AGROGRAM catalog: offer and buy-request boards, listing search, product reference data"

COPY --from=build /out/catalog /catalog
USER nonroot:nonroot
EXPOSE 50051 8081
ENV CATALOG_GRPC_ADDR=:50051 \
    CATALOG_HTTP_ADDR=:8081

# GET /readyz on CATALOG_HTTP_ADDR; ready = Postgres reachable.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/catalog", "healthcheck"]

ENTRYPOINT ["/catalog"]
CMD ["serve"]
