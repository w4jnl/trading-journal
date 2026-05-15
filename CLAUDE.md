# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

Greenfield project. The committed files are:

- `trading-journal-design.md` — original design spec (numbered §1–§12). Architecture, schema, and feature scope still apply unless overridden below.
- `exportData/MotivewaveExport.csv`, `exportData/TopstepxExport.csv` — real samples of the v1 data sources. Build and test importers against these.

No Go code, migrations, templates, or `go.mod` exist yet. Read the design doc and these export samples before scaffolding.

## v1 scope decisions (override the design doc)

The design doc proposes live broker APIs (Rithmic, IBKR Client Portal, CQG watcher) as Phases 3–4. **v1 ignores all of that** — the user already runs MotiveWave on every brokered account, so a single MotiveWave CSV importer covers Apex / Lucid / IBKR / AMP-CQG. TopStepX is handled by a second importer because it's not a MotiveWave-supported broker.

- **Data sources in v1:** CSV import only. MotiveWave export + TopStepX export. Two entry points, same `ImportService` code path:
  1. **UI upload** at `/import` (drag-drop or file picker).
  2. **Watch folder** — `fsnotify` on `imports/inbox/`. Drop a file there and it auto-imports.
  Format is auto-detected from the CSV header row (`Account,Ticket,…` → MotiveWave, `Id,AccountName,…` → TopstepX). After processing, files always move out of `inbox/`: success → `imports/processed/<timestamp>_<filename>`, failure → `imports/failed/<filename>` with a sibling `<filename>.error.txt`. No live polling, no broker credentials, no encrypted-at-rest secrets, no IBKR Gateway dependency, no Rithmic R|Protocol risk.
- **Live sync (§5.1, §5.2):** deferred to a later phase, gated on whether the manual flow becomes painful. When/if added, IBKR Client Portal first (gateway already runs at `ibgw.example.internal:4005`).
- **Refresh model:** end-of-day. No HTMX live polling on the dashboard in v1 — the data only changes when a CSV is imported.
- **Auth:** none. Private network / VPN. Don't add OAuth or basic auth.
- **Deployment:** no Docker initially. Plain `go build` + `./trading-journal`. Containerization is a later concern.

## Stack choices

- Go, stdlib `net/http` with `http.ServeMux` (Go 1.22+ method-based routing). **No chi.**
- `modernc.org/sqlite` (pure Go, CGO-free).
- `dbmate` for migrations. **Not** golang-migrate or goose — the design doc was inconsistent; dbmate wins.
- `sqlc` for type-safe queries from raw SQL in `internal/db/queries/`.
- `html/template` + HTMX, server-rendered. Charts: uPlot from CDN. No SPA framework.
- Tailwind CSS, prebuilt output committed to `web/static/css/`.
- Templates and static assets embedded via `embed.FS` so the binary is self-contained.
- Go style guide: `~/.claude/go-style.md`.

## Schema additions beyond §4

The design doc's tables stand, but these are required for correctness:

- **`accounts` two-axis broker split.** Replace the original single `broker` field with:
  - `firm` — `apex`/`lucid`/`topstep`/`amp`/`ibkr`. Drives commission lookup. May be NULL until user confirms via the accounts UI.
  - `import_source` — `motivewave_csv`/`topstepx_csv`. Drives parser dispatch.
  - `account_ref` — raw account string from the CSV (e.g. `APEX-100001-28`, `LFE050-…`, `EXPRESS-V2-…`).
  - Unique constraint: `(import_source, account_ref)`.
- **`instruments`** — `symbol_root PK, exchange?, tick_size?, tick_value?, multiplier?, currency, needs_setup`. Tick specs are **nullable** so the importer can auto-create a stub row for any unknown `symbol_root` (e.g. a new MGC/GC) and continue ingesting executions. Stubs have `needs_setup=1` and NULL tick specs; user fills them via the instruments UI later. Seeded via migration with full specs for ES, MES, NQ, MNQ, CL, MCL.
- **`instrument_aliases`** — `broker, broker_symbol, instrument_id`. Empty in v1 (MotiveWave/TopstepX symbols are uniform), but the indirection means CQG/Rithmic/IBKR symbol-format differences are a data change, not a schema change, when live sync arrives.
- **`commission_rates`** — `firm, symbol_root, per_contract_rt`. Round-turn rate (covers both legs of a trade). Position builder applies `rate × qty` once at trade close. Seeded with user-provided rates for Apex / Lucid / TopStep / AMP / IBKR.

### Firm guessing rules (used by importer when auto-creating accounts)

- `APEX-…` → apex
- `LFE…` / `LFF…` → lucid
- TopstepX export (always, regardless of `AccountName` shape) → topstep
- `U######` (IBKR convention) → ibkr
- MotiveWave Source column contains "AMP" (e.g. `AMP Global Clearing - CQG`) → amp. AMP account_refs in MotiveWave exports are bare numeric IDs like `120042` with no prefix, so the Source field is the only reliable hint.
- Anything else: NULL + `needs_setup=1`.

### Symbol aliasing

CQG (and the AMP-CQG MotiveWave feed) uses `EP` for the E-mini S&P 500; everywhere else uses `ES`. The MotiveWave parser canonicalizes `EP` → `ES` at parse time when the Source column contains "CQG" or "AMP" (see `CanonicalizeSymbolRoot` in `internal/importer/types.go`). The contract code in `Symbol` (e.g. `EPM26` vs `ESM26`) is left as-is — only the root used for joins to `instruments` and `commission_rates` is normalized. If a second alias case appears, move this to the `instrument_aliases` DB table.

## Import format facts (verified against samples)

