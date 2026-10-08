# syntax=docker/dockerfile:1

# ---- Build stage ----
# Version matches go.mod (go 1.25.0).
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads between builds.
COPY go.mod go.sum ./
RUN go mod download

# Web UI is embedded via //go:embed web in main.go — the binary is
# self-contained, no asset copying needed.
COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/zcode-proxy .

# ---- Runtime stage ----
# Alpine (not distroless) so busybox wget is available for the container
# healthcheck defined in docker-compose.yml.
FROM alpine:3.20

# Chromium for captcha solving (captcha.go findRealBrowser picks up
# /usr/bin/chromium-browser). Without it every Aliyun captcha solve fails inside
# the container and the relay silently degrades to captcha-less JWT-direct.
# Headed-manual fallback still can't open a display here — headless only.
RUN apk add --no-cache chromium

RUN addgroup -g 10001 app && adduser -D -u 10001 -G app -H app \
    && mkdir -p /app/data \
    && chown -R app:app /app

COPY --from=build /out/zcode-proxy /app/zcode-proxy

# SQLite DB, the auto-generated vault.key and the bootstrap admin password
# file initial_admin_password (0600; written on first start instead of being
# logged, see auth.go) all live here. Bind mounts must be chown'd 10001:10001
# or the app cannot write them (see docker-compose.yml header).
VOLUME ["/app/data"]

# Documentation only — the real port comes from listen_addr in config/config.json
# (default 127.0.0.1:8687; inside Docker it must be 0.0.0.0:8687 to be reachable).
EXPOSE 8687

USER app
WORKDIR /app

ENTRYPOINT ["/app/zcode-proxy"]
# Flag names per main.go: -config takes the config DIRECTORY (config.json is
# read from inside it), -db takes the SQLite database path.
CMD ["-config", "/app/config", "-db", "/app/data/zcode-proxy.db"]
