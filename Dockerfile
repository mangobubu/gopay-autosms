# syntax=docker/dockerfile:1.7

FROM node:22-bookworm-slim AS frontend-builder
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN if [ -f package-lock.json ]; then \
      npm ci --no-audit --no-fund; \
    else \
      npm install --no-audit --no-fund; \
    fi
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-bookworm AS backend-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend-builder /src/internal/webui/dist/ ./internal/webui/dist/
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags="-s -w" -o /out/autosms ./cmd/autosms

FROM postgres:16-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl tini \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=backend-builder /out/autosms /app/autosms
COPY docker-entrypoint.sh /app/docker-entrypoint.sh
RUN sed -i 's/\r$//' /app/docker-entrypoint.sh \
    && chmod 0755 /app/autosms /app/docker-entrypoint.sh \
    && mkdir -p /data \
    && chown -R postgres:postgres /data

ENV AUTOSMS_HTTP_ADDR=:8080 \
    AUTOSMS_DATA_DIR=/data \
    AUTOSMS_HEALTH_URL=http://127.0.0.1:8080/readyz \
    AUTOSMS_PUBLIC_URL=http://localhost:8080 \
    PGDATA=/data/postgres

VOLUME ["/data"]
EXPOSE 8080
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=6 \
  CMD curl --fail --silent --show-error "${AUTOSMS_HEALTH_URL}" >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/app/docker-entrypoint.sh"]
CMD []
