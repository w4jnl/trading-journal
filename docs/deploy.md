# Deployment

[‹ README](../README.md)

One static binary, one SQLite file, a few directories. The server listens on `:8080` by
default, writes `./data/trading.db` and watches `./imports/inbox/`. Everything is relative to
the paths in `config.yaml`; `config.example.yaml` lists every key with a comment.

## Docker

The image `ghcr.io/w4jnl/trading-journal` is built for linux/amd64 and linux/arm64 on every
release (tags `vX.Y.Z`, `vX.Y`, `vX`, `latest`), from distroless `static`: no shell, runs as
uid 65532. A development image from main between releases is one command away,
`gh workflow run docker.yml --ref main`, and comes out as `:dev` (plus `:main` and the short
sha); `:dev` moves with every such run, so pin the sha for anything that must stay put. `Docker/docker-compose.yml` is the deploy recipe:

```sh
git clone git@github.com:w4jnl/trading-journal.git && cd trading-journal/Docker
cp config.example.yaml config.yaml && $EDITOR config.yaml
mkdir -p data imports/{inbox,processed,failed,uploads} oi_dashboard
sudo chown -R 65532:65532 data imports oi_dashboard
docker compose pull && docker compose up -d
docker compose logs -f trading-journal
```

Paths inside the container: `/etc/trading-journal/config.yaml`, `/var/lib/trading-journal/data`
(database and attachments), `/var/lib/trading-journal/imports`, and
`/var/lib/trading-journal/oi_dashboard` read-only for the open-interest files. To update, `docker
compose pull && docker compose up -d`; migrations run at start. There is no shell in the image,
so user management runs as `docker compose run --rm trading-journal users add bob
--config=/etc/trading-journal/config.yaml` against the mounted database, or you switch to proxy
mode. `GET /healthz` is the probe. [Docker/README.md](../Docker/README.md)
has the manual multi-arch build.

## Binary

```sh
go build -o trading-journal ./cmd/server      # Go 1.26 or newer, no cgo
cp config.example.yaml config.yaml
./trading-journal -config config.yaml
```

`make build` does the same, stamping the version from the git tag (the sidebar footer shows
it). A systemd unit needs nothing special: run the binary in a working directory it may write,
with `-config` pointing at the file. Set `server.external_url` to the address users reach the
app on; the API docs, the OpenAPI servers list and the pairing QR use it.

## Reverse proxy

Terminate TLS at your proxy and keep `auth.cookie_secure` on. In local mode nothing else is
needed. In proxy mode (Authelia, oauth2-proxy, Caddy `forward_auth`) set
`auth.proxy.trusted_proxies` to the proxy's own address and make sure the proxy strips
client-supplied `Remote-*` headers; [auth.md](auth.md) has the headers and the flow. MCP clients
that sign in through OAuth need `/mcp`, `/oauth/*` and `/.well-known/oauth-*` to bypass the
proxy's own authentication; [mcp-oauth-setup.md](mcp-oauth-setup.md) has the rules per proxy.
The MotiveWave SMTP listener and the IBKR gateway are LAN or VPN only; never publish them.

## Secrets

Three values the config carries that should not live in a checked-in file. Each has an
environment variable that wins over the YAML, for Docker secrets or a systemd `EnvironmentFile`:

| Config key | Env | Protects |
| --- | --- | --- |
| `auth.session_secret` | `TJ_SESSION_SECRET` | sessions, CSRF tokens, the act-as cookie |
| `secrets.encryption_key` | `TJ_ENCRYPTION_KEY` | users' AI provider keys and TopstepX credentials (AES-256-GCM) |
| `ai.api_key`, `gex.api_key`, `topstepx.encryption_key`, `apns.key_file` | `TJ_AI_API_KEY`, `TJ_GEX_API_KEY`, `TJ_TOPSTEPX_ENC_KEY`, `TJ_APNS_KEY` | the server-wide AI key, the gex-engine token, the older TopstepX key, the APNs key PEM |

Generate the keys with `openssl rand -hex 32` and back them up with the database: a restored
database without its encryption key means every user re-enters their provider keys.

## Phone, push, Live Activity

The iOS app pairs through a code shown under Settings > Phone app and then talks to `/api/v1`
only, so it needs the same address the browser uses. Push and the Session Live Activity need an
APNs key from your Apple developer account in the `apns` section (`key_file` or `TJ_APNS_KEY`,
`key_id`, `team_id`); without one the app works but gets no pushes. The app is built from
`app/ios` with your own team, see [app/ios/README.md](../app/ios/README.md).

## Backup and restore

Everything mutable is in two places: `data/` (the database and the journal attachments) and
`imports/` (the CSV history; `processed/` keeps every successful import).

With `backup.enabled: true` the server writes `backups/backup-YYYY-MM-DD.tar.gz` once a day at
`backup.hour`, plus once at start if today's is missing. Each archive holds a consistent
`VACUUM INTO` snapshot of the database, `attachments/`, `imports/processed/`,
`imports/failed/` and the `config.yaml` in force. Archives are mode 0600 because the config
carries secrets; keep the backup directory off the database's disk. Retention keeps the ten
newest daily archives plus one per month. Before any schema migration runs the server also
writes `backups/pre-migrate-YYYY-MM-DD-<version>.tar.gz` (the three newest are kept), so a
release can be rolled back to the exact pre-upgrade state.

Restore:

1. Stop the server or the container.
2. Unpack the archive: `mkdir restore && tar xzf backups/backup-YYYY-MM-DD.tar.gz -C restore`.
3. Check the snapshot: `sqlite3 restore/trading.db "PRAGMA integrity_check"` must print `ok`.
4. Move the live files aside and put the snapshot in place: `trading.db` (delete any stale
   `trading.db-wal` and `trading.db-shm` next to it), `data/attachments/`, and `config.yaml` if
   you need the old one.
5. Start the server. It applies any migrations newer than the snapshot; a binary older than the
   snapshot's schema refuses to start with "database schema is ahead of this binary", so roll the
   binary forward instead.

The raw MotiveWave e-mail archive (`data/mw-raw/`) is not part of the backup; it is an audit
trail the CSV imports make redundant.

## Housekeeping

`sync_log_retention_days` prunes the sync history the API polls write (rows that added fills or
errored stay 90 days, CSV imports forever). `market_data.bar_retention_days` prunes cached
intraday bars. `mw_email.archive_retention_days` prunes the e-mail archive. All run daily inside
the server; there is no cron to install.
