# syntax=docker/dockerfile:1.7
# ─────────────────────────────────────────────────────────────────────────────
# Stage 1 — build the Svelte SPA and write it into the Go embed dir.
# publish:go-static preserves the embed dir's .gitignore/README and writes
# build-info.json. Plain `npm run build` would rimraf the dir.
# ─────────────────────────────────────────────────────────────────────────────
FROM node:20-alpine AS frontend-builder

WORKDIR /app/apps/web/svelte

COPY apps/web/svelte/package*.json ./
RUN --mount=type=cache,target=/root/.npm npm ci

COPY apps/web/svelte/ ./

RUN mkdir -p /app/server/internal/webapp/static-next
RUN npm run publish:go-static

# ─────────────────────────────────────────────────────────────────────────────
# Stage 2 — build the Go binary with the SPA embedded.
# ─────────────────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS go-builder

ARG XUVA_VERSION=dev
ARG XUVA_COMMIT=unknown
ARG XUVA_BUILD_DATE=
ARG XUVA_DEFAULT_TMDB_API_KEY=
ARG XUVA_DEFAULT_FANARTTV_API_KEY=
ARG XUVA_DEFAULT_OMDB_API_KEY=

RUN apk add --no-cache git

WORKDIR /app/server

COPY server/go.mod server/go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY server/ ./

COPY --from=frontend-builder /app/server/internal/webapp/static-next ./internal/webapp/static-next

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X github.com/EJM-Digital/Xuva/server/internal/buildinfo.Version=${XUVA_VERSION} -X github.com/EJM-Digital/Xuva/server/internal/buildinfo.Commit=${XUVA_COMMIT} -X github.com/EJM-Digital/Xuva/server/internal/buildinfo.Date=${XUVA_BUILD_DATE} -X github.com/EJM-Digital/Xuva/server/internal/config.DefaultTMDBAPIKey=${XUVA_DEFAULT_TMDB_API_KEY} -X github.com/EJM-Digital/Xuva/server/internal/config.DefaultFanartTVAPIKey=${XUVA_DEFAULT_FANARTTV_API_KEY} -X github.com/EJM-Digital/Xuva/server/internal/config.DefaultOMDbAPIKey=${XUVA_DEFAULT_OMDB_API_KEY}" \
    -o /xuva ./cmd/Xuva

# ─────────────────────────────────────────────────────────────────────────────
# Stage 3 — minimal runtime image.
# Hardened for self-hosting: PUID/PGID at runtime via entrypoint, OCI image
# labels for inventory tools, alpine 3.21 (current stable), tini as PID 1 for
# correct signal forwarding and zombie reaping.
# ─────────────────────────────────────────────────────────────────────────────
FROM alpine:3.21

ARG XUVA_VERSION=dev

# Runtime deps:
#   ffmpeg/ffprobe — transcoding + probing
#   ca-certificates — HTTPS to TMDB / OMDB / FanartTV
#   tzdata — schedule semantics for "Date Added" + scan windows in non-UTC TZs
#   tini — proper PID 1 (SIGTERM forwarding + reaping)
#   su-exec — drop privilege in the entrypoint when PUID/PGID switch is needed
#   shadow — `usermod` / `groupmod` for runtime UID/GID changes
RUN apk add --no-cache \
      ffmpeg \
      ca-certificates \
      tzdata \
      tini \
      su-exec \
      shadow \
      wget

# Default UID/GID; users override at run-time via PUID/PGID env vars (the
# linuxserver.io / Plex / Jellyfin convention). Building the user up front
# means a stock `docker run` without PUID/PGID set works without any chown
# step on startup.
RUN addgroup -g 1000 xuva && \
    adduser -u 1000 -G xuva -s /bin/sh -D xuva && \
    mkdir -p /data && \
    chown xuva:xuva /data

COPY --from=go-builder /xuva /usr/local/bin/xuva
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

VOLUME /data

# Environment variables — see docs/docker.md for the full reference.
#   XUVA_DATA_DIR      path for database, cache, and generated files
#   XUVA_MOVIES_PATH   path where movie files are stored
#   XUVA_TV_PATH       path where TV episode files are stored
#   XUVA_HTTP_ADDR     listen address:port
#   XUVA_SERVER_NAME   display name shown in the web UI (default: Xuva)
#   XUVA_FFMPEG_PATH   override ffmpeg binary path
#   XUVA_FFPROBE_PATH  override ffprobe binary path
#   PUID / PGID        host UID/GID for media-mount permission matching
ENV XUVA_DATA_DIR=/data \
    XUVA_HTTP_ADDR=0.0.0.0:8097

EXPOSE 8097
EXPOSE 5353/udp

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:8097/api/health" || exit 1

# OCI image labels — picked up by GHCR, registry UIs, container inventory
# tools, and security scanners. Augments the per-build labels release.yml
# stamps via build-push-action.
LABEL org.opencontainers.image.title="Xuva" \
      org.opencontainers.image.description="Self-hosted media server — browse, scan, transcode, and stream your library." \
      org.opencontainers.image.url="https://github.com/EJM-Digital/Xuva" \
      org.opencontainers.image.source="https://github.com/EJM-Digital/Xuva" \
      org.opencontainers.image.documentation="https://github.com/EJM-Digital/Xuva#readme" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.vendor="Xuva" \
      org.opencontainers.image.version="${XUVA_VERSION}"

ENTRYPOINT ["/sbin/tini", "--", "/entrypoint.sh"]
CMD []
