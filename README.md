# Trading Journal

A single-user, self-hosted trading journal for futures traders. Imports
fills from MotiveWave and TopstepX CSV exports, builds trades via a FIFO
position builder, and gives you a place to write about them.

Server-rendered Go app, SQLite storage, HTMX for the dynamic bits. No
JavaScript framework, no broker API integration, no cloud — drop a CSV
into a folder (or upload one) and the rest of the app comes to life
around it.

## What it does

- **Imports** MotiveWave CSV exports (covers Apex / Lucid / IBKR / AMP-CQG
  via one parser) and TopstepX exports through a second one. Drop a file
  into `imports/inbox/` or upload it at `/import`; the watcher handles
  the rest. Format is auto-detected from the header row.
- **Trades** are derived from fills by a FIFO position builder: partial
  closes, long→short flips, and commission allocation all handled per
  account per symbol.
- **Journal** entries attach to any of four scopes — a single trade, a
  whole day, an ISO week, or a freely-assembled group of trades. Each
  view shows the same shape: tag picker, rich markdown editor (paste
  screenshots, drag-drop images, apply built-in templates, recall prior
  entries), and the related trades panel.
- **Dashboard** leads with a 60-day cumulative P&L curve, a Today panel
  with the day's net P&L as a hero number, and a five-tile stat strip
  with inline-SVG sparklines for trades / win rate / R-multiple /
  streak.
- **Analytics** auto-surfaces findings (best/worst day-of-week + hour,
  most-traded vs most-profitable symbol, profit-factor and R-multiple
  flags) and lets you drill into `/trades` from breakdown rows by
  symbol or direction.
- **Calendar** is the at-a-glance month view with per-day P&L heat and
  weekly summaries — clicking a day pivots to the trades on it.
- **Exports** to Markdown and PDF, filterable by the same controls as
  the on-screen feed. Each entry carries its summary line + up to five
  underlying trades.
- **⌘K command palette** for fast navigation, recent pages, symbol
  jumps (type `MNQ` → `/trades?symbol_root=MNQ`), and date keywords
  (`today`, `last week`, …).
- **Themes** light + dark, persisted; full keyboard story across the
  app (`j`/`k` to walk trades, `g`/`n`/`b` to tag, `⌘S` to save, `⌘D`
  to journal a multi-trade selection, `⌘P` to toggle live preview).

## Quick start (local)

```bash
# 1) Build (Go 1.26+, no CGO needed — modernc.org/sqlite is pure Go)
go build -o trading-journal ./cmd/server

# 2) Configure
cp config.example.yaml config.yaml
$EDITOR config.yaml        # set port, timezone, paths if you want

# 3) Run
./trading-journal -config config.yaml
```

By default the server binds on `:8080`, writes `./data/trading.db`, and
watches `./imports/inbox/` for CSV drops.

On first start the app creates a single user (default name `admin`,
configurable via `auth.bootstrap_user`) and assigns every existing
account / trade / journal entry to that user. Set its password and
sign in:

```bash
./trading-journal users reset admin   # interactive password prompt
./trading-journal                     # start the server
# open http://localhost:8080/  — login at /login
```

Then either upload your first export at `/import` or drop a `.csv`
into `imports/inbox/` and watch the dashboard light up.

The schema lives in `internal/db/migrations/*.sql` (dbmate format) and
is applied automatically on startup from the embedded files. No
external migration tool required.

## Authentication & users

The app is multi-user. Every request resolves to a row in the `users`
table; data isolation is enforced at the query layer so two users on
the same DB never see each other's accounts, trades, journal entries,
balances, or sync log.

**Auth modes** are picked by `auth.mode` in `config.yaml`:

- `local` (default) -- built-in login page at `/login`, bcrypt
  passwords, sqlite-backed sessions, HMAC-bound CSRF tokens on every
  mutating request. Cookie is `HttpOnly + SameSite=Lax` and `Secure`
  by default (drop `auth.cookie_secure: false` if you're running plain
  HTTP behind a trusted VPN).
- `proxy` -- reserved for a future phase. Will trust `Remote-User`
  (configurable) from an upstream reverse proxy like Authelia,
  oauth2-proxy, or Caddy `forward_auth`. Not implemented yet.

See **[config.example.yaml](config.example.yaml)** for every auth
knob, with comments. `auth.session_secret` is the HMAC key for CSRF
tokens -- set it to any 32+ byte string (or `base64:<random>`) so
tokens survive restarts.

### Managing users

All user management is through the CLI subcommand, which shares the
same binary and config as the server:

```bash
./trading-journal users -h            # full subcommand help

# Create / reset
./trading-journal users add bob              # prompts for password (x2)
./trading-journal users add jane --admin --email=jane@example.com
./trading-journal users reset bob            # also destroys bob's sessions

# Inspect
./trading-journal users list                 # id, username, admin, active, email

# Toggle state
./trading-journal users promote bob          # grant admin
./trading-journal users demote bob           # revoke admin
./trading-journal users deactivate bob       # disable login + kill sessions
./trading-journal users activate bob

# Permanent removal
./trading-journal users delete bob           # interactive: retype username
./trading-journal users delete bob --yes     # skip the retype prompt
```

