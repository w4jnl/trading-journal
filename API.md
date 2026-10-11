# HTTP API

A small, read-plus-append HTTP API over your own strategies, journal
entries, trades and daily P&L / trade stats, authenticated by a **per-user
API key**. It's designed to be LLM-friendly: a discovery index, a
machine-readable OpenAPI 3.1 spec, stable camelCase JSON, and append-only
note writes that can never destroy what you've already written.

Everything is scoped to the key's user. There is no anonymous access -
every endpoint, including the index and the OpenAPI spec, requires a key.

- **Base URL:** `/api/v1` (e.g. `https://YOUR_HOST/api/v1`)
- **Spec:** `GET /api/v1/openapi.json` (OpenAPI 3.1)
- **Index:** `GET /api/v1` (lists the endpoints + who you're authenticated as)

## Authentication

### Generating a key

Sign in to the web UI and open **Settings** (sidebar → Settings, or
`/settings`). Give the key a name and click **Generate key**. The full key
is shown **exactly once** - copy it immediately; only a salted hash is
stored, so it cannot be retrieved later. Lost it? Generate a new one and
revoke the old.

Keys look like `tj_` followed by a URL-safe base64 secret:

```
tj_0xNxEJKtQ8r3v...                # 256-bit random, ~46 chars total
```

You can keep several keys (one per agent/tool) and **revoke** any of them
independently from the same page. Revoking takes effect immediately.

### Presenting a key

Send the key on every request, either way:

```bash
# Preferred
curl -H "Authorization: Bearer tj_..." https://YOUR_HOST/api/v1

# Or
curl -H "X-API-Key: tj_..." https://YOUR_HOST/api/v1
```

Query-string keys are intentionally **not** accepted (they leak into
access logs and `Referer` headers).

### When auth fails

Failures return `401` with a JSON body `{"error": "..."}`:

| Situation | `error` |
|---|---|
| No key, or it doesn't start with `tj_` | `missing or malformed API key` |
| Unknown or revoked key | `invalid API key` |
| The key's user has been deactivated | `account disabled` |

## Conventions

- **JSON, camelCase.** Request and response bodies are `application/json`
  with camelCase keys. (Field names are stable and never expose internal
  snake_case columns.)
- **Errors** are always `{"error": "<message>"}` with a `4xx`/`5xx` status.
- **Single resources** are returned as a bare object. **Lists** are
  `{"items": [...]}` plus a `page` object - offset-based on the journal
  list (`{"limit", "offset", "count"}`), page-based on the trades list
  (`{"page", "pageSize", "pages", "total"}`).
- **Date ranges.** `from` / `to` are `YYYY-MM-DD` calendar dates in the
  trading timezone, **inclusive** on both ends, and the stats and trades
  endpoints also accept the keywords **`today`**, **`yesterday`** and
  **`last`** (the last trading day before today), resolved server-side in
  the trading timezone - so a client (or a model) never has to know today's
  date in that zone. `day=X` is shorthand for `from=X&to=X`. The stats
  endpoint rejects a malformed or inverted range with `400`; the list
  endpoints ignore junk dates.
- **`refresh=1`** on the stats and trades endpoints pulls the latest fills
  from the brokers first and waits for them (bounded, ~15 s) before
  answering - use it for intraday "how am I doing today" reads. Without it a
  read still nudges the sync, but answers from the data on hand.
- **Account filter.** `filter=acc:<id>` and `filter=type:<slug>` (slugs
  `live`, `prop_live`, `prop_funded`, `prop_eval`, `paper`) are repeatable
  and OR-combined; the legacy singular `account_id=<id>` still works.
  Retired accounts never count in stats; accounts hidden in Settings
  (*excluded from stats*) are dropped from default stats **and from the
  default trades list** unless an explicit filter names them.
- **`null` means absent, never zero** - a P&L withheld for a stub
  instrument, a win rate with no decided trades, a largest win on a day
  with no winners. Render it as "—".
- **Reads keep the data fresh.** Fetching `/version`, `/stats/daily` or the
  trades endpoints nudges the broker syncs (TopstepX, IBKR) for your user,
  the same way opening a page does - throttled to at most one sync per
  `api_sync_throttle_sec` (default 60 s; `0` disables). The sync is
  asynchronous: the response you get is what is there now, and new fills
  change `/version` a few seconds later, so the poll-then-refetch loop below
  picks them up.
- **Ownership.** You only ever see your own data. A request for an id you
  don't own returns `404` (not `403`) - the resource simply doesn't exist
  for you.
- **Times.** Timestamp fields (`createdAt`, `updatedAt`, `entryTime`,
  `exitTime`) are RFC 3339. Calendar `date` fields and the appended-note
  heading are formatted in the server's configured **trading timezone**;
  everything is stored in UTC underneath.
- **Markdown + images.** Strategy descriptions and journal notes are
  markdown. Embedded images are rewritten to API URLs you fetch with the
  same key - see [Embedded images](#embedded-images).

## Discovery (for agents)

Two endpoints exist so a tool can bootstrap itself without hard-coding the
surface:

- `GET /api/v1` - a human/agent index: the service name, the user you're
  authenticated as, and the endpoint list.
- `GET /api/v1/openapi.json` - the full OpenAPI 3.1 document (schemas,
  parameters, security schemes). Both are behind key auth.

```jsonc
// GET /api/v1
{
  "service": "trading-journal-api",
  "version": "v1",
  "authenticatedAs": { "id": 1, "username": "admin" },
  "endpoints": [
    { "method": "GET",  "path": "/api/v1/strategies",            "summary": "List strategies" },
    { "method": "POST", "path": "/api/v1/journal/{id}/notes",    "summary": "Append a dated note (append-only)" }
    // ...
  ],
  "openapi": "/api/v1/openapi.json"
}
```

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET`  | `/api/v1` | Discovery index |
| `GET`  | `/api/v1/openapi.json` | OpenAPI 3.1 spec |
| `GET`  | `/api/v1/version` | Data version for change detection |
| `GET`  | `/api/v1/stats/daily` | Daily P&L + trade stats with range totals |
| `GET`  | `/api/v1/analytics/summary` | The analytics page for a range as one object (totals, R, drawdown, streaks, breakdowns) |
| `GET`  | `/api/v1/strategies` | List your strategies |
| `GET`  | `/api/v1/strategies/{id}` | One strategy |
| `GET`  | `/api/v1/journal` | List journal entries (summary rows) |
| `POST` | `/api/v1/journal` | Create an entry (day/week/trade/group) |
| `POST` | `/api/v1/journal/prepare-day` | Cluster a day's trades + create the entries to analyze |
| `GET`  | `/api/v1/journal/{id}` | One journal entry (full detail) |
| `POST` | `/api/v1/journal/{id}/notes` | Append a dated note (append-only) |
| `POST` | `/api/v1/journal/{id}/strategy` | Assign a strategy (only if the entry has none) |
| `POST` | `/api/v1/journal/{id}/tag` | Set a tag (only if the entry has none) |
| `POST` | `/api/v1/journal/{id}/labels` | Add mistake / emotion labels (additive) |
| `GET`  | `/api/v1/journal/{id}/chart.png` | Server-rendered chart image |
| `GET`  | `/api/v1/trades` | List trades (filtered, paged) |
| `GET`  | `/api/v1/trades/{id}` | One trade (fills + chart) |
| `GET`  | `/api/v1/trades/{id}/chart.png` | Server-rendered trade chart image |
| `GET`  | `/api/v1/attachments/{id}` | Fetch an embedded image |

### Strategies

A strategy is a short name plus a full markdown description (which may
embed images).

`GET /api/v1/strategies` -> `{"items": [Strategy, ...]}`
`GET /api/v1/strategies/{id}` -> `Strategy` (or `404`)

```jsonc
// Strategy
{
  "id": 3,
  "name": "ORB fade",
  "body": "## Opening-range fade\n\nWait for...\n\n![chart](/api/v1/attachments/12)",
  "attachments": [ { "id": 12, "url": "/api/v1/attachments/12" } ],
  "createdAt": "2026-05-02T09:14:00Z",
  "updatedAt": "2026-05-20T18:31:00Z"
}
```

`body` is the complete description with image references rewritten to
`/api/v1/attachments/{id}`; `attachments` lists those same images.

### Journal list

`GET /api/v1/journal` returns summary rows, newest first, with paging.

Query parameters (all optional):

| Param | Type | Notes |
|---|---|---|
| `scope` | `day` \| `week` \| `trade` \| `group` | Repeatable (e.g. `?scope=day&scope=week`) |
| `from` | `YYYY-MM-DD` | Inclusive lower bound on entry date |
| `to` | `YYYY-MM-DD` | Inclusive upper bound |
| `tag` | `good` \| `neutral` \| `bad` \| `untagged` | |
| `strategy` | strategy id, or `none` | Filter by assigned strategy |
| `mistake` | label key, or `none` | Repeatable; entries carrying any of the picked mistakes (`none` = no mistake logged) |
| `emotion` | label key | Repeatable; entries carrying any of the picked emotions |
| `limit` | integer | Default `50`, max `200` |
| `offset` | integer | Default `0` |

```jsonc
// GET /api/v1/journal?scope=day&limit=3
{
  "items": [
    {
      "id": 8,
      "scope": "day",
      "date": "2026-05-18",
      "tag": "good",
      "mood": null,
      "strategy": null,                       // assigned strategy's short name, or null
      "snippet": "Good day overall. Could have taken bigger short swings...",
      "summary": { "trades": 57, "net": 688.81, "gross": 836.25, "commission": 147.44, "wins": 51, "losses": 6 },
      "links": { "self": "/api/v1/journal/8", "chartPng": "/api/v1/journal/8/chart.png" }
    }
  ],
  "page": { "limit": 3, "offset": 0, "count": 1 }
}
```

### Journal detail

`GET /api/v1/journal/{id}` -> full entry, or `404` if you don't own it.

```jsonc
{
  "id": 8,
  "scope": "day",
  "date": "2026-05-18",
  "tag": "good",
  "mood": null,
  "notes": "Good day overall...\n\n![](/api/v1/attachments/1)",  // markdown, image refs rewritten
  "strategy": { "id": 3, "name": "ORB fade" },                    // short name ONLY (never the body), or null
  "summary": { "trades": 57, "net": 688.81, "gross": 836.25, "commission": 147.44, "wins": 51, "losses": 6 },
  "trades": [ Trade, ... ],
  "attachments": [ { "id": 1, "url": "/api/v1/attachments/1" } ],
  "chart": ChartEnvelope,        // present only when market-data/charting is enabled
  "chartPng": "/api/v1/journal/8/chart.png",
  "createdAt": "2026-05-18T22:05:00Z",
  "updatedAt": "2026-06-06T21:19:00Z"
}
```

The assigned `strategy` is deliberately reduced to `{id, name}` - the full
strategy description is never inlined here. Fetch
`/api/v1/strategies/{id}` if you want it.

```jsonc
// Trade
{
  "id": 4412,
  "symbol": "MESM6",
  "symbolRoot": "MES",
  "direction": "long",            // long | short
  "qty": 3,
  "account": "APEX-100001-28",
  "entryTime": "2026-05-18T14:32:11Z",
  "exitTime": "2026-05-18T14:48:02Z",  // null if still open
  "grossPnl": 37.5,               // null for stub instruments awaiting tick setup
  "commission": 4.2,              // null for stub instruments
  "netPnl": 33.3                  // null for stub instruments
}
```

> P&L fields are `null` until the trade's instrument has tick size/value
> configured (see the instruments UI). The trade still appears; only its
> money columns are withheld rather than guessed.

#### The `chart` field

Present only when charting is enabled on the server. It mirrors the data
the web UI's trade-replay charts use - one panel per contract:

```jsonc
// ChartEnvelope
{
  "status": "ok",                 // ok | needs_load | no_data
  "panels": [
    {
      "contract": "MESM6",
      "symbolRoot": "MES",
      "timeframe": "15m",         // display bar size
      "barTimeframe": "15m",      // granularity of the bars array
      "timeframes": ["1m","5m","15m","1h","1d"],
      "direction": "long",
      "from": 1747526400,         // unix seconds
      "to": 1747612800,
      "entryAvg": 7420.25,
      "exitAvg": 7423.50,
      "bars": [ { "t": 1747526400, "o": 7410, "h": 7415, "l": 7408, "c": 7412, "v": 1200 } ],
      "markers": [ { "t": 1747527000, "price": 7420.25, "side": "buy", "role": "entry", "qty": 3 } ]
    }
  ]
}
```

The API serves **cached** bars only and never fetches from the upstream
data vendor. If `status` is `needs_load` or `no_data`, open the entry's
chart once in the web UI to populate the cache, then re-request.

### Append a note (append-only)

`POST /api/v1/journal/{id}/notes`

Adds a dated paragraph to the **bottom** of an existing entry's notes. It
**never** overwrites or removes existing content - that's the whole point,
so an agent can safely add observations.

Request body (max 1 MiB):

```json
{ "note": "Reviewed the 14:32 long: entry was late vs the plan." }
```

Success -> `200`:

```json
{ "id": 8, "appended": true }
```

The server appends a block of the form:

```
<existing notes...>

---

_Note added 2026-06-06 16:19 CDT_

Reviewed the 14:32 long: entry was late vs the plan.
```

The timestamp is in the server's trading timezone.

Errors:

| Status | When |
|---|---|
| `400` | Body isn't `{"note": "..."}`, or the note is empty/whitespace |
| `404` | Entry doesn't exist or you don't own it - this endpoint only appends to an existing entry; create one with `POST /api/v1/journal` |

### Creating journal entries

`POST /api/v1/journal` creates one entry. A single `scope` field selects the
kind; the rest of the body depends on it. On success you get **`201`** with the
new entry in the exact shape `GET /api/v1/journal/{id}` returns, plus a
`Location` header.

| `scope` | Required | Optional | Keyed by (uniqueness) |
|---|---|---|---|
| `day` | `date` (`YYYY-MM-DD`), `body` | `tag`, `mood` (1-5), `strategyId` | one per date |
| `week` | `week` (`YYYY-Www`), `body` | `tag`, `strategyId` | one per ISO week |
| `trade` | `tradeId`, `body` | `tag`, `strategyId` | one per trade |
| `group` | `tradeIds` (≥1) | `body`, `tag`, `strategyId` | **none - create freely** |

```bash
# A day entry
curl -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"scope":"day","date":"2026-06-02","body":"Choppy open, sat out.","tag":"neutral"}' \
  "$HOST/api/v1/journal"

# A group entry over several trades (the group is DEFINED by these trades)
curl -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"scope":"group","tradeIds":[4412,4413,4418],"body":"NQ ramp scalps.","tag":"good"}' \
  "$HOST/api/v1/journal"
