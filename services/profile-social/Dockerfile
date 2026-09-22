# syntax=docker/dockerfile:1
#
# profile-social: PLACEHOLDER. Phase 0 is contract only; there is no service code yet.
#
# The image builds and runs but only prints a notice, so compose files and CI
# can reference it before the service exists. The real build lands with the
# implementation. Intended shape (Go; gRPC on :50051, health/metrics on :8081):
#
#   FROM golang:1.27-alpine AS build
#   WORKDIR /src
#   COPY gen/go/ gen/go/
#   COPY services/profile-social/ services/profile-social/
#   RUN cd services/profile-social && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
#         -o /out/profile-social ./cmd/profile-social
#
#   FROM gcr.io/distroless/static-debian12:nonroot
#   COPY --from=build /out/profile-social /profile-social
#   EXPOSE 50051 8081
#   ENTRYPOINT ["/profile-social"]
#
# Build context is back/, because the service compiles against gen/ from
# `make proto-gen`:
#   docker build -f services/profile-social/Dockerfile -t agrogram/profile-social:dev .

FROM busybox:1.37

LABEL org.opencontainers.image.title="agrogram-profile-social" \
      org.opencontainers.image.description="AGROGRAM profile-social (Go): placeholder, not implemented yet"

CMD ["echo", "agrogram profile-social: not implemented yet (Phase 0 scaffold, contract only)"]