### MotiveWave CSV
- Columns: `Account, Ticket, Underlying, Symbol, Exchange, Time, Action, Quantity, Price, Source`
- One row = one fill (execution-level).
- **One file contains multiple accounts** (`APEX-100001-28`, `LFE050-…`, `LFF050-…`). Importer must route each row to the matching internal account.
- Dedup key: `(account_id, Ticket)` — `INSERT OR IGNORE`.
- `Action`: `BOT` = buy, `SLD` = sell.
- Time format: `MM/DD/YYYY HH:MM:SS.fff±HHMM` (offset present, parse to UTC).
- `Source` field (`Rithmic` vs `Rithmic WS`) does not produce duplicate rows — ignore for dedup.
- **No commission column.**

### TopStepX CSV
- **TopstepX exports include a UTF-8 BOM** (`EF BB BF`) at the start of the file; MotiveWave does not. `encoding/csv` doesn't strip it, so `stripBOMReader` in `internal/importer/types.go` does. Forget this and `DetectFormat` won't recognize the file and `ParseTopstepX` will report `missing column "Id"`.
- This is **order-level, not fill-level**. Importer must filter `Status = Filled` and treat each filled order as one execution.
- Columns of interest: `Id, AccountName, ContractName, Status, Size, Side, FilledAt, ExecutePrice, PositionDisposition`.
- Dedup key: `(account_id, Id)`.
- `Side` mapping: `Ask` = BUY, `Bid` = SELL. Verified against `PositionDisposition`/`CreationDisposition` (Opening+Trader rows are Ask; Closing+StopLoss on a long position are Bid).
- Use `ExecutePrice` as fill price and `FilledAt` as fill time. Ignore `LimitPrice`/`StopPrice`/`TriggeredPrice` for execution records.
- One file = one account.
- **No commission column.**
- TopStepX has its own API (not Rithmic-backed despite the prop firm using Rithmic for some plans). Future automation would use the TopstepX API directly.

## Project layout (revised from §8)

Replace `internal/sync/{rithmic,ibkr,cqg}/` with:

```
internal/importer/
    types.go            -- Fill struct, Format const, firm-guessing rules
    service.go          -- ImportService: detect, dedup, account/instrument upsert, file-move
    watcher.go          -- fsnotify watcher on imports/inbox/, debounced, single-flight
    motivewave.go       -- ParseMotiveWave(io.Reader) ([]Fill, error)
    topstepx.go         -- ParseTopstepX(io.Reader) ([]Fill, error)
internal/positions/
    builder.go          -- FIFO fill-grouping into trades, commission allocation, position flips
```

Importer is a flat package — sub-packages would force a `Fill`-types-only base package to avoid an import cycle, which is overkill at this scope.

The watcher runs a boot-time sweep of `imports/inbox/` on startup so files left from a previous run get processed before normal watching begins. File-move semantics (success → `processed/`, failure → `failed/` + `.error.txt`) live in `ImportService` via a deferred outcome inspector — never silently lose a dropped file.

Position builder is correctness-critical: partial closes, position flips (long → short in one fill produces two trades), per-account per-symbol FIFO queues. Test against the real export samples before wiring it into the import flow.

## Architectural constraints to preserve

These are easy to violate by accident:

- **All times stored in UTC**, converted to the configured trading timezone at render time only.
- **Brokers/CSVs deliver executions, not trades.** `executions` is the system of record; `trades` is derived by the position builder.
- **Futures P&L** uses `tick_size × tick_value × qty`, not raw price difference. Always go through the `instruments` table. If `tick_size`/`tick_value` is NULL (stub instrument awaiting setup), leave `trades.gross_pnl` and `net_pnl` NULL — never silently substitute a price-difference calculation. Trades for stub instruments materialize for tracking but show "—" in P&L until the user configures the symbol; a recompute is triggered when the stub is filled in.
- **Importer auto-upserts instruments.** Before inserting executions, the importer does `INSERT INTO instruments (symbol_root, exchange, currency, needs_setup) VALUES (?, ?, 'USD', 1) ON CONFLICT(symbol_root) DO NOTHING` for each unique root in the file. This guarantees the FK never fails on unknown symbols.
- **Account auto-create:** when an importer sees an unknown `(import_source, account_ref)` pair, create an `accounts` row, guess `firm` from the account-string prefix, and mark `needs_setup=1` if anything is unresolved (firm couldn't be guessed, type unknown). User clears the flag via the accounts UI.
- **`daily_stats` is a cache**, rebuilt from `trades` after each import. Never the source of truth.

## sqlc gotcha: ASCII-only in `.sql` comments

sqlc 1.26 and 1.31 mis-account for multi-byte UTF-8 characters when computing query boundaries — a single em-dash (`—`, U+2014) inside a `--` comment in `internal/db/queries/*.sql` shifts the parser by 2 bytes, which silently truncates the *next* query in the file (last 1–2 chars get dropped, and the dropped bytes get prepended to the constant after that). Symptoms are runtime "incomplete input" / "syntax error" from modernc.org/sqlite.

Keep `.sql` comments ASCII-only. Use `--` (two hyphens) or `-` in place of em / en dashes, and avoid smart quotes / non-breaking spaces. The constraint is comments only — query bodies are fine, since they hit the SQLite parser directly. (Generated Go is unaffected for queries whose bodies and preceding comments are clean.)

## Migration tooling

Migration files use dbmate format (`-- migrate:up` / `-- migrate:down` markers) and live in `internal/db/migrations/*.sql`. They are embedded into the binary via `embed.FS` and applied at startup by a small runner in `internal/db/migrations.go` — no external `dbmate` binary required at runtime. The dbmate CLI (when installed) can be used as a dev tool for rollbacks since the file format matches.
