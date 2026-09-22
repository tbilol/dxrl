# syntax=docker/dockerfile:1
#
# signaling: PLACEHOLDER. Phase 0 is contract only; there is no service code yet.
#
# The image builds and runs but only prints a notice, so compose files and CI
# can reference it before the service exists. The real build lands with the
# implementation. Intended shape (Go; gRPC on :50051, health/metrics on :8081):
#
#   FROM golang:1.27-alpine AS build
#   WORKDIR /src
#   COPY gen/go/ gen/go/
#   COPY services/signaling/ services/signaling/
#   RUN cd services/signaling && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
#         -o /out/signaling ./cmd/signaling
#
#   FROM gcr.io/distroless/static-debian12:nonroot
#   COPY --from=build /out/signaling /signaling
#   EXPOSE 50051 8081
#   ENTRYPOINT ["/signaling"]
#
# Build context is back/, because the service compiles against gen/ from
# `make proto-gen`:
#   docker build -f services/signaling/Dockerfile -t agrogram/signaling:dev .

FROM busybox:1.37

LABEL org.opencontainers.image.title="agrogram-signaling" \
      org.opencontainers.image.description="AGROGRAM signaling (Go): placeholder, not implemented yet"

CMD ["echo", "agrogram signaling: not implemented yet (Phase 0 scaffold, contract only)"]
