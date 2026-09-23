# syntax=docker/dockerfile:1
#
# identity (Go): gRPC on :50051, /healthz and /readyz on :8081.
#
# The build context is back/, because the service compiles against the
# generated code in gen/go (run `make gen` in services/identity first; it also
# writes gen/go/go.mod). Dockerfile.dockerignore next to this file keeps the
# context to gen/go and services/identity.
#
#   cd back && docker build -f services/identity/Dockerfile -t agrogram/identity:dev .
#
# The image runs `serve` by default; `migrate up|down|status` and `healthcheck`
# are subcommands of the same binary (distroless has no shell).

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

# Dependencies first, for layer caching. The replace directive in
# services/identity/go.mod points at ../../gen/go, so its go.mod comes along.
COPY gen/go/go.mod gen/go/
COPY services/identity/go.mod services/identity/go.sum services/identity/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd services/identity && go mod download

COPY gen/go/ gen/go/
COPY services/identity/ services/identity/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd services/identity && \
    go build -trimpath -ldflags="-s -w" -o /out/identity ./cmd/identity

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="agrogram-identity" \
      org.opencontainers.image.description="AGROGRAM identity: accounts, roles, e-ijara farmer verification, plan"

COPY --from=build /out/identity /identity
USER nonroot:nonroot
EXPOSE 50051 8081
ENV IDENTITY_GRPC_ADDR=:50051 \
    IDENTITY_HTTP_ADDR=:8081

# GET /readyz on IDENTITY_HTTP_ADDR; ready = Postgres reachable.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/identity", "healthcheck"]

ENTRYPOINT ["/identity"]
CMD ["serve"]
