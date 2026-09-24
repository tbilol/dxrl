# syntax=docker/dockerfile:1
#
# auth-gateway (Go): gRPC on :50051, /healthz and /readyz on :8081.
#
# The build context is back/, because the service compiles against the
# generated code in gen/go (run `make gen` in services/auth-gateway first; it
# also writes gen/go/go.mod). Dockerfile.dockerignore next to this file keeps
# the context to gen/go and services/auth-gateway, minus secrets/.
#
#   cd back && docker build -f services/auth-gateway/Dockerfile -t agrogram/auth-gateway:dev .
#
# The image runs `serve` by default; `migrate up|down|status` and `healthcheck`
# are subcommands of the same binary (distroless has no shell). The signing key
# is NEVER baked in: mount it (compose.yaml mounts services/auth-gateway/secrets
# read-only) or pass AUTH_GATEWAY_JWT_PRIVATE_KEY.

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

# Dependencies first, for layer caching. The replace directive in
# services/auth-gateway/go.mod points at ../../gen/go, so its go.mod comes along.
COPY gen/go/go.mod gen/go/
COPY services/auth-gateway/go.mod services/auth-gateway/go.sum services/auth-gateway/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd services/auth-gateway && go mod download

COPY gen/go/ gen/go/
COPY services/auth-gateway/ services/auth-gateway/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd services/auth-gateway && \
    go build -trimpath -ldflags="-s -w" -o /out/auth-gateway ./cmd/auth-gateway

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="agrogram-auth-gateway" \
      org.opencontainers.image.description="AGROGRAM auth-gateway: sign-in, sessions, access and refresh tokens, JWKS"

COPY --from=build /out/auth-gateway /auth-gateway
USER nonroot:nonroot
EXPOSE 50051 8081
ENV AUTH_GATEWAY_GRPC_ADDR=:50051 \
    AUTH_GATEWAY_HTTP_ADDR=:8081

# GET /readyz on AUTH_GATEWAY_HTTP_ADDR; ready = Postgres reachable.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/auth-gateway", "healthcheck"]

ENTRYPOINT ["/auth-gateway"]
CMD ["serve"]
