# syntax=docker/dockerfile:1
#
# identity: PLACEHOLDER. Phase 0 is contract only; there is no service code yet.
#
# The image builds and runs but only prints a notice, so compose files and CI
# can reference it before the service exists. The real build lands with the
# implementation. Intended shape (Go; gRPC on :50051, health/metrics on :8081):
#
#   FROM golang:1.27-alpine AS build
#   WORKDIR /src
#   COPY gen/go/ gen/go/
#   COPY services/identity/ services/identity/
#   RUN cd services/identity && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
#         -o /out/identity ./cmd/identity
#
#   FROM gcr.io/distroless/static-debian12:nonroot
#   COPY --from=build /out/identity /identity
#   EXPOSE 50051 8081
#   ENTRYPOINT ["/identity"]
#
# Build context is back/, because the service compiles against gen/ from
# `make proto-gen`:
#   docker build -f services/identity/Dockerfile -t agrogram/identity:dev .

FROM busybox:1.37

LABEL org.opencontainers.image.title="agrogram-identity" \
      org.opencontainers.image.description="AGROGRAM identity (Go): placeholder, not implemented yet"

CMD ["echo", "agrogram identity: not implemented yet (Phase 0 scaffold, contract only)"]
