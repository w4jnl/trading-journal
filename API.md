# HTTP API

A small, read-plus-append HTTP API over your own strategies and journal
entries, authenticated by a **per-user API key**. It's designed to be
LLM-friendly: a discovery index, a machine-readable OpenAPI 3.1 spec,
stable camelCase JSON, and append-only note writes that can never destroy
what you've already written.

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
  `{"items": [...]}`; the journal list additionally carries
  `{"page": {"limit", "offset", "count"}}`.
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
| `GET`  | `/api/v1/strategies` | List your strategies |
| `GET`  | `/api/v1/strategies/{id}` | One strategy |
| `GET`  | `/api/v1/journal` | List journal entries (summary rows) |
| `GET`  | `/api/v1/journal/{id}` | One journal entry (full detail) |
| `POST` | `/api/v1/journal/{id}/notes` | Append a dated note (append-only) |
| `GET`  | `/api/v1/journal/{id}/chart.png` | Server-rendered chart image |
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
| `404` | Entry doesn't exist or you don't own it - **the API does not create entries** |

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

## Status codes

| Code | Meaning |
|---|---|
| `200` | OK |
| `400` | Bad request (malformed id or note body) |
| `401` | Missing/invalid/disabled key |
| `404` | Not found, or not owned by you |
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
```

## Notes & limits (v1)

- Keys are **full-access** to their user's data (no per-key scopes yet).
- The only write is **append a note**. The API cannot create or delete
  entries, edit tags/mood/strategy, or change trades.
- No rate limiting (intended for a private network / trusted agents).
- The PNG is a single panel; the JSON `chart` field carries every panel.
