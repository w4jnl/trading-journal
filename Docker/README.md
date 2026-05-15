# Docker deployment

Multi-arch container build + push for the trading journal. The image is
fully static (pure-Go binary, distroless `static` base), so the same
`linux/amd64` + `linux/arm64` manifest works on x86 Linux servers, Apple
Silicon, and Raspberry Pi 4/5 alike.

## Files in this folder

| File                  | Purpose                                                   |
| --------------------- | --------------------------------------------------------- |
| `Dockerfile`          | Multi-stage, multi-arch image definition                  |
| `docker-compose.yml`  | Deploy recipe — pulls `ghcr.io/w4jnl/trading-journal`   |
| `config.example.yaml` | Container-path config; copy to `config.yaml` and edit     |
| `README.md`           | This file                                                 |

## One-time prerequisites

```bash
# 1) Make sure Docker Desktop / Engine has buildx available
docker buildx version

# 2) Bootstrap a buildx builder that supports multi-arch via QEMU emulation
#    (skip if you already have one; check with `docker buildx ls`)
docker buildx create --name multiarch --use --bootstrap

# 3) Log in to GHCR (use a PAT with write:packages)
echo "$GHCR_PAT" | docker login ghcr.io -u w4jnl --password-stdin
```

## Build + push (release)

Run from the **repo root** (build context = `.` so the Dockerfile can
reach the Go module). Pin the image to a short git ref so the registry
keeps an audit trail; `:latest` moves to whatever was last pushed.

```bash
GIT_SHA=$(git rev-parse --short HEAD)

docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --file Docker/Dockerfile \
  --build-arg VERSION="$GIT_SHA" \
  --tag ghcr.io/w4jnl/trading-journal:latest \
  --tag ghcr.io/w4jnl/trading-journal:"$GIT_SHA" \
  --push \
  .
```

`--push` is the buildx way of producing a multi-arch manifest — `docker
push` after a normal `docker build` would only push a single platform.
Don't substitute it.

If you want to inspect what's in the registry afterwards:

```bash
docker buildx imagetools inspect ghcr.io/w4jnl/trading-journal:latest
```

You should see a `manifest list` with one entry per platform.

## Local one-platform build (quick test)

For a fast smoke build of just your host architecture, no buildx needed:

```bash
docker build -t trading-journal -f Docker/Dockerfile .
docker run --rm -p 8080:8080 \
  -v "$(pwd)/Docker/config.example.yaml":/etc/trading-journal/config.yaml:ro \
  -v "$(pwd)/Docker/data":/var/lib/trading-journal/data \
  -v "$(pwd)/Docker/imports":/var/lib/trading-journal/imports \
  trading-journal
```

(Make sure the local `Docker/data` and `Docker/imports` dirs exist and
are writable by uid 65532 — see "Volume permissions" below.)

## Deploy with docker compose

On the host that runs the app:

```bash
git clone <repo> trading-journal
cd trading-journal/Docker

# Set up persistent paths
cp config.example.yaml config.yaml
$EDITOR config.yaml                       # adjust timezone, port if needed

mkdir -p data imports/{inbox,processed,failed,uploads}

# nonroot uid 65532 inside the container needs to own these dirs
sudo chown -R 65532:65532 data imports

# Pull the published multi-arch image (Docker picks the right platform)
docker compose pull
docker compose up -d
docker compose logs -f trading-journal
```

Browse to `http://<host>:8080/dashboard`. The app auto-creates the
SQLite schema on first start via the embedded migrations.

### Updating a deployed instance

```bash
cd /path/to/Docker
docker compose pull && docker compose up -d
```

Migrations apply on startup. The compose file uses `restart:
unless-stopped`, so the container will come back automatically after a
host reboot.

## Volume permissions

The image runs as the distroless `nonroot` user — uid/gid `65532:65532`.
SQLite needs write access to its database directory; the importer needs
to move files between `imports/inbox`, `imports/processed`, etc. The
host directories must be writable by that uid.

The cleanest one-shot:

```bash
sudo chown -R 65532:65532 data imports
```

If you'd rather the container run as a different uid (e.g., to match
your existing data ownership), add `user: "1000:1000"` to the
compose service. The distroless base is happy at any uid.

## Architecture coverage

| Platform        | Tag suffix in manifest  | Verified on                    |
| --------------- | ----------------------- | ------------------------------ |
| `linux/amd64`   | x86_64                  | Generic Linux server, NUC      |
| `linux/arm64`   | aarch64                 | Apple Silicon under Docker Desktop, Raspberry Pi 4/5, AWS Graviton |

Adding more is one flag away — append `linux/arm/v7` for 32-bit ARM
boards. The pure-Go binary cross-compiles cleanly under any
`GOOS=linux GOARCH=<x>` combo Go's toolchain supports.

## Health and observability

- `GET /healthz` returns `200 ok` once the server is past
  `ListenAndServe`. Use it as a probe from a reverse proxy (Traefik,
  Caddy) since the distroless base has no shell to run the probe from
  inside the container.
- Logs go to stdout/stderr at `slog`'s default text format. `docker
  compose logs -f trading-journal` is enough; ship them to your
  aggregator of choice via the standard Docker log driver if you want.

## Backup

Everything mutable is under the two host mounts:

```
data/                trading.db  +  attachments/
imports/inbox        unprocessed CSVs
imports/processed    successful imports (kept)
imports/failed       failures + .error.txt sidecars
imports/uploads      transient UI uploads (safe to clear)
```

A nightly `tar czf` of `data/` is the single backup that matters; the
imports/processed history is reproducible from the source CSVs you
already have.
