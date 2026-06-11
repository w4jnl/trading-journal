---
name: run-trading-journal
description: Build, run, boot, and smoke-test the trading-journal Go web app (server-rendered HTML + HTMX, pure-Go SQLite, and a per-user-key JSON API at /api/v1). Use when asked to run, start, launch, boot, build, smoke-test, drive, screenshot, or chart the trading journal, or to verify changes to its HTTP handlers, CSV importer, FIFO position builder, or market-data/chart code.
---

# Run: trading-journal

One Go binary (`cmd/server`): a server-rendered web app (`html/template` + HTMX,
uPlot/JS charts) plus an HTTP JSON API at `/api/v1`. Storage is a single pure-Go
SQLite file (`modernc.org/sqlite`, CGO-free); migrations apply automatically on
boot. CSV exports (MotiveWave / TopstepX) dropped in `imports/inbox/` are
auto-imported and turned into trades by a FIFO position builder.

**Drive it with the committed harness:**
`.claude/skills/run-trading-journal/driver.sh` boots a *throwaway* instance
(scratch DB + scratch port — never the repo's `./data/trading.db`), seeds a real
sample CSV so the importer/position builder run, then exercises HTML pages + the
JSON API + a rendered chart PNG and prints `RESULT: ALL PASS`. All paths below
are relative to the repo root.

## Prerequisites
- Go 1.26+ (verified `go1.26.2`). No CGO / C toolchain.
- `sqlite3`, `curl`. (`shasum`/`sha256sum` for the key step — the driver picks
  whichever exists.)
- Outbound network to Yahoo Finance — only for the optional chart-PNG step.

Verified on macOS (darwin/arm64). Pure-Go, so it builds the same on Linux:
`apt-get install -y golang sqlite3 curl`.

## Run (agent path) — the driver
```bash
.claude/skills/run-trading-journal/driver.sh
```
Builds, boots a throwaway instance, smoke-tests it, tears down. Knobs (env vars):
- `PORT=19000 ...` — port (default `18099`).
- `KEEP=1 ...` — leave the server + scratch dir running; prints the base URL and
  scratch path so you can poke it yourself.
- `SEED=0 ...` — skip the CSV import (empty DB).

With `KEEP=1`, drive it by hand — HTML via the proxy-auth header, the API via the
bearer key the driver minted (it prints both):
```bash
curl -H "Remote-User: admin" -H "Remote-Groups: admin" http://127.0.0.1:18099/trades
```

## Build / boot by hand (what the driver automates)
```bash
go build -o /tmp/tj/trading-journal ./cmd/server          # CGO-free
# Write a scratch config: auth.mode=proxy (no password), database.path + port
# under /tmp, backup off. See driver.sh for the exact config heredoc.
/tmp/tj/trading-journal -config /tmp/tj/config.yaml        # auto-migrates; boot-sweeps imports/inbox/
curl -sf http://127.0.0.1:18099/healthz                    # readiness probe
```
Mint an API key without the login UI (key auth is independent of sessions):
```bash
TOKEN="tj_$(date +%s)_x"; SHA=$(printf '%s' "$TOKEN" | shasum -a 256 | awk '{print $1}')
sqlite3 /tmp/tj/data/trading.db "INSERT INTO api_keys (user_id,name,key_prefix,key_sha256) VALUES (1,'cli','tj_cli','$SHA');"
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:18099/api/v1
```

## Visual artifact
No browser/chromium here, so there is no DOM screenshot. The app's own
server-rendered **chart PNG** is the visual check: the driver warms one trade's
bars and writes `chart.png` to the scratch dir (use `KEEP=1` to inspect) — a
candlestick of the trade with entry/exit markers, from
`GET /api/v1/trades/{id}/chart.png` (also `/api/v1/journal/{id}/chart.png`).

## Run (human path)
Set `auth.mode: local` in `config.yaml` (copy `config.example.yaml`), build, and
run; first boot creates the bootstrap user with no password, so set one with
`./trading-journal -config config.yaml users reset admin` (interactive — see
Gotchas), then open `http://localhost:8080/login`. Useless headless, which is why
the agent path uses proxy auth + the API instead.

## Gotchas (battle scars)
- **`users reset` is interactive and will hang a non-tty/agent shell** — it reads
  the password from the terminal. Don't script it. For non-interactive runs use
  **proxy auth** (a `Remote-User` header authenticates HTML requests, no
  password) and mint API keys by inserting `sha256(token)` into `api_keys`
  directly. This is the single biggest unlock for driving the app.
- **Proxy auth needs the loopback in `trusted_proxies`** (`127.0.0.1/32`) *and*
  you must connect via `127.0.0.1` — otherwise the `Remote-User` header is
  ignored and every page is 401.
- **Never run against `./data/trading.db`** — point `database.path` at a scratch
  file (the driver does). It's the real user's data.
- **Times are stored as Go strings** (`2006-01-02 15:04:05 -0700 MST`), which
  SQLite's `date()`/`datetime()` cannot parse. Filter with `substr(col,1,10)` or
  `LIKE '2026-05-18%'`, never `date(col)` — the latter silently returns nothing.
- **`/api/v1/.../chart.png` serves cached bars only** (it never fetches) → 404 on
  a fresh instance until bars are warmed: `GET /charts/trade/{id}?load=1` (needs
  Yahoo network) or boot with `market_data.prefetch: true`.
- **`/` 302-redirects to `/dashboard`** — drive `/dashboard` or follow redirects.
- **`/api/*` bypasses CSRF + session (key auth only); the HTML app keeps CSRF.**
  So API writes need only the bearer key, but scripting an HTML *form POST* needs
  the token from the page's `<meta name="csrf-token">`. The driver only GETs HTML.

## Troubleshooting
- `healthz` never comes up → read `"$SCRATCH"/server.log` (the driver tails it on
  failure). Usual causes: port already in use (`PORT=`), or a bad
  `database.path`.
- HTML 401 even with the header → `trusted_proxies` missing `127.0.0.1/32`, or you
  hit `localhost` that resolved to `::1` (use `127.0.0.1`).
- `chart.png` returns 404 → no cached bars / no Yahoo network. That's a SKIP, not
  a failure (the driver reports it as such).
- `go build` errors on language version → needs Go 1.26+ (`go version`).

## Test (the project's own suite)
```bash
go test ./...
```
