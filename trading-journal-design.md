# Trading Journal — Design Document
*For handoff to Claude Code CLI*

---

## 1. Project Overview

A self-hosted web application for logging, analysing, and journalling daytrading activity across multiple accounts and brokers. The goal is a unified dashboard and diary modelled on the TopstepX trading dashboard experience — real-time session awareness, P&L calendar, per-trade and per-day notes — but broker-independent and fully self-owned.

**Stack:** Go (backend), SQLite (database), HTMX + Tailwind CSS (frontend — no JS framework)
**Deployment:** Single binary, self-hosted (Docker or bare metal), accessible from any browser on the local network or VPN

---

## 2. Account & Broker Inventory

| Account | Broker/Clearing | Data Feed | Sync Method |
|---|---|---|---|
| Apex Trader Funding | Prop firm | Rithmic | Rithmic R\|Protocol API |
| Lucid Trader Funding | Prop firm | Rithmic | Rithmic R\|Protocol API |
| AMP Futures | AMP/CQG | CQG | Scheduled CSV import |
| IBKR (multiple) | Interactive Brokers | IBKR | IBKR Client Portal API / TWS API |
| TopStep | Prop firm | Rithmic (underlying) | Rithmic R\|Protocol API |

Asset classes traded: **Futures** (primary), **Stocks** (secondary)

---

## 3. Architecture

```
┌─────────────────────────────────────────────────────┐
│                   Go Web Server                     │
│                                                     │
│  ┌──────────────┐   ┌──────────────────────────┐   │
│  │  HTTP Router │   │   Background Workers      │   │
│  │  (chi/stdlib)│   │   - Rithmic poller        │   │
│  │              │   │   - IBKR poller            │   │
│  │  HTMX pages  │   │   - CQG CSV watcher        │   │
│  │  REST API    │   │   - Scheduler (cron)       │   │
│  └──────────────┘   └──────────────────────────┘   │
│           │                     │                   │
│           └─────────┬───────────┘                   │
│                     ▼                               │
│              ┌─────────────┐                        │
│              │   SQLite DB  │                       │
│              │  (via sqlc)  │                       │
│              └─────────────┘                        │
└─────────────────────────────────────────────────────┘
```

### Key Go packages
- `net/http` + `go-chi/chi` — HTTP routing
- `mattn/go-sqlite3` or `modernc.org/sqlite` — SQLite driver (prefer modernc for pure Go, no CGO)
- `sqlc` — type-safe SQL query generation
- `robfig/cron` — scheduled polling jobs
- `dbmate` — database migrations
- Template rendering via Go's `html/template` with HTMX for partial page updates
- refer to ~/.claude/go-style.md for stiling guide

---

## 4. Database Schema