```

**Group entries** are the only multi-trade kind: you pass an explicit
`tradeIds` array and the server links each (after dropping any you don't own),
deriving the entry's date from the latest exit among them. Trades may belong to
many groups, so a group create always succeeds - there is no conflict check.

**Day / week / trade are singletons.** If one already exists, create returns
**`409`** with the existing id - it never overwrites:

```jsonc
// POST /api/v1/journal {"scope":"day","date":"2026-06-02", ...} when one exists
// -> 409
{ "error": "a day entry already exists for 2026-06-02", "entryId": 8 }
// then append instead:
// POST /api/v1/journal/8/notes  {"note":"..."}
```

Other errors are `400`: unknown `scope`, missing the scope's required field, a
future `date`, `mood` outside 1-5, a `tag` outside `good|neutral|bad`, an
unknown/foreign `strategyId` or `tradeId`, or (group) no owned trades. Editing an
existing entry's tag/mood/strategy and deleting entries are out of scope -
create new, or append notes.

### Prepare a day for review

`POST /api/v1/journal/prepare-day` - the daily-review entry point for
agents. Clusters the day's copytrader fan-out (same root + direction +
entry time within ~2 s across accounts) into **group** entries and the
solos into **trade** entries, creates the missing ones (idempotent - an
existing entry is reused, never duplicated), and returns them so the
caller analyzes by `entryId` and never handles raw trade ids.

Request body (optional): `{"date": "YYYY-MM-DD"}`; default = the last
trading day before today (skips weekends, holidays and no-trade days).

```jsonc
// 200
{
  "date": "2026-05-12",
  "entries": [
    { "entryId": 41, "scope": "group", "created": true, "alreadyAnalyzed": false,
      "label": "MES long 14:32", "symbolRoot": "MES", "direction": "long",
      "accounts": 3, "netPnl": 96.4, "tradeIds": [4412, 4413, 4418] },
    { "entryId": 42, "scope": "trade", "created": false, "alreadyAnalyzed": true,
      "label": "MNQ short 15:10", "symbolRoot": "MNQ", "direction": "short",
      "accounts": 1, "netPnl": -22.0, "tradeIds": [4420] }
  ],
  "note": "..."                        // informational (e.g. no trades that day); may be absent
}
```

`alreadyAnalyzed` reflects the analysis marker in the entry's notes, so a
re-run knows what to skip. `400` for a malformed or future date.

### Assign a strategy (only if unset)

`POST /api/v1/journal/{id}/strategy` with `{"strategyId": 3}` assigns a
strategy to a **trade or group** entry that has none yet. An existing
assignment is **never changed** - the response then says
`"assigned": false`. Day and week entries span mixed setups and are
rejected.

```json
{ "id": 41, "assigned": true }
```

`400` for an unknown/foreign entry or strategy, a missing `strategyId`, or
a day/week entry.

### Set a tag (only if unset)

`POST /api/v1/journal/{id}/tag` with `{"tag": "good"}` (`good` | `neutral` |
`bad`) tags an entry that is still untagged. An existing tag is **never
changed** (`"assigned": false` then).

```json
{ "id": 41, "assigned": true }
```

`400` for an unknown entry or a bad/missing tag.

### Add labels (mistakes / emotions)

`POST /api/v1/journal/{id}/labels` with
`{"mistakes": ["chased", "moved-stop"], "emotions": ["frustrated"]}` adds
review labels to an entry. Either array may be omitted. The call is
**additive**: labels already on the entry stay, duplicates are ignored, and
nothing is ever removed through the API (the trader does that in the UI).

Keys come from a fixed vocabulary, so breakdowns stay comparable over time:

| Kind | Keys |
|---|---|
| `mistakes` | `chased`, `early-exit`, `late-exit`, `oversized`, `no-stop`, `moved-stop`, `held-loser`, `revenge`, `fomo`, `hesitated`, `overtraded`, `against-plan` |
| `emotions` | `calm`, `focused`, `confident`, `anxious`, `greedy`, `frustrated`, `bored`, `tilted`, `rushed` |

Response is the entry's labels after the add:

```json
{ "id": 41, "labels": { "mistakes": ["chased", "moved-stop"], "emotions": ["frustrated"] } }
```

`400` for an unknown key or an empty body, `404` for an entry you don't own.
The same `labels` object is part of every journal detail response, and
`POST /api/v1/journal` accepts `mistakes` / `emotions` arrays on create.

### Chart image (PNG)

`GET /api/v1/journal/{id}/chart.png`

A server-rendered candlestick PNG (1000x500) of the entry's chart:
green/red candles, entry/exit markers, and dashed average-price lines.
Rendered from **cached** bars only - no network call.

| Param | Notes |
|---|---|
| `contract` | When an entry spans several contracts, pick one by contract code (`MESM6`) or symbol root (`MES`). Defaults to the first panel that has data. |

Returns `image/png` (`Cache-Control: no-store`), or `404`
`{"error": "no cached chart data; open the chart in the UI first"}` when
there's nothing cached or charting is disabled.

### Embedded images

`GET /api/v1/attachments/{id}` streams an embedded image's bytes with its
stored content type (`image/png`, `image/jpeg`, `image/gif`, `image/webp`).

You don't construct these URLs yourself - they arrive already rewritten in
strategy `body` and journal `notes`, and enumerated in each item's
`attachments` array. Fetch them with the same key.

For safety, an image is served over the API only when it's referenced by a
strategy or journal body **you own**; anything else is `404`.

### Trades list

`GET /api/v1/trades` returns closed trades, newest first, **page-based** (fixed
page size 50). Accounts hidden in Settings are left out by default - the same
rule as the daily stats; naming an account via `account_id` / `filter`
overrides it.

Query parameters (all optional):

| Param | Type | Notes |
|---|---|---|
| `symbol_root` | string | e.g. `MES` |
| `direction` | `long` \| `short` | |
| `outcome` | `win` \| `loss` \| `breakeven` | by net P&L sign |
| `risk` | `planned` \| `past-stop` | trades carrying a plan; or planned trades whose measured heat exceeded the planned risk |
| `from` | `YYYY-MM-DD` \| `today` \| `yesterday` \| `last` | inclusive lower bound on the exit day (trading timezone) |
| `to` | same | inclusive upper bound |
| `account_id` | integer | one account (`filter=acc:<id>` / `filter=type:<slug>` also work) |
| `page` | integer | 1-based; default `1` |
| `show_retired` | `1` | include trades on retired accounts |
| `refresh` | `1` | pull the latest broker fills first and wait for them (~15 s max) before listing |

```jsonc
// GET /api/v1/trades?symbol_root=MES&outcome=win&page=1
{
  "items": [
    {
      "id": 4412, "accountId": 7, "account": "APEX-100001-28", "firm": "apex",
      "symbol": "MESM6", "symbolRoot": "MES", "direction": "long", "qty": 3,
      "entryPrice": 7410.25, "exitPrice": 7423.50,
      "entryTime": "2026-05-18T14:32:11Z", "exitTime": "2026-05-18T14:48:02Z",
      "grossPnl": 37.5, "commission": 4.2, "netPnl": 33.3, "status": "closed",
      "retired": false, "hasJournal": true, "journalTag": "good", "groupCount": 1
    }
  ],
  "page": { "page": 1, "pageSize": 50, "pages": 3, "total": 124 }
}
```

P&L fields are `null` for stub instruments (same rule as journal trades).

### Trade detail

`GET /api/v1/trades/{id}` -> the trade plus its fills, the linked trade-scope
journal entry id (if any), how many group entries include it, and cached chart
data. `404` if you don't own it.

```jsonc
{
  "id": 4412, "accountId": 7, "account": "APEX-100001-28", "firm": "apex",
  "symbol": "MESM6", "symbolRoot": "MES", "direction": "long", "qty": 3,
  "entryPrice": 7410.25, "exitPrice": 7423.50,
  "entryTime": "2026-05-18T14:32:11Z", "exitTime": "2026-05-18T14:48:02Z",
  "grossPnl": 37.5, "commission": 4.2, "netPnl": 33.3, "status": "closed",
  "brokerTradeId": null,
  "hasJournal": true, "journalEntryId": 31, "journalTag": "good", "groupCount": 1,
  "executions": [
    { "time": "2026-05-18T14:32:11Z", "side": "buy",  "role": "entry", "qty": 3, "price": 7410.25 },
    { "time": "2026-05-18T14:48:02Z", "side": "sell", "role": "exit",  "qty": 3, "price": 7423.50 }
  ],
  "chart": ChartEnvelope,        // present only when charting is enabled
  "chartPng": "/api/v1/trades/4412/chart.png"
}
```

`executions` are the actual fills; `qty` is the portion attributed to this
trade. The `chart` field has the same shape as the journal
[`chart` field](#the-chart-field).

### Trade chart image (PNG)

`GET /api/v1/trades/{id}/chart.png` - same renderer and `?contract=` rule as the
journal chart image; `image/png` or `404` when nothing is cached.

### Daily stats

`GET /api/v1/stats/daily` - per trading day: trade count, winners/losers,
net/gross/commission, win rate and largest win/loss, plus totals over the
range. These are **the Calendar's numbers**: the same cache (`daily_stats`,
rebuilt after every import/recompute), the same account rule, the same day
bucketing. Built for consumer apps and dashboards - use it instead of
summing `/api/v1/trades` yourself. The contract-first brief for building
such an app is `docs/api-handoff-001-daily-stats.md`.

Query parameters (all optional):

| Param | Type | Notes |
|---|---|---|
| `from` | `YYYY-MM-DD` \| `today` \| `yesterday` \| `last` | First day, inclusive (trading timezone). Default: `to` minus 29 days |
| `to` | same | Last day, inclusive. Default: today |
| `day` | same | Shorthand for `from=to=day` - `day=today` is the intraday check |
| `filter` | `acc:<id>` \| `type:<slug>` | Repeatable; an explicit filter overrides *excluded from stats* |
| `account_id` | integer | Legacy alias for one `filter=acc:<id>` |
| `refresh` | `1` | Pull the latest broker fills first and wait for them (~15 s max) before computing |

At most **400 days** per request.

```jsonc
// GET /api/v1/stats/daily?from=2026-08-03&to=2026-09-01
{
  "from": "2026-08-03", "to": "2026-09-01",   // effective range, echoed even when defaulted
  "timezone": "America/Chicago",              // every "date" is a calendar day in this zone
  "totals": {
    "trades": 41, "wins": 24, "losses": 15,
    "net": 1834.5, "gross": 2011.0, "commission": 176.5,
    "winRate": 0.615,                         // wins / (wins + losses); null when both are 0
    "winDays": 12, "lossDays": 6,             // days with net > 0 / net < 0
    "bestDay":  { "date": "2026-08-14", "net": 612.0 },   // null when items is empty
    "worstDay": { "date": "2026-08-21", "net": -388.0 },
    "largestWin": 402.5, "largestLoss": -210.0            // best/worst single trade; null when none
  },
  "items": [                                  // one per day WITH closed trades, ascending; gap days omitted
    { "date": "2026-08-03", "trades": 3, "wins": 2, "losses": 1,
      "net": 120.0, "gross": 132.5, "commission": 12.5,
      "winRate": 0.667, "largestWin": 95.0, "largestLoss": -40.0 }
  ]
}
```

Semantics:

- A trade belongs to the day of its **exit** (last closing fill), in the
  trading timezone. Closed trades only.
- `wins` = net > 0, `losses` = net < 0. Breakeven trades and trades whose
  instrument has no tick spec yet count in `trades` but in neither bucket
  and add 0 to the money totals, so `wins + losses <= trades`.
- Retired accounts never count; *excluded from stats* accounts are dropped
  unless named by `filter=`. An explicit filter that matches no account
  yields an empty result, never everything.
- An empty range is a normal `200`: `"items": []`, zeroed totals, the
  nullable fields `null`.

Errors: `400` for a malformed `from`/`to`, `to` before `from`, or a span
over 400 days (the message says which).

### Analytics summary

`GET /api/v1/analytics/summary` - everything the **Analytics** page shows
for a date range and account filter, as one object: totals (trades, win
rate, net/gross/commission, average win and loss, payoff ratio, profit
factor, expectancy), **R** over the trades that carry a planned risk,
**drawdown** (deepest peak-to-trough fall of cumulative net P&L, its
dates, days to recover, where the curve stands now), **streaks** (by trade
and by trading day) and **breakdowns** by symbol, direction, strategy,
tag, weekday and entry hour. Built for "how is my edge, where do I lose"
questions - use it instead of summing `/api/v1/trades` yourself. The MCP
tool `get_analytics_summary` returns the same object.

Query parameters are those of `/api/v1/stats/daily` (`from`, `to`, `day`,
`filter`, `account_id`, `refresh`), same defaults (last 30 days), same
400-day cap, same account rules.

```jsonc
// GET /api/v1/analytics/summary?from=2026-08-01&to=2026-08-31
{
  "from": "2026-08-01", "to": "2026-08-31", "timezone": "America/Chicago",
  "totals": {
    "trades": 41, "wins": 24, "losses": 15, "winRate": 0.615,
    "net": 1834.5, "gross": 2011.0, "commission": 176.5,
    "avgWin": 118.2, "avgLoss": -66.9,            // avgLoss is negative
    "payoffRatio": 1.77,                          // avgWin / |avgLoss|; null without losses
    "profitFactor": 2.83,                         // sum wins / sum losses; null without losses
    "expectancy": 44.7,                           // per trade, in money
    "largestWin": 402.5, "largestLoss": -210.0
  },
  "r": { "plannedTrades": 9, "avgR": 0.62, "winRate": 0.67, "avgWinR": 1.45, "avgLossR": -1.03 },
  "excursion": { "trades": 120, "exitEfficiency": 0.58, "avgMae": 41.2, "avgMfe": 96.5,   // MAE/MFE off cached bars
                 "winnersAvgMae": 28.0, "losersAvgMfe": 44.7 },
  "drawdown": {
    "maxDrawdown": 612.0,                         // positive magnitude
    "maxDrawdownPeakAt": "2026-08-11T20:01:00Z", "maxDrawdownTroughAt": "2026-08-14T19:12:00Z",
    "maxDrawdownRecoverAt": "2026-08-20T18:40:00Z",   // null while the fall is still open
    "maxDrawdownDays": 9, "currentDrawdown": 0, "underwater": false, "peakAt": "2026-08-29T20:00:00Z"
  },
  "streaks": { "longestWinTrades": 7, "longestLossTrades": 3, "currentTrades": 2,   // signed: +wins / -losses
               "longestGreenDays": 5, "longestRedDays": 2, "currentDays": 1 },
  "breakdowns": {
    "bySymbol":    [ { "key": "ES", "trades": 30, "wins": 18, "losses": 12, "net": 1500.0, "gross": 1620.0, "best": 402.5, "worst": -210.0 } ],
    "byDirection": [ /* long, short */ ],
    "byStrategy":  [ /* strategy names; "(no strategy)" for the rest */ ],
    "byTag":       [ /* good, neutral, bad, "(untagged)" */ ],
    "byMistake":   [ /* label names, e.g. "Moved stop"; "(none logged)" for the rest; a trade counts in each of its labels */ ],
    "byEmotion":   [ /* same for emotions */ ],
    "byDayOfWeek": [ /* Mon..Fri */ ],
    "byHour":      [ /* "09:00" .. by entry hour, trading timezone */ ]
  }
}
```

Semantics: trades belong to the day of their exit; `r.*` is meaningless
when `plannedTrades` is 0 (plans are set on the trade sheet); a strategy
or tag reaches a trade through its own entry or a group entry it is in, and
a label also through the day entry of the day the trade closed. `excursion`
covers only trades measured from cached bars (every trade row carries
`mae`, `mfe`, `maePoints`, `mfePoints`, `exitEfficiency`, `excursionBars`;
all null until the bars are cached).
Errors as for `/stats/daily`.

### Data version (change detection)

`GET /api/v1/version` -> `{"v": 137}` - an opaque per-user data version that
changes after every committed mutation of your data (imports, recomputes,
journal writes, account changes). Poll it (the web UI polls every 10 s) and
refetch when the value **differs** from the last one you saw. Compare for
inequality only: the counter is process-local and restarts at zero when the
server restarts. It is an in-memory read with no database work, so it is
the cheap thing to poll; the response carries `Cache-Control: no-store`.
Polling it also acts as your presence signal: each poll nudges the broker
syncs (throttled per `api_sync_throttle_sec`), so a consumer that only ever
polls `/version` still keeps the underlying data moving.

## MCP server (for AI agents)

Beyond the REST API, the app exposes an **MCP** (Model Context Protocol) server at
`POST /mcp`. AI clients — Claude Code, the Claude app, ChatGPT — connect to it by
**signing in with OAuth** (the journal is its own authorization server; see the
next section): point the client at `/mcp`, log in, click Allow. No key is copied
anywhere, and each connection can be revoked under Settings → Connected apps.
The same per-user API keys (`Authorization: Bearer tj_...`) are also accepted on
`/mcp` for scripts and clients that prefer them. Toggle the server with
`mcp.enabled` (default true) and the sign-in flow with `mcp.oauth` in
`config.yaml`.

Tools: `prepare_day` (cluster a day's copytrader fan-out + create the journal
entries to analyze - the daily-review entry point), `server_info`, `list_trades`,
`get_trade`, `list_journal`, `get_journal_entry`, `list_strategies`,
`get_strategy`, `get_chart_image` (returns the rendered PNG as image content),
`create_journal_entry`, `append_note`, `assign_strategy` and `set_tag` (both
only-if-unset, never overwrite), and `get_daily_stats` (the daily P&L / trade
stats above).

Connect (all three sign in through the browser; nothing to paste):

```bash
# Claude Code
claude mcp add --transport http trading-journal https://YOUR_HOST/mcp
claude mcp login trading-journal      # browser -> your login -> Allow
```

- **Claude app** (claude.ai / desktop): Settings → Connectors → *Add custom
  connector* → URL `https://YOUR_HOST/mcp`, leave client ID/secret empty →
  *Connect* → log in → **Allow**.
