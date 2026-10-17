# syntax=docker/dockerfile:1.7

# -----------------------------------------------------------------------------
# Multi-arch Dockerfile for the trading journal.
#
# Build context = repo root. The binary is fully static (CGO_ENABLED=0 +
# modernc.org/sqlite is pure Go), so the final image can be the distroless
# `static` base — no shell, no libc, ~2 MB on top of the binary.
#
# Build for the local platform only (quick test):
#   docker build -t trading-journal -f Docker/Dockerfile .
#
# Build + push multi-arch (linux/amd64, linux/arm64) via buildx — see
# Docker/README.md for the full bootstrap and tag conventions.
# -----------------------------------------------------------------------------

# --- Stage 1: build ----------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Buildx fills these from --platform; default to host arch when not present
# so `docker build` on the local platform still works without buildx.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Optional version stamp baked into the binary. CI sets this to a git ref;
# local builds get an empty string.
ARG VERSION=

WORKDIR /src

# Cache module downloads in their own layer so binary rebuilds don't refetch.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the rest of the source. The .dockerignore at the repo root (kept
# minimal: data/, imports/, .git, etc.) keeps build context lean.
COPY . .

# Pure-Go build. -trimpath drops the absolute paths from the embedded
# debug info; -ldflags strips DWARF and embeds the optional VERSION.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.Version=${VERSION}" \
      -o /out/trading-journal \
      ./cmd/server

# --- Stage 2: runtime --------------------------------------------------------
# distroless/static-debian12:nonroot ships CA certs + tzdata + nothing else;
# pairs perfectly with a static Go binary. Running as nonroot (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot

# Standard paths the binary expects under config. Bind-mount or compose
# volume these directories at runtime so data persists across upgrades.
#   /etc/trading-journal/config.yaml       — config
#   /var/lib/trading-journal/data/         — DB + attachments
#   /var/lib/trading-journal/imports/      — inbox/processed/failed/uploads
WORKDIR /app
COPY --from=builder /out/trading-journal /app/trading-journal

# Open port matches the example config.
EXPOSE 8080

# Image labels — registries (incl. ghcr.io) surface these.
LABEL org.opencontainers.image.title="Trading Journal" \
      org.opencontainers.image.description="Server-rendered trading journal app (Go + HTMX + SQLite)." \
      org.opencontainers.image.source="https://github.com/w4jnl/trading-journal" \
      org.opencontainers.image.licenses="Apache-2.0"

USER nonroot:nonroot
ENTRYPOINT ["/app/trading-journal"]
CMD ["-config", "/etc/trading-journal/config.yaml"]