`delete` is destructive. It cascades through trades, executions,
journal entries (every scope), balance anchors, daily stats, custom
journal templates, sessions, and accounts -- the user's slice of the
DB is gone. `sync_log` rows are preserved with `user_id` set NULL so
the import audit history survives.

The CLI refuses to delete the last active admin so the deployment
doesn't end up unmanageable -- promote another user first.

## Quick start (Docker)

A container-based deployment is in `Docker/`:

```bash
cd Docker
cp config.example.yaml config.yaml      # adjust timezone if needed
mkdir -p data imports/{inbox,processed,failed,uploads}
sudo chown -R 65532:65532 data imports  # for the nonroot container user
docker compose pull
docker compose up -d
```

See **[Docker/README.md](Docker/README.md)** for the multi-arch
build+push flow against `ghcr.io/w4jnl/trading-journal`, the buildx bootstrap, and
the chown step explained in detail.

## Stack

- Go 1.26, stdlib `net/http` with `http.ServeMux` (method-based routing,
  no chi).
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) — pure
  Go SQLite, CGO-free.
- [`sqlc`](https://sqlc.dev) for type-safe queries from raw SQL.
- `dbmate`-format migrations, embedded via `embed.FS` and applied by a
  small runtime runner.
- `html/template` server-rendering, [HTMX](https://htmx.org) for the
  partial swaps (trade detail modal, journal preview), inline SVG for
  sparklines, [uPlot](https://github.com/leeoniya/uPlot) loaded from
  CDN for the few real charts (dashboard equity curve, trades
  cumulative, analytics daily series).
- A single CSS file (`web/static/css/app.css`) carries the design system
  — tokens, light/dark themes, journal editor, trade modal, calendar
  heat, etc. No build step; the file ships as-is.
- Inter + JetBrains Mono web fonts served locally from
  `web/static/fonts/`.

## Project layout

```
cmd/server/             — main()
internal/
  config/               — YAML config loader
  db/                   — sqlite open + migrations runner
  db/migrations/        — *.sql migration files (dbmate format)
  db/queries/           — *.sql files for sqlc
  models/               — sqlc output (Queries struct, row types)
  importer/             — CSV parsers (MotiveWave, TopstepX),
                          ImportService, inbox watcher
  positions/            — FIFO position builder
  handlers/             — HTTP layer (one file per page + a few helpers)
web/
  templates/            — html/template tree, embedded into the binary
  static/css/app.css    — design system
  static/js/            — toast, journal editor, trade sheet, command palette
  static/fonts/         — Inter + JetBrains Mono woff2
Docker/                 — Dockerfile, docker-compose, deployment README
exportData/             — sample CSVs used to verify the importers
```

## Data sources (v1)

The app intentionally avoids broker APIs in v1 — the trader already
runs MotiveWave on every brokered account, so one MotiveWave CSV
importer covers Apex / Lucid / IBKR / AMP-CQG. TopstepX gets its own
importer because it isn't a MotiveWave-supported broker.

Two entry points, same code path:

1. **UI upload** at `/import` (drag-drop or file picker).
2. **Watch folder** — `fsnotify` on `imports/inbox/`. Drop a file there
   and it auto-imports.

After processing, files move out of `inbox/`:
- success → `imports/processed/<username>/<timestamp>_<filename>`
- failure → `imports/failed/<username>/<filename>` plus a sibling
  `<filename>.error.txt`

No file is silently lost.

### Per-user inbox subdirectories

The watcher recognizes `imports/inbox/<username>/` subdirs and routes
each one's drops to that user. Subdir name must match a `users.username`
in the DB and the user must be active (`is_active = 1`); subdirs that
don't resolve are logged at startup and ignored. So:

```
imports/
  inbox/
    alice/   foo.csv          ← alice's account
    bob/     bar.csv          ← bob's account
    leftover.csv              ← still works → bootstrap user
  processed/
    alice/   2026-…_foo.csv
    bob/     2026-…_bar.csv
    2026-…_leftover.csv       ← flat path for flat drops
  failed/    … (mirrors processed/<username>/ …)
```

Files dropped directly into `inbox/` (no subdir) still work for
single-user compatibility — they get attributed to the bootstrap user
and use the flat `processed/` / `failed/` paths. The UI upload at
`/import` always uses the request's logged-in user, regardless of
where the staged file ends up.

## Backup

Everything mutable lives in two places:

- `data/` — the SQLite database and journal attachments (chart
  screenshots).
- `imports/` — CSV history; `processed/` keeps a record of every
  successful import.

A nightly `tar czf data-$(date +%F).tar.gz data/` is the single backup
that matters; `imports/processed/` is reproducible from the broker
exports you already have on hand.

## Further reading

- **[CLAUDE.md](CLAUDE.md)** — repository-wide rules and design
  decisions that are easy to violate by accident (TZ handling, the
  `executions → trades` derivation, instrument stub auto-creation,
  firm-guessing for new accounts, sqlc-vs-raw-SQL trade-offs). Read
  this before scaffolding new features.
- **[trading-journal-design.md](trading-journal-design.md)** — the
  original design spec. Schema and architecture still apply except
  where CLAUDE.md overrides (notably the v1 scope decision to skip
  live broker APIs).
- **[Docker/README.md](Docker/README.md)** — deployment.