- **ChatGPT**: Settings → Connectors (Plugins) → *Create* → server URL
  `https://YOUR_HOST/mcp`, authentication **OAuth** → *Create* → log in →
  **Allow**. Registration is dynamic; keep the discovered advanced settings as
  they are. (The former Custom GPT + Action route is retired.)
- **Scripts / anything that holds a key**: `Authorization: Bearer tj_...` on
  `/mcp` works as before.

A ready-made daily-review workflow ships as the **`trading-journal-analyst`**
skill (`.claude/skills/trading-journal-analyst/`) — it clusters copytrader
fan-out into group entries, writes each analysis once (idempotent via a marker),
and synthesizes the daily journal.

## OAuth for MCP connectors

Some MCP clients cannot hold a static key: ChatGPT's MCP connector, claude.ai
custom connectors and Claude Code (`claude mcp add --transport http`) expect
the server to offer **OAuth 2.1** and register themselves with it. When
`mcp.oauth` is enabled in the config, the journal is its own authorization
server for `/mcp`:

- **Discovery**: `/.well-known/oauth-protected-resource` (RFC 9728) and
  `/.well-known/oauth-authorization-server` (RFC 8414). An unauthenticated
  call to `/mcp` answers `401` with
  `WWW-Authenticate: Bearer resource_metadata="…"`, which is how a client
  finds the rest.
