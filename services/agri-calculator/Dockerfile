# syntax=docker/dockerfile:1
#
# agri-calculator: PLACEHOLDER. Phase 0 is contract only; there is no service code yet.
#
# The image builds and runs but only prints a notice, so compose files and CI
# can reference it before the service exists. The real build lands with the
# implementation. Intended shape (Python; gRPC on :50051, health/metrics on :8081):
#
#   FROM python:3.14-slim
#   ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PYTHONPATH=/app/gen
#   WORKDIR /app
#   COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv
#   COPY services/agri-calculator/pyproject.toml services/agri-calculator/uv.lock ./
#   RUN uv sync --frozen --no-dev
#   COPY gen/python/ gen/
#   COPY services/agri-calculator/app/ app/
#   USER 10001
#   EXPOSE 50051 8081
#   CMD ["uv", "run", "--no-sync", "python", "-m", "app"]
#
# The application package is `app`: generated code occupies top-level
# names such as `common`, `events` and `analytics` on PYTHONPATH.
#
# Build context is back/, because the service compiles against gen/ from
# `make proto-gen`:
#   docker build -f services/agri-calculator/Dockerfile -t agrogram/agri-calculator:dev .

FROM busybox:1.37

LABEL org.opencontainers.image.title="agrogram-agri-calculator" \
      org.opencontainers.image.description="AGROGRAM agri-calculator (Python): placeholder, not implemented yet"

CMD ["echo", "agrogram agri-calculator: not implemented yet (Phase 0 scaffold, contract only)"]