### `accounts`
```sql
CREATE TABLE accounts (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,           -- "Apex Main", "IBKR US Stocks"
    broker      TEXT NOT NULL,           -- rithmic | ibkr | cqg
    account_ref TEXT NOT NULL,           -- broker-side account ID
    type        TEXT NOT NULL,           -- prop | live | paper
    currency    TEXT NOT NULL DEFAULT 'USD',
    active      INTEGER NOT NULL DEFAULT 1,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### `trades`
```sql
CREATE TABLE trades (
    id              INTEGER PRIMARY KEY,
    account_id      INTEGER NOT NULL REFERENCES accounts(id),
    broker_trade_id TEXT,               -- dedup key from broker
    symbol          TEXT NOT NULL,
    asset_class     TEXT NOT NULL,      -- futures | stock | option
    direction       TEXT NOT NULL,      -- long | short
    qty             REAL NOT NULL,
    entry_price     REAL NOT NULL,
    exit_price      REAL,
    entry_time      DATETIME NOT NULL,
    exit_time       DATETIME,
    gross_pnl       REAL,
    commission      REAL,
    net_pnl         REAL,
    status          TEXT NOT NULL DEFAULT 'open',  -- open | closed
    tags            TEXT,               -- JSON array of strings
    setup           TEXT,               -- setup label e.g. "ORB", "VWAP Reclaim"
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(account_id, broker_trade_id)
);
```

### `executions`
```sql
CREATE TABLE executions (
    id              INTEGER PRIMARY KEY,
    trade_id        INTEGER REFERENCES trades(id),
    account_id      INTEGER NOT NULL REFERENCES accounts(id),
    broker_exec_id  TEXT,
    symbol          TEXT NOT NULL,
    side            TEXT NOT NULL,      -- buy | sell
    qty             REAL NOT NULL,
    price           REAL NOT NULL,
    commission      REAL,
    exec_time       DATETIME NOT NULL,
    raw_data        TEXT                -- JSON blob of original broker payload
);
```

### `journal_entries`
```sql
CREATE TABLE journal_entries (
    id          INTEGER PRIMARY KEY,
    trade_id    INTEGER REFERENCES trades(id),  -- NULL = day-level entry
    account_id  INTEGER REFERENCES accounts(id),
    entry_date  DATE NOT NULL,
    body        TEXT NOT NULL,          -- markdown text
    mood        INTEGER,                -- 1-5 scale (optional)
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### `daily_stats` (materialised cache, rebuilt each sync)
```sql
CREATE TABLE daily_stats (
    id              INTEGER PRIMARY KEY,
    account_id      INTEGER NOT NULL REFERENCES accounts(id),
    stat_date       DATE NOT NULL,
    trades_count    INTEGER NOT NULL DEFAULT 0,
    wins            INTEGER NOT NULL DEFAULT 0,
    losses          INTEGER NOT NULL DEFAULT 0,
    gross_pnl       REAL NOT NULL DEFAULT 0,
    net_pnl         REAL NOT NULL DEFAULT 0,
    commission      REAL NOT NULL DEFAULT 0,
    largest_win     REAL,
    largest_loss    REAL,
    UNIQUE(account_id, stat_date)
);
```

### `sync_log`
```sql
CREATE TABLE sync_log (
    id          INTEGER PRIMARY KEY,
    account_id  INTEGER NOT NULL REFERENCES accounts(id),
    sync_type   TEXT NOT NULL,          -- rithmic | ibkr | csv
    status      TEXT NOT NULL,          -- success | error | partial
    trades_added INTEGER NOT NULL DEFAULT 0,
    message     TEXT,
    synced_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

---

## 5. Data Sync Modules

### 5.1 Rithmic (Apex, Lucid)

Rithmic exposes R|Protocol — a binary protocol over TCP/SSL. Third-party Go or C libraries exist.

**Recommended approach:** Use the Rithmic R|Protocol Go SDK or wrap the official C++ library via CGO. Alternatively, connect via the **Rithmic Web API** (REST/WebSocket) if the account tier supports it — simpler and preferred.

**Implementation:**
```
internal/sync/rithmic/
    client.go       -- connection management, auth, reconnect logic
    poller.go       -- background goroutine, polls fills every N seconds
    mapper.go       -- maps Rithmic fill objects → internal Execution structs
```

**Fields to capture per fill:** account ID, order ID, fill ID, symbol, buy/sell, quantity, fill price, fill time, commission, exchange.

**Dedup strategy:** `broker_exec_id` = Rithmic fill ID. `INSERT OR IGNORE` on conflict.

### 5.2 IBKR (Interactive Brokers)

Two API options:
- **TWS API** (socket-based, requires Trader Workstation running locally) — real-time, well-documented
- **Client Portal API** (REST, web-based) — easier to self-host, no TWS dependency

**Recommended:** Client Portal API (port 5000, runs as a local gateway process). IBGW will be listening on ibgw.example.internal:4005 Optionally, another GW might be running on different port. 

```
internal/sync/ibkr/
    client.go       -- HTTP client wrapping Client Portal API
    poller.go       -- polls /v1/api/iserver/account/trades on schedule
    mapper.go       -- maps IBKR trade objects → internal structs
```

**Multi-account:** IBKR Client Portal supports switching between sub-accounts. Iterate all configured account IDs per sync cycle. 

**Dedup:** `broker_exec_id` = IBKR execution ID (`execId` field).

### 5.3 CQG / AMP (CSV Import)

CQG Desktop exports a fill report CSV. No live API integration.

**Implementation:**
```
internal/sync/cqg/
    watcher.go      -- fs.Watcher on a configured drop folder path
    parser.go       -- parses CQG CSV format into Execution structs
    mapper.go       -- maps to internal structs
```

**Workflow:** User exports CSV from CQG Desktop → saves to a watched folder on the server → app auto-ingests within seconds. Alternatively, expose a `/api/import/cqg` endpoint accepting file upload via the UI.

**CQG CSV columns:** Account, Underlying, Symbol, Time, Action (SLD/BOT), Quantity, Avg Price — map to internal schema accordingly.


### 5.4 Topstep

Topstepx platform is used. Consider options to read trading data without API. 

---

## 6. Application Pages & Features

### 6.1 Dashboard (`/`)

Topstep-style session overview. Updates via HTMX polling every 30s during market hours.

**Widgets:**
- Account selector (all accounts or individual)
- Today's P&L (gross + net, colour-coded)
- Trades today: count, win/loss split, win rate %
- Best trade / worst trade of the day
- Long vs short % breakdown
- Running equity curve (intraday, Chart.js or uPlot via CDN)
- Open positions (live, if broker supports it)
- Last sync timestamp per broker

### 6.2 P&L Calendar (`/calendar`)

Monthly grid, one cell per trading day. Colour intensity by net P&L magnitude.

- Click any day → drill-down showing trades, executions, journal entry for that day
- Month navigation
- Account filter
- Summary row: monthly total, win days, loss days

### 6.3 Trade Log (`/trades`)

Paginated, sortable, filterable table of all closed trades.

**Columns:** Date/Time, Account, Symbol, Direction, Qty, Entry, Exit, Gross P&L, Commission, Net P&L, Setup, Tags

**Filters:** Account, Date range, Symbol, Asset class, Direction, Setup tag, Win/loss

**Trade detail modal:** Full execution breakdown, inline journal note editor, tag editor

### 6.4 Journal (`/journal`)

Day-by-day diary. Each entry is markdown text.

- Day view: date header, linked trades for that day, journal body (rendered markdown)
- Edit mode: textarea with markdown, mood rating (1-5 emojis), save button
- Entry can be linked to a specific trade or free-form (day-level)
- Search across all journal entries

### 6.5 Analytics (`/analytics`)

Aggregated stats over a configurable date range and account filter.

**Metrics:**
- Win rate, profit factor, expectancy (avg win × win rate − avg loss × loss rate)
- Average win / average loss / largest win / largest loss
- Average hold time (winners vs losers)
- Performance by: time of day, day of week, symbol, setup tag, direction
- Drawdown: max drawdown, current drawdown from peak
- Equity curve (cumulative net P&L over time)
- Commission drag total

Charts: use uPlot (lightweight, fast) served from CDN.

### 6.6 Accounts (`/accounts`)

CRUD for account configuration. Per-account: name, broker type, credentials (stored encrypted), sync schedule toggle.

### 6.7 Import (`/import`)

Manual CSV upload endpoint for CQG/AMP. Drag-and-drop file zone, broker format selector, preview before commit, import result summary.

### 6.8 Settings (`/settings`)

- Market hours configuration (for dashboard "active session" detection)
- Sync schedule (cron expression per broker)
- Tag/setup label management
- Data export (full SQLite dump or CSV)
- Timezone setting

---

## 7. API Endpoints

All returning JSON, used internally by HTMX partials and optionally by future mobile clients.

```
GET  /api/dashboard/summary          -- today's stats across accounts
GET  /api/trades?page=&filter=       -- paginated trade list
GET  /api/trades/:id                 -- single trade detail + executions
GET  /api/calendar?month=&account=   -- monthly P&L grid data
GET  /api/analytics?from=&to=&acct=  -- aggregated analytics
GET  /api/journal?date=              -- journal entry for a date
POST /api/journal                    -- create/update journal entry
POST /api/trades/:id/tags            -- update trade tags
POST /api/sync/trigger/:account_id   -- manual sync trigger
POST /api/import/csv                 -- CSV file upload
GET  /api/sync/status                -- last sync times and status per account
```

---

## 8. Project Structure

```
trading-journal/
├── cmd/
│   └── server/
│       └── main.go              -- entrypoint, config loading, server start
├── internal/
│   ├── config/
│   │   └── config.go            -- env/YAML config struct
│   ├── db/
│   │   ├── migrations/          -- .sql migration files
│   │   ├── queries/             -- .sql files for sqlc
│   │   └── db.go                -- DB init, connection pool
│   ├── sync/
│   │   ├── rithmic/
│   │   │   ├── client.go
│   │   │   ├── poller.go
│   │   │   └── mapper.go
│   │   ├── ibkr/
│   │   │   ├── client.go
│   │   │   ├── poller.go
│   │   │   └── mapper.go
│   │   └── cqg/
│   │       ├── watcher.go
│   │       ├── parser.go
│   │       └── mapper.go
│   ├── handlers/
│   │   ├── dashboard.go
│   │   ├── trades.go
│   │   ├── journal.go
│   │   ├── analytics.go
│   │   ├── calendar.go
│   │   ├── accounts.go
│   │   └── import.go
│   ├── models/
│   │   └── models.go            -- domain structs (generated by sqlc + hand-written)
│   ├── analytics/
│   │   └── calculator.go        -- P&L, win rate, drawdown, expectancy logic
│   └── scheduler/
│       └── scheduler.go         -- cron job wiring for sync workers
├── web/
│   ├── templates/
│   │   ├── base.html            -- layout with nav
│   │   ├── dashboard.html
│   │   ├── trades.html
│   │   ├── journal.html
│   │   ├── analytics.html
│   │   ├── calendar.html
│   │   └── partials/            -- HTMX partial templates
│   └── static/
│       ├── css/                 -- Tailwind output (build step)
│       └── js/                  -- uPlot or Chart.js from CDN (minimal local JS)
├── docker/
│   └── Dockerfile
├── docker-compose.yml
├── sqlc.yaml
├── go.mod
├── go.sum
└── README.md
```

---

## 9. Configuration

Loaded from environment variables or a `config.yaml` file:

```yaml
server:
  port: 8080
  timezone: "America/Chicago"   # trading timezone

database:
  path: "./data/trading.db"

market_hours:
  futures:
    open: "17:00"               # Sunday-Friday 5pm CT
    close: "16:00"              # following day 4pm CT
  stocks:
    open: "08:30"
    close: "15:00"

brokers:
  rithmic:
    - account_id: 1
      username: ""
      password: ""              # stored encrypted at rest
      server_env: "paper"       # paper | live
      poll_interval: 30s
  ibkr:
    - account_id: 2
      gateway_url: "https://localhost:5000"
      ibkr_account: "U1234567"
      poll_interval: 60s
    - account_id: 3
      gateway_url: "https://localhost:5000"
      ibkr_account: "U7654321"
      poll_interval: 60s
  cqg:
    - account_id: 4
      watch_folder: "./imports/cqg"
```

Sensitive credentials (passwords, API keys) should be stored encrypted in the database, with the encryption key provided via environment variable `APP_SECRET_KEY`. Do not store plaintext credentials in config files committed to source control.

---

## 10. Deployment

### Docker Compose (recommended)

```yaml
version: "3.9"
services:
  trading-journal:
    build: .
    ports:
      - "8080:8080"
    volumes:
      - ./data:/app/data           # SQLite database persistence
      - ./imports:/app/imports     # CQG CSV drop folder
    environment:
      - APP_SECRET_KEY=${APP_SECRET_KEY}
      - CONFIG_PATH=/app/config.yaml
    restart: unless-stopped
```

### Dockerfile

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o trading-journal ./cmd/server

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/trading-journal .
COPY --from=builder /app/web ./web
EXPOSE 8080
CMD ["./trading-journal"]
```

Access via browser at `http://<server-ip>:8080`. No authentication layer in v1 (assumed private network / VPN). Add basic auth or OAuth in v2 if remote access is needed.

---

## 11. Build Order / Implementation Phases

### Phase 1 — Core foundation
1. Project scaffolding, go.mod, directory structure
2. SQLite setup with golang-migrate, all schema migrations
3. sqlc setup and query generation
4. Config loading (env + YAML)
5. Basic HTTP server with chi router
6. Base HTML template + Tailwind CSS build

### Phase 2 — Manual import & trade log
1. CQG CSV parser and importer
2. Manual trade entry (fallback form)
3. Trade log page with filtering and pagination
4. Trade detail modal

### Phase 3 — IBKR sync
1. IBKR Client Portal API client
2. Background poller with dedup
3. Multi-account support
4. Sync status UI

### Phase 4 — Rithmic sync
1. Rithmic API client (evaluate SDK options, may use REST gateway if available)
2. Background poller for Apex, Lucid, TopStep accounts
3. Dedup and error handling

### Phase 5 — Dashboard & analytics
1. Dashboard page with HTMX live polling
2. P&L calendar
3. Analytics page with all metrics
4. Chart integration (uPlot)

### Phase 6 — Journal
1. Journal entry CRUD
2. Markdown rendering (server-side with `gomarkdown` or `blackfriday`)
3. Trade-to-journal linking
4. Journal search

### Phase 7 — Polish
1. Sync log and error visibility
2. Data export (CSV, SQLite dump)
3. Settings page
4. Docker packaging and documentation

---

## 12. Key Technical Decisions & Notes for Claude Code

- **No ORM.** Use `sqlc` for type-safe queries from raw SQL. Keep SQL readable and auditable.
- **No SPA framework.** Server-rendered HTML with HTMX for dynamic updates. Keep JS minimal.
- **SQLite is sufficient.** Single-user app, tens of thousands of trades at most. No need for Postgres.
- **Rithmic integration is the highest-risk item.** R|Protocol is a proprietary binary protocol. Evaluate whether an unofficial Go library exists (search GitHub for `rithmic go`). If not, the fallback is: trigger manual Rithmic R|Trader export to the CQG/CSV drop folder using the same watcher pattern.
- **IBKR Client Portal Gateway** must be running as a separate process on the host. It's a JAR file provided by IBKR. Document the setup step clearly in README.
- **Timezone handling:** All times stored in UTC in the database. Convert to user's configured trading timezone for display only.
- **Futures P&L:** Calculate using tick size × tick value × quantity, not just price difference, for correct dollar P&L on futures contracts. Store a symbol metadata table mapping futures symbols to their tick specifications.
- **Trade reconstruction:** Brokers provide executions (fills), not trades. A trade = one or more fills that open then close a position. Implement a position builder that groups fills into trades using FIFO matching.
