# syntax=docker/dockerfile:1
#
# Build from the REPOSITORY ROOT (this directory):
#
#   docker build -t moesekai-v2 .
#
# A .dockerignore keeps host-built web/node_modules and web/.next out of the
# context so they cannot clobber the Linux artifacts produced in the builders.

# ---- Stage 1: build the Next.js console ----
FROM node:20-alpine AS web-builder
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm install --no-audit --no-fund 2>/dev/null || npm install --no-audit --no-fund
COPY web/ .
# Console talks to the backend on the same origin via ingress/proxy in prod.
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

# ---- Stage 2: build the Go backend ----
FROM golang:1.25-alpine AS go-builder
WORKDIR /src
COPY server/go.mod server/go.sum* ./
RUN go mod download 2>/dev/null || true
COPY server/ .
# modernc.org/sqlite is pure Go, so CGO stays off.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /moesekai-server .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /moesekai-migrate ./cmd/migrate

# ---- Stage 3: runtime ----
FROM node:20-alpine AS runtime
RUN apk add --no-cache ca-certificates tzdata git nginx && \
    git config --system --add safe.directory '*'
WORKDIR /app

# Backend binaries.
COPY --from=go-builder /moesekai-server ./moesekai-server
COPY --from=go-builder /moesekai-migrate ./moesekai-migrate

# Next.js standalone-style runtime: copy build output + node_modules.
COPY --from=web-builder /web/.next ./web/.next
COPY --from=web-builder /web/public ./web/public
COPY --from=web-builder /web/node_modules ./web/node_modules
COPY --from=web-builder /web/package.json ./web/package.json
COPY --from=web-builder /web/next.config.ts ./web/next.config.ts

# Nginx reverse proxy config template (rendered at runtime by the entrypoint so
# the upstream ports follow WEB_PORT / BACKEND_PORT instead of being hardcoded).
COPY nginx.conf /etc/nginx/nginx.conf.template

# Optional seed translations (used on first run when the DB is empty). This repo
# ships no translations/ dir, so the entrypoint detects the absent seed and
# starts with an empty DB; uncomment the COPY below if you add a seed tree.
# COPY translations/ ./seed-translations/
COPY docker-entrypoint.sh ./docker-entrypoint.sh
RUN chmod +x ./docker-entrypoint.sh

ENV DB_PATH=/data/moesekai.db \
    DATA_DIR=/data \
    WEB_PORT=3000 \
    BACKEND_PORT=9090 \
    BACKEND_ORIGIN=http://localhost:9090

VOLUME ["/data"]
EXPOSE 80

CMD ["./docker-entrypoint.sh"]

