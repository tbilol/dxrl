# syntax=docker/dockerfile:1
#
# media-pipeline: PLACEHOLDER. Phase 0 is contract only; there is no service code yet.
#
# The image builds and runs but only prints a notice, so compose files and CI
# can reference it before the service exists. The real build lands with the
# implementation. Intended shape (Go; gRPC on :50051, health/metrics on :8081):
#
#   FROM golang:1.27-alpine AS build
#   WORKDIR /src
#   COPY gen/go/ gen/go/
#   COPY services/media-pipeline/ services/media-pipeline/
#   RUN cd services/media-pipeline && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
#         -o /out/media-pipeline ./cmd/media-pipeline
#
#   # Needs FFmpeg (video transcode, poster frames) and an OCR engine
#   # (weighbridge receipts) at runtime, so no distroless here.
#   FROM alpine:3.24
#   RUN apk add --no-cache ffmpeg tesseract-ocr ca-certificates
#   COPY --from=build /out/media-pipeline /usr/local/bin/media-pipeline
#   USER 10001
#   EXPOSE 50051 8081
#   ENTRYPOINT ["/usr/local/bin/media-pipeline"]
#
# Build context is back/, because the service compiles against gen/ from
# `make proto-gen`:
#   docker build -f services/media-pipeline/Dockerfile -t agrogram/media-pipeline:dev .

FROM busybox:1.37

LABEL org.opencontainers.image.title="agrogram-media-pipeline" \
      org.opencontainers.image.description="AGROGRAM media-pipeline (Go): placeholder, not implemented yet"

CMD ["echo", "agrogram media-pipeline: not implemented yet (Phase 0 scaffold, contract only)"]