- **Registration**: `POST /oauth/register` (RFC 7591). Public clients only;
  PKCE S256 is mandatory; redirect URIs must be https (optionally
  allow-listed) or loopback http (Claude Code's local callback).
- **Authorization**: the client opens `GET /oauth/authorize` in your browser.
  You log in as usual and click **Allow** on a page naming the app; the app
  then calls `POST /oauth/token` to exchange the one-time code, and later to
  rotate its refresh token.
- **Tokens** are opaque — `tjo_…` access tokens (1 h) and `tjr_…` rotating
  refresh tokens (30 d) — and are valid **only on `/mcp`**; the REST API at
  `/api/v1` stays key-only. Reusing a refresh token or replaying a code
  revokes that app's whole grant.
- **Revoking**: Settings → **Connected apps** lists each approved app with a
  Revoke button; revocation is immediate.
- `tj_` API keys keep working everywhere, including `/mcp`.

Operator setup (config block, reverse-proxy routing, per-client steps):
`docs/mcp-oauth-setup.md`.

## Status codes

| Code | Meaning |
|---|---|
| `200` | OK |
| `201` | Created (journal create) |
| `400` | Bad request (malformed id, bad/missing field, malformed or inverted date range, range too large) |
| `401` | Missing/invalid/disabled key |
| `404` | Not found, or not owned by you |
| `409` | Conflict - a singleton entry already exists (carries `entryId`) |
| `500` | Server error |

## Worked example

```bash
HOST=https://YOUR_HOST
KEY=tj_...                       # from Settings -> Generate key

# Who am I, and what's here?
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1"

# Recent day entries
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/journal?scope=day&limit=5"

# One entry in detail
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/journal/8"

# Save its rendered chart
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/journal/8/chart.png" -o chart.png

# Append an observation (original notes stay intact)
curl -s -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"note":"Late entry on the 14:32 long; see chart."}' \
  "$HOST/api/v1/journal/8/notes"

# Browse the trade log, then one trade in detail
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/trades?symbol_root=MES&outcome=win"
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/trades/4412"

# Daily P&L + trade stats for August, then just the live accounts
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/stats/daily?from=2026-08-01&to=2026-08-31"
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/stats/daily?from=2026-08-01&to=2026-08-31&filter=type:live"

# Has anything changed since my last fetch? (poll this, not the data)
curl -s -H "Authorization: Bearer $KEY" "$HOST/api/v1/version"

# Create a day journal entry (201, or 409 with entryId if it already exists)
curl -s -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"scope":"day","date":"2026-06-02","body":"Choppy open, sat out.","tag":"neutral"}' \
  "$HOST/api/v1/journal"
```

## Notes & limits (v1)

- Keys are **full-access** to their user's data (no per-key scopes yet).
- Writes are **create an entry** and **append a note** - both additive. The API
  never overwrites or deletes: it can't edit an existing entry's tag/mood/
  strategy, delete entries, or create/modify trades.
- Trades are read-only and the list is page-based (fixed page size 50).
- No rate limiting (intended for a private network / trusted agents).
- The PNG is a single panel; the JSON `chart` field carries every panel.
- Daily stats come from the `daily_stats` cache (rebuilt after every import
  or recompute) - the Calendar's numbers - and are capped at 400 days per
  request.
- `openapi.json` needs a key like everything else; while signed in to the
  web UI the same document is at `/settings/api-docs/openapi.json`, and this
  reference is rendered under **Settings → API docs**.
