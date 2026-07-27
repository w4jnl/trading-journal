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
- **HTTP API** with per-user API keys for read + append-only access to
  strategies and journal entries — built to be LLM/agent-friendly
  (discovery index + OpenAPI spec). See **[API.md](API.md)**.
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
- `proxy` -- trusts identity headers (`Remote-User`, `Remote-Email`,
  `Remote-Groups`) from an upstream reverse proxy like Authelia,
  oauth2-proxy, or Caddy `forward_auth`. See the [Proxy
  mode](#proxy-mode) section below.

See **[config.example.yaml](config.example.yaml)** for every auth
knob, with comments. `auth.session_secret` is the HMAC key for CSRF
tokens, session-id binding, and the act-as cookie -- set it to any
32+ byte string (or `base64:<random>`) so tokens survive restarts.
For deployments where the config file is checked in, the secret can
also be supplied at runtime via the `TJ_SESSION_SECRET` environment
variable (Docker secrets, systemd `EnvironmentFile`, etc.); the
env value overrides whatever is in the YAML.

### Managing users

Two surfaces are available, both backed by the same code path. Pick
whichever fits the task at hand.

**Web UI:** `/admin/users` (admins only; non-admins get 403). Inline
actions for create / reset password / promote-demote / deactivate-
activate / delete / act-as. Linked from the sidebar's **Admin →
Users** entry whenever the signed-in user has the admin flag.

**CLI:** the binary doubles as a user-management tool. Useful for
bootstrapping the first admin (no UI is reachable before then) and
for scripted maintenance:

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

`delete` is destructive. It removes everything that belongs to the
user, in one transaction with an end-of-tx `foreign_key_check`:

- DB rows: accounts, trades, executions, journal entries (every
  scope), `journal_entry_trades`, `journal_attachments` rows,
  balance anchors, daily stats, custom journal templates, sessions.
  `sync_log` rows have their `user_id` set to NULL so the import
  audit history survives anonymized.
- Files: journal attachment files referenced by the deleted entries,
  the user's `imports/inbox/<name>/`, `imports/processed/<name>/`,
  `imports/failed/<name>/`, and `imports/uploads/<id>/` directories.

Both the CLI and the web UI refuse to delete the last active admin
(promote another user first) and refuse to let an admin delete the
account they're signed in as.

### Admin act-as

Admins can browse the app as another user without learning their
password. From `/admin/users`, click **Act as** on the target row;
a signed cookie carries the target id, every page renders a
red-tinted "Acting as ALICE -- signed in as ADMIN" banner with a
**Return** button. The cookie is HMAC-signed (any tampering invalidates
it) and the target's `is_active` is re-checked on every request, so
a deactivated target can't be impersonated even if a stale cookie
survives. POST `/admin/act-as/clear` (or the banner button) drops
the cookie. Audit: every act-as session uses the *target's* user_id
end-to-end (queries, sync_log, importer attribution) -- the admin's
identity stays in the cookie for the banner only.

### Proxy mode

Set `auth.mode: "proxy"` to delegate authentication to a reverse
proxy. The app trusts identity from configurable headers (default
`Remote-User`, `Remote-Email`, `Remote-Groups`) but only when the
request originates from an allowlisted CIDR -- this is the only thing
between the public internet and impersonation, so it has to be set:

```yaml
auth:
  mode: "proxy"
  bootstrap_user: "admin"             # still required (owns flat-inbox drops)
  proxy:
    user_header: "Remote-User"
    email_header: "Remote-Email"
    display_name_header: "Remote-Name"
    groups_header: "Remote-Groups"
    admin_group: "admin"              # comma-list match grants is_admin
    trusted_proxies:                  # CIDR allowlist for r.RemoteAddr
      - "172.20.0.0/16"
      - "127.0.0.1"
    auto_provision: false             # create users on first sight?
    logout_url: ""                    # where /logout sends the browser
```

On every request the proxy authenticator:

1. Verifies `r.RemoteAddr` falls inside `trusted_proxies`. If not,
   the headers are ignored and the request reads as unauthenticated.
2. Reads `user_header`; empty -> unauthenticated.
3. Looks up the username. If absent and `auto_provision: true`,
   creates the row with no password (proxy users never log in
   locally). Otherwise refuses.
4. Refreshes `email` / `display_name` / `is_admin` on the existing
   row so changes in the upstream SSO propagate. `is_admin` comes
   from an exact comma-separated match of `admin_group` inside
   `groups_header`.

`/login` redirects to `/`; `/logout` redirects to `logout_url` if
set, else returns a stub page asking the user to sign out via the
SSO.

A minimal Authelia setup pointing at the app uses
`forwarded_headers` with `Remote-User` / `Remote-Email` /
`Remote-Groups` and a per-rule access policy that grants `admin`
group membership to whoever should administer the trading journal.
oauth2-proxy and Caddy `forward_auth` work the same way; whichever
proxy you use, double-check that it strips client-supplied
`Remote-*` headers before forwarding -- otherwise the CIDR
allowlist is the only thing standing between a curious client and
impersonation.

## HTTP API

Beyond the web UI, the app exposes a small **HTTP API** for external
clients and AI agents: read your strategies and journal entries (trades
overview, detailed trade list, tags, assigned strategy, chart data + a
server-rendered PNG, embedded images), and append dated notes to an entry
(append-only — existing content is never overwritten).

Access is by **per-user API key**, generated under **Settings**
(`/settings`) and sent as `Authorization: Bearer tj_...` (or `X-API-Key`).
The surface is LLM-friendly: a discovery index at `GET /api/v1` and an
OpenAPI 3.1 spec at `GET /api/v1/openapi.json`, both behind the key.

Full reference — authentication, every endpoint, JSON shapes, and a
worked example — lives in **[API.md](API.md)**.

For AI agents the app also speaks **MCP** (Model Context Protocol) at
`/mcp`, authenticated by the same per-user keys — so Claude Code, the
Claude app, and ChatGPT can read trades/journal and write entries with
the key held in the connector config, never in a prompt. A ready-made
analysis workflow ships as the **`trading-journal-analyst`** skill
(`.claude/skills/trading-journal-analyst/`): it reviews a trading day,
groups copytrader fan-out, writes the per-trade/group analysis once, and
synthesizes the daily journal. See **[API.md](API.md)** for the MCP tool
list and setup.

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

### MotiveWave email capture + live ingest

MotiveWave can send an email on every order fill
(**Preferences → Orders → Send Email On Filled Order**). The app acts as
MW's SMTP server so those notifications land here instead of a real mail
provider — near-real-time fills for the accounts that have no API
connector (Rithmic props, AMP/CQG). Verified against live captures: the
notification carries the account id, order id, symbol, side, cumulative
filled quantity, and average fill price, and copier-target fills are
emailed individually per account.

Two layers, both on by default when `mw_email.enabled`:

- **Capture** — every message is archived verbatim as `.eml` under
  `mw_email.archive_dir` (default `data/mw-raw/`). Duplicate deliveries
  (SMTP retries) are dropped by content hash, surviving restarts; parse
  or storage problems are logged, never bounced back to MW.
- **Ingest** (`mw_email.ingest`, default true) — complete fill
  notifications become **provisional executions** immediately: trades
  and stats update in near-real-time. MW emails *cumulative* order
  snapshots (a 3-lot filling 1+2 sends "Quantity: 1" then
  "Quantity: 3"), so provisional rows are upserted per
  (account, order id), never appended per email.

At every startup the archive is **replayed** through the same ingest
path (oldest first, in the background), so fills whose live ingest was
missed — a crash between archive and ingest, a capture-only period, a
transient DB error — still land. The replay is idempotent: covered
orders come back unchanged/skipped, and a fill already recorded by a
manual process (a hand-entered row or an old-format CSV without order
ids) is detected by (symbol, side, qty, price, time) and left alone.

**The MotiveWave CSV export stays authoritative.** The Executions-view
export carries the same `Order ID` as the emails, so importing it
replaces the matching provisional rows exactly — an end-of-day
re-import (full or partial, once or repeatedly) never double-counts.
`/admin/transactions` shows each row's import method in the *Via*
column; provisional rows carry an `email · provisional` badge until a
CSV import supersedes them. Accounts live-synced via a broker API
(IBKR) skip email ingest automatically — their fills arrive through the
API; the email copy is archive-only.

Setup:

1. Enable the `mw_email:` section in `config.yaml` (see
   `config.example.yaml`) and restart the app. LAN/VPN only — never
   expose the listener publicly.
2. MW **Preferences → Email**: SMTP host = journal host's LAN IP; port
   per your `tls_mode` (MW infers encryption from the port: 25 = plain,
   465 = implicit TLS, 587 = STARTTLS; a nonstandard port like 2525 is
   treated as plain); username/password = your configured AUTH creds;
   any from/to addresses. **MW applies mail settings only after a full
   restart** — Apply → OK → restart MW → Send Test Email.
3. MW **Preferences → Orders**: enable *Send Email On Filled Order*.

Inspect any captured message (and what the parser makes of it) with:

```
./trading-journal mwmail parse data/mw-raw/<file>.eml
```

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

- **[API.md](API.md)** — the HTTP API: per-user key auth, the full
  endpoint reference, JSON shapes, and an agent-oriented worked example.
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
