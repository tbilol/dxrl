# syntax=docker/dockerfile:1
#
# profile-social (Go): gRPC on :50051, /healthz and /readyz on :8081.
#
# The build context is back/, because the service compiles against the
# generated code in gen/go (run `make gen` in services/profile-social first; it
# also writes gen/go/go.mod). Dockerfile.dockerignore next to this file keeps
# the context to gen/go and services/profile-social.
#
#   cd back && docker build -f services/profile-social/Dockerfile -t agrogram/profile-social:dev .
#
# The image runs `serve` by default (gRPC, HTTP health, outbox relay and the
# account/transaction-stream consumer); `migrate up|down|status` and
# `healthcheck` are subcommands of the same binary (distroless has no shell).

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0

# Dependencies first, for layer caching. The replace directive in
# services/profile-social/go.mod points at ../../gen/go, so its go.mod comes along.
COPY gen/go/go.mod gen/go/
COPY services/profile-social/go.mod services/profile-social/go.sum services/profile-social/
RUN --mount=type=cache,target=/go/pkg/mod \
    cd services/profile-social && go mod download

COPY gen/go/ gen/go/
COPY services/profile-social/ services/profile-social/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd services/profile-social && \
    go build -trimpath -ldflags="-s -w" -o /out/profile-social ./cmd/profile-social

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="agrogram-profile-social" \
      org.opencontainers.image.description="AGROGRAM profile-social: public profiles, follows, ratings, a user's own listings"

COPY --from=build /out/profile-social /profile-social
USER nonroot:nonroot
EXPOSE 50051 8081
ENV PROFILE_SOCIAL_GRPC_ADDR=:50051 \
    PROFILE_SOCIAL_HTTP_ADDR=:8081

# GET /readyz on PROFILE_SOCIAL_HTTP_ADDR; ready = Postgres reachable.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/profile-social", "healthcheck"]

ENTRYPOINT ["/profile-social"]
CMD ["serve"]
