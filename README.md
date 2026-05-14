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
watches `./imports/inbox/` for CSV drops. Open
<http://localhost:8080/dashboard> and either upload your first export at
`/import` or drop a `.csv` into `imports/inbox/` and watch the
dashboard light up.

The schema lives in `internal/db/migrations/*.sql` (dbmate format) and
is applied automatically on startup from the embedded files. No
external migration tool required.

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
build+push flow against `registry.example.com`, the buildx bootstrap, and
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
- success → `imports/processed/<timestamp>_<filename>`
- failure → `imports/failed/<filename>` plus a sibling
  `<filename>.error.txt`

No file is silently lost.

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
