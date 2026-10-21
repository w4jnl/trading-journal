# HTTP API

An HTTP API over your own strategies, journal entries, trades, open
positions, accounts and daily P&L / trade stats, authenticated by a
**per-user API key**. It's designed to be LLM-friendly: a discovery index, a
machine-readable OpenAPI 3.1 spec and stable camelCase JSON.

There are two kinds of key (see *Pairing a phone*). An **agent key**, made
with *Generate key* under Settings, writes additively only: it can create
entries and append notes, but can never edit or delete what you've already
written. A **device key**, made by pairing your phone, is you: it may also
edit and delete journal entries, keep balance anchors, change settings and
register for push. Routes marked *device key* answer `403` to an agent key.

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

### Pairing a phone (device keys)

The companion phone app does not paste a key: it **pairs**. In
**Settings → Phone app** click *Pair a phone*; the page shows a one-time
code such as `AB7Q-K2MN`, as text and as a QR of
`tj://pair?server=https://YOUR_HOST&code=AB7Q-K2MN`. The app scans it (or
you type the server and the code) and calls the one API route that needs
no key:

```bash
curl -X POST https://YOUR_HOST/api/v1/pair \
  -H "Content-Type: application/json" \
  -d '{"code": "AB7Q-K2MN", "deviceName": "Trader'"'"'s iPhone"}'
```

```jsonc
// 201
{
  "key": "tj_...",                                   // shown once; the app stores it in its keychain
  "server": {"url": "https://YOUR_HOST", "version": "v2.17.0", "timezone": "America/Chicago",
             "features": {"ai": true, "charts": true, "guardrail": true, "push": false}},
  "user": {"id": 1, "username": "trader", "displayName": "Trader", "email": "trader@example.com"}
}
```

Case, spaces and the dash in the code do not matter. A code works **once**
and for **five minutes**; making a new one retires the old; a wrong, used
or expired code answers `400` with the same message, and an address that
keeps failing is answered `429` with `Retry-After` for a while.

The key it returns is a **device key** (`kind: "device"`): it is the
trader's own phone, so it may do what an agent key may not - edit and
delete journal entries, change settings, register for push. Keys made with
*Generate key* are **agent keys** (`kind: "agent"`): the additive rules in
this document (append notes, set a tag only if unset, never overwrite)
apply to them, and the write routes marked *device key* answer `403` to
them. Paired phones are listed under *Phone app* with an *Unpair* button;
that revokes the key.

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
| `POST` | `/api/v1/pair` | Exchange a pairing code for a device key (no key; the phone app's sign-in) |
| `GET`  | `/api/v1/me` | The key's user, the server and the user's settings |
| `GET`  | `/api/v1/today` | The day in one object (stats, positions, plan, nudges, curve, equity) |
| `GET`  | `/api/v1/events` | Server-sent events: a version event after every change |
| `GET`  | `/api/v1/nudges` | A day's guardrail nudges with their seen state |
| `GET`  | `/api/v1/devices` | The phones registered for push (device key) |
| `POST` | `/api/v1/devices` | Register a phone's push token and settings (device key) |
| `PUT`  | `/api/v1/devices/{token}/prefs` | Replace a phone's notification settings (device key) |
| `PUT`  | `/api/v1/devices/{token}/activity` | Report the push-to-start token and running Live Activities (device key) |
| `DELETE` | `/api/v1/devices/{token}` | Forget a phone (device key) |
| `POST` | `/api/v1/nudges/seen` | Mark several nudges seen |
| `POST` | `/api/v1/nudges/{id}/seen` | Mark a nudge seen |
| `PUT`  | `/api/v1/settings` | Change the user's switches (device key) |
| `GET`  | `/api/v1/stats/daily` | Daily P&L + trade stats with range totals |
| `GET`  | `/api/v1/analytics/summary` | The analytics page for a range as one object (totals, R, drawdown, streaks, breakdowns) |
| `GET`  | `/api/v1/strategies` | List your strategies |
| `POST` | `/api/v1/strategies` | Create a strategy (name, playbook, checklist, inline images) |
| `GET`  | `/api/v1/strategies/{id}` | One strategy |
| `PUT`  | `/api/v1/strategies/{id}` | Change a strategy (the fields passed replace; append and images add) |
| `GET`  | `/api/v1/journal` | List journal entries (summary rows) |
| `POST` | `/api/v1/journal` | Create an entry (day/week/trade/group) |
| `POST` | `/api/v1/journal/prepare-day` | Cluster a day's trades + create the entries to analyze |
| `GET`  | `/api/v1/journal/{id}` | One journal entry (full detail) |
| `PUT`  | `/api/v1/journal/{id}` | Replace the entry: body, tag, mood, strategy, labels, checklist (device key) |
| `DELETE` | `/api/v1/journal/{id}` | Delete the entry (device key) |
| `POST` | `/api/v1/journal/attachments` | Upload an image for a body (device key) |
| `GET`  | `/api/v1/journal/labels` | The mistake / emotion vocabulary with display names |
| `POST` | `/api/v1/journal/{id}/notes` | Append a dated note (append-only) |
| `POST` | `/api/v1/journal/{id}/strategy` | Assign a strategy (only if the entry has none) |
| `POST` | `/api/v1/journal/{id}/tag` | Set a tag (only if the entry has none) |
| `POST` | `/api/v1/journal/{id}/labels` | Add mistake / emotion labels (additive) |
| `GET` | `/api/v1/coach` | Rule-based coach checks for a trading day |
| `GET` | `/api/v1/plan` | A day's pre-market plan and how the day measured against it |
| `GET` | `/api/v1/digest` | The weekly edge digest |
| `POST` | `/api/v1/plan` | Write or amend the plan for today or a future day |
| `GET`  | `/api/v1/journal/{id}/chart.png` | Server-rendered chart image |
| `GET`  | `/api/v1/trades` | List trades (filtered, paged) |
| `GET`  | `/api/v1/trades/{id}` | One trade (fills + chart) |
| `GET`  | `/api/v1/trades/{id}/chart.png` | Server-rendered trade chart image |
| `GET`  | `/api/v1/accounts` | Accounts with their running balance and today's net |
| `POST` | `/api/v1/accounts/{id}/balances` | Record a balance anchor (device key) |
| `DELETE` | `/api/v1/accounts/{id}/balances/{anchorId}` | Remove a balance anchor (device key) |
| `GET`  | `/api/v1/equity` | Equity now by type, the per-day curve, the drawdown, today's change |
| `GET`  | `/api/v1/attachments/{id}` | Fetch an embedded image |

### Strategies

A strategy is a short name plus a full markdown description (which may
embed images).

`GET /api/v1/strategies` -> `{"items": [Strategy, ...]}` (the active ones)
`GET /api/v1/strategies/{id}` -> `Strategy` (or `404`; archived ones too)

```jsonc
// Strategy
{
  "id": 3,
  "name": "ORB fade",
  "body": "## Opening-range fade\n\nWait for...\n\n![chart](/api/v1/attachments/12)",
  "attachments": [ { "id": 12, "url": "/api/v1/attachments/12" } ],
  "checklist": [ "Range is set", "Stop above the high" ],
  "createdAt": "2026-05-02T09:14:00Z",
  "updatedAt": "2026-05-20T18:31:00Z",
  "archivedAt": null
}
```

`body` is the complete description with image references rewritten to
`/api/v1/attachments/{id}`; `attachments` lists those same images;
`checklist` holds the rules a trade of the strategy is checked against, in
order.

**Writing.** `POST /api/v1/strategies` creates one (`201`) and
`PUT /api/v1/strategies/{id}` changes one (`200`); both take the same JSON
and answer with the `Strategy` plus `images`, the images this write stored:

```jsonc
{
  "name": "ORB fade",                       // POST: required. PUT: renames
  "body": "## Opening-range fade\n\n![the setup](setup)\n\nWait for...",
  "append": "Never before 9:45.",           // a paragraph added at the end
  "checklist": [ "Range is set", "Stop above the high" ],
  "images": [ { "name": "setup", "data": "<base64 PNG/JPEG/GIF/WebP>" } ]
}
```

On `PUT` every field is optional and only what is present changes: `name`,
`body` and `checklist` **replace** (`"checklist": []` clears the rules),
`append` and `images` **add**. An image goes in `images` as base64 (a
`data:` URL is accepted, the type is read from the bytes, 8 MiB and ten
images per write); it is stored as your attachment and lands in the body
where the body refers to it by name, `![caption](name)`, or at the end when
nothing does. Image URLs you echo back from a `GET` are fine in `body`.
Errors: `400` (no name, bad JSON or base64), `404`, `409` (the name is in
use), `413` (an image over 8 MiB), `415` (not an image type we store).
The MCP tools `create_strategy` and `update_strategy` are these two calls.

### Journal list

`GET /api/v1/journal` returns summary rows, newest first, with paging.

Query parameters (all optional):

| Param | Type | Notes |
|---|---|---|
| `scope` | `day` \| `week` \| `trade` \| `group` | Repeatable (e.g. `?scope=day&scope=week`) |
| `from` | `YYYY-MM-DD` | Inclusive lower bound on entry date |
| `to` | `YYYY-MM-DD` | Inclusive upper bound |
| `tag` | `good` \| `neutral` \| `bad` \| `untagged` | A tag also matches an untagged entry whose AI review suggests it |
| `strategy` | strategy id, or `none` | Filter by assigned strategy; an id also matches an unassigned entry whose AI review suggests it |
| `mistake` | label key, or `none` | Repeatable; entries carrying any of the picked mistakes or with one of them suggested (`none` = no mistake logged) |
| `emotion` | label key | Repeatable; entries carrying any of the picked emotions or with one of them suggested |
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
  "checklist": { "strategyId": 3, "done": 2, "total": 3,             // the strategy's playbook rules and the trader's ticks; null without one
                 "items": [ { "id": 9, "text": "Stop placed before the entry", "checked": true }, { "id": 10, "text": "Flat by 15:58", "checked": false } ] },
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

### Editing and deleting entries (device key)

The paired phone edits like the page editors do; an agent key gets `403`
on these three routes and keeps to the additive calls above.

`PUT /api/v1/journal/{id}` replaces the entry in one go, the way a page
save does: **absent fields clear** (no `tag` removes the tag, an empty
`mistakes` removes the mistakes), `body` must not be empty (use DELETE),
`mood` is accepted on day entries only, `strategyId` must be one of your
strategies, `checks` are the ticked checklist item ids of the entry's
strategy (trade and group entries). Images in the body are written as
`/api/v1/attachments/{id}` (the server stores them in its own form and
rewrites them back on read). What you write is recorded as the trader's
own, not as `api`.

```bash
curl -X PUT https://YOUR_HOST/api/v1/journal/412 -H "Authorization: Bearer tj_..." \
  -H "Content-Type: application/json" -d '{
    "body": "Took the open-drive long at 7762; **held into the target**.\n\n![](/api/v1/attachments/88)",
    "tag": "good", "mood": 4, "strategyId": 12,
    "mistakes": [], "emotions": ["calm", "focused"], "checks": [3, 4],
    "updatedAt": "2026-10-15T14:02:11Z"
  }'
```

The response is the entry as `GET /api/v1/journal/{id}` returns it. Pass
`updatedAt` exactly as you loaded it: when the entry changed meanwhile
(the desk edited it, an AI review wrote to it), the answer is `409` with
`"reason": "stale"` and the current entry under `entry`, so the app can
reload or ask before overwriting. While an AI review is writing the note
the save is refused with `409` and `"reason": "review-pending"`; append a
note instead, or wait.

`DELETE /api/v1/journal/{id}` removes the entry (`204`; `409` under an AI
review; a group entry's links to its trades go with it).

`POST /api/v1/journal/attachments` takes a multipart upload (field
`file`: PNG, JPEG, GIF or WebP, up to 8 MiB) and answers `201` with the
image's id, its API URL and the markdown line to put in a body:

```jsonc
{"id": 88, "url": "/api/v1/attachments/88", "name": "IMG_0412.jpg", "size": 182311, "markdown": "![IMG_0412.jpg](/api/v1/attachments/88)"}
```

`GET /api/v1/journal/labels` lists the vocabulary with display names and a
`description` of what each label's evidence looks like, for pickers:
`{"mistakes": [{"key": "chased", "name": "Chased the entry", "description": "Entered after the move had run: ..."}, ...], "emotions": [...]}`.
The AI review reads the same descriptions when it suggests labels.

### The AI review's suggestions (device key)

The in-app AI review never writes a tag, strategy or label: it records
suggestions the trader accepts or dismisses. `GET /api/v1/journal/{id}`
lists the open ones under `suggestions`, each with what to show:

```jsonc
"suggestions": [ { "kind": "strategy", "value": "12", "name": "VWAP Bounce" },
                 { "kind": "mistake", "value": "chased", "name": "Chased the entry" } ]
```

`POST /api/v1/journal/{id}/suggestions` with `{"kind", "value", "do":
"accept"|"dismiss"}` acts on one: accept makes it the field, recorded as
the trader's own; dismiss keeps it dismissed, so a later review does not
propose it again. The answer is the entry as `GET` returns it (`404` for
a suggestion that is not open, `409` with `reason: review-pending` while
a review writes the note, `403` for an agent key).

### The review method

`GET /api/v1/ai/method` is the method this journal's reviews work by, for
agents and scripts that are not MCP clients: `version` (the method version,
recorded on every review marker as `method=` and on every stored AI
output), `marker` (the line an external agent ends a review with, `<!--
tj-ai-mcp:v1 method=2 -->`), `markers` (`app`, `agent`), the user's review
`language`, their `profile` (`style`: `scalper`, `daytrader` or `swing`;
`focus`, the evidence families they weigh most in order, among `levels`,
`session`, `volume`, `gamma`, `coach`, `playbooks`; `markets`; `houseRules`,
their standing instructions; set in Settings > AI), `methodMarkdown` (the
whole method: the analyst, the briefs per scope, the trader's profile, the
lesson and verdict lines, the write rules, the modes with their tool order,
the evidence glossary, the tool list), `labels` (the vocabulary
a review may name) and `modes` (`review_day`, `review_entry`, `today`,
`review_week`, `plan_day`, each with its arguments and steps). The MCP
server serves the same method as its instructions, as the resource
`trading-journal://method`, as the tool `get_review_method` and as prompts;
the client skills only point at it.

`GET /api/v1/ai/preview/{id}` is the request a review of journal entry `id`
would send right now, without calling a model and without writing anything:
`system` and `user` as lists of blocks (`text`, `cached`, `chars`,
`tokensEstimate` at four characters a token), `images` (whether the chart
would ride along), `language`, `method` and `totals` (`chars`,
`tokensEstimate`, `cachedTokensEstimate`). `?lang=Czech` previews the review
in another offered language. The cached blocks are what a provider's prompt
cache reads back from the second call on; the last user block is the item
itself and changes every time.

```bash
curl -s -H "Authorization: Bearer tj_..." "$BASE/api/v1/ai/preview/42" \
  | jq '{scope, images, totals, blocks: [.system[], .user[] | {cached, tokensEstimate}]}'
```

### AI from the phone (device key)

Every AI call runs on the server with the user's own provider; a client only
starts work and reads the result where it lands. `GET /api/v1/me` carries
`"ai": {"ready": true|false}`: whether this user can run AI at all (the
server has AI, the user a provider). Every start below needs a device key and
answers `409` with `reason: ai-off` for a user without a provider, `404` when
the server runs no AI. A start and its finish bump the data version, so a
client on `GET /api/v1/events` refetches the object the result lands on.

**Reviewing a note.** `POST /api/v1/ai/review` with `{"entryId": 12}` reviews
an existing note; `{"scope": "trade", "tradeId": 5}`, `{"scope": "set",
"setId": 3}`, `{"scope": "day", "date": "2026-10-14"}` or `{"scope": "week",
"week": "2026-W42"}` review the note of that thing, created first when there
is none. `202 {"entryId", "pending": true}`; `409` with `reason:
review-pending` while a review of that note runs. `GET /api/v1/journal/{id}`
shows `"review": {"pending": true|false, "error"?: "..."}`; the error is the
last failure, shown once. While pending, page saves and deletes of the note
answer `409 review-pending` (see above); appends are unaffected. The result is
the review appended to the note plus its `suggestions`.

**The day run.** `POST /api/v1/ai/day/{date}` (`YYYY-MM-DD` or `last`; body
`{"mode": "all"}` re-reviews reviewed notes) reviews every note of the day
and writes the day's synthesis: `202 {"status": {...}}`, `409` with `reason:
day-run` while one runs. `GET /api/v1/ai/day` is the status: `{"status":
{"state": "idle"|"running"|"done"|"error"|"cancelled", "date", "done", "total",
"current", "err"?, "model", "startedAt"}}`. `DELETE /api/v1/ai/day` cancels;
the notes reviewed so far keep their reviews.

**Levels on an open position.** `POST /api/v1/positions/{account}/{symbol}/levels`
asks for a stop and target on the open position, `202 {"pending": true}`. Each
position of `GET /api/v1/positions` carries `"ai"` when the server runs AI:

```jsonc
"ai": {"pending": false, "error": "...",         // the last failure, shown once
       "levels": {"stop": 7802.5, "target": 7781, "target2": null, "scaleOut": null,
                  "trail": "...", "reasons": {"stop": "...", "target": "..."}, "summary": "...",
                  "risk": 87.5, "r1": 1.5, "r2": null, "entry": 7794.25, "qty": 1},
       "model": "claude-...", "at": "2026-10-15T20:03:00Z",
       "outdated": false,                           // the size or entry changed since
       "auto": "off"}                               // what a new position gets without asking: off, suggest, plan
```

`POST /api/v1/positions/{account}/{symbol}/levels/apply` writes the stop and
first target into the plan (source `ai`, every leg of a copied decision),
answering with the position; `DELETE /api/v1/positions/{account}/{symbol}/levels`
forgets the proposal (`204`). `409` when the position is flat or there is
nothing to apply.

**A day plan draft.** `POST /api/v1/plan/{date}/draft` (today or later)
drafts the plan from the trader's own numbers and lessons, `202`. `GET
/api/v1/plan?day=` then carries `"draft": {"pending", "error"?, "maxTrades",
"stopTime", "maxLoss", "levels", "notes", "reasons": [...], "model",
"createdAt"}`, and `GET /api/v1/today` the same under `plan.draft` for today;
saving the plan with `POST /api/v1/plan` drops the draft, and
`DELETE /api/v1/plan/{date}/draft` discards it (`204`).

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
re-run knows what to skip. `400` for a malformed or future date. The
response also carries `checks`: the day's coach checks (below), so the
reviewer has the objective observations next to the entries.

### Weekly digest

`GET /api/v1/digest?week=YYYY-Www` (default: the week of the last trading
day) is the `/digest` page as one object: the ISO week counted **per
decision** (a copy set once, its accounts' money summed) against the
week before. The MCP tool `get_weekly_digest` returns the same object and
the in-app AI review of a week entry is built on it.

```jsonc
{
  "week": "2026-W41", "from": "2026-10-05", "to": "2026-10-11", "tradingDays": 5,
  "kpis": [ { "key": "net", "label": "Net P&L", "value": 1906.65, "previous": 1204.10 },
            { "key": "decisions", "value": 61, "previous": 58 },
            { "key": "winRate", "value": 0.82 }, { "key": "profitFactor", "value": 2.1 },
            { "key": "avgR", "value": null }, { "key": "exitKept", "value": 0.16 } ],
  "keep": [ "VWAP Bounce carried the week: +812.00 over 9 decision(s)." ],
  "fix":  [ "Moved the stop cost -240.50 over 3 decision(s).",
            "Re-entry right after a loss came up on 3 of 5 trading days.",
            "No day plan on 4 of 5 trading days." ],
  "bestDecision":  { "tradeId": 340404, "day": "2026-10-09", "symbolRoot": "ES", "direction": "short", "accounts": 4, "net": 82.02 },
  "worstDecision": { "tradeId": 339872, "day": "2026-10-06", "symbolRoot": "MES", "direction": "long", "accounts": 6, "net": -96.30 },
  "coach": [ { "key": "revenge-entry", "title": "Re-entry right after a loss", "severity": "warn", "count": 4, "days": 3 } ],
  "plan": { "tradingDays": 5, "planned": 1, "writtenLate": 0,
            "rules": [ { "key": "max-trades", "name": "decision cap", "held": 0, "total": 1 } ] },
  "lessons": [ { "mistake": "chased", "mistakeName": "Chased the entry", "count": 2, "open": 2,
                 "lesson": "Wait for the retest.", "day": "2026-10-09" } ],
  "this": { /* the analytics summary of the week, unit=sets */ },
  "previous": { /* the week before */ }
}
```

`keep` and `fix` are one sentence each, derived from the numbers only:
the strategy that carried the week, the best entry hour, better exits;
the costliest logged mistake, a warning that recurred on two or more
days, missing day plans and the plan rule that held least. `lessons` are
the one-line lessons the in-app AI reviews of the week's notes named,
grouped by the mistake they are about (`count`; `open` = not yet marked
learned on `/journal/lessons`); a lesson about no mistake stands alone. `GET
/digest.md?week=` downloads the same digest as markdown.

### Day plan

A pre-market plan per trading day: a `bias` (`long`, `short`, `neutral`
for both ways, `flat` to stay out), key `levels` (free text, one per
line), `maxTrades` (decisions, a copy set counts once), `stopTime` (HH:MM,
trading timezone; no new entries after it), `maxLoss` (per account: stop
once any account is this much down on the day) and `notes`. The day's
journal page shows it with each rule checked against the day.

`GET /api/v1/plan?day=YYYY-MM-DD|today|yesterday|last` (default today)
returns the plan and its outcome so far:

```jsonc
{
  "day": "2026-10-09",
  "plan": { "day": "2026-10-09", "bias": "short", "levels": "7860 HVL\n7900 call wall",
            "maxTrades": 6, "stopTime": "21:00", "maxLoss": 300, "notes": "",
            "createdAt": "2026-10-09T06:40:11Z", "updatedAt": "2026-10-09T06:40:11Z" },
  "outcome": {
    "checks": [
      { "key": "bias", "rule": "Short bias", "actual": "9 with it (+412.00), 2 against (-61.50)", "held": false },
      { "key": "max-trades", "rule": "At most 6 decision(s)", "actual": "took 11", "held": false },
      { "key": "stop-time", "rule": "Done by 21:00", "actual": "last entry 20:47", "held": true },
      { "key": "max-loss", "rule": "Stop at 300.00 down per account", "actual": "worst account -84.20 (APEX-1)", "held": true }
    ],
    "held": 2, "writtenLate": false, "editedLate": false
  }
}
```

`outcome.checks` stays empty until the day has trades; `writtenLate` /
`editedLate` flag a plan saved or changed after the day's first entry.

`POST /api/v1/plan` with `{"day": "today", "bias": "short", "maxTrades": 6,
"stopTime": "21:00"}` writes or amends the plan for **today or a future
day**; a past day is refused (its record is edited on the journal page,
where a late edit is flagged). Present fields replace the stored ones, an
empty string or `0` clears one, absent fields are kept; a plan left empty
is removed. The MCP tools `get_day_plan` and `set_day_plan` do the same.

### Coach checks

`GET /api/v1/coach?day=YYYY-MM-DD|today|yesterday|last` (default: the
last trading day) runs deterministic checks over the day's trades and
returns observations, not verdicts. The MCP tool `get_coach_checks`
returns the same object.

| Key | Fires when | Suggests |
|---|---|---|
| `revenge-entry` | a decision opens within a minute of a losing exit on the same market | `revenge` |
| `size-up-after-loss` | an account's position grows right after its losing trade | `oversized` |
| `past-stop` | measured heat exceeded the planned risk | `moved-stop` |
| `left-on-table` | a winner kept under a quarter of the best move it saw | `early-exit` |
| `held-loser` | a losing decision was on for three or more times a usual winner's hold | `held-loser` |
| `overtraded` | decisions exceed 1.5 x the usual day (and at least 10) | `overtraded` |
| `late-session` | entries after the usual last entry plus 30 minutes | - |

When the trader wrote a pre-market plan for the day (below), its rules
come first as `plan-bias`, `plan-max-trades`, `plan-stop-time` and
`plan-max-loss` findings, and the report carries the plan's outcome under
`plan`. A plan's cap and stop time replace `overtraded` and
`late-session` for that day.

A decision is a copy set or a lone trade. The norms (`baseline`: median
decisions per day, usual last entry at the 90th percentile of entry
times, median winner hold) come from the preceding weeks and the last
three checks stay quiet until five trading days of history exist.

```jsonc
// 200
{
  "date": "2026-10-09", "trades": 71, "decisions": 12,
  "baseline": { "days": 22, "medianDecisions": 9, "usualLastEntry": "21:40", "medianWinnerHoldSec": 48 },
  "findings": [
    { "key": "revenge-entry", "title": "Re-entry right after a loss", "severity": "warn",
      "detail": "MES short opened 20s after a -40.00 loss on the same market.",
      "tradeIds": [4412, 4413], "label": { "kind": "mistake", "key": "revenge", "name": "Revenge trade" } }
  ]
}
```

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

An image is served over the API to the user who stored it only; anything
else is `404`.

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
| `sort` | `entry` \| `exit` \| `account` \| `symbol` \| `dir` \| `duration` \| `qty` \| `entryp` \| `exitp` \| `gross` \| `comm` \| `net` \| `r` | order of the list; default newest entry first |
| `dir` | `asc` \| `desc` | with `sort`; each column has its natural default |
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

### Open positions

`GET /api/v1/positions` returns what is open right now: every account and
contract whose fills do not net flat, with the FIFO average entry of the
contracts still open, the freshest price a provider gives, the unrealised
P&L at it, and the **plan** recorded on the position - stop, risk, target,
thesis - with `planSource` saying who set it (`trader`, `api`, `ai`, or
`set` for a leg that inherited the copied decision's plan) and `setLegs`
naming the other accounts of the same decision. A position is not a trade
yet: it becomes one, carrying its plan, when it closes. `?refresh=1` pulls
the latest broker fills first. Positions on accounts hidden in Settings
(`excludedFromStats` on `GET /accounts`) are not listed: they show nowhere,
count nowhere and raise no nudge, like the account's trades leave every
default list.

```jsonc
{
  "items": [
    {
      "accountId": 7, "account": "APEX-100001-28", "firm": "apex",
      "symbol": "MESZ6", "symbolRoot": "MES", "direction": "long", "qty": 3,
      "avgEntry": 7766.33, "openedAt": "2026-10-14T20:23:04Z", "fills": 2,
      "lastPrice": 7767.25, "lastAt": "2026-10-14T20:31:00Z", "priceSource": "Yahoo", "unrealized": 13.75,
      "stop": 7755, "risk": 170, "target": 7790, "thesis": "reclaim of the overnight high",
      "planSource": "trader", "setPlan": true, "ticksToStop": 49, "beyondStop": false,
      "setLegs": [6], "stale": false,
      "planUrl": "/api/v1/positions/7/MESZ6/plan"
    }
  ]
}
```

`lastPrice` is the most recent answer among the sources that can price the
contract, and `priceSource` names it: `MotiveWave` (the mark a chart posted
through the levels feed within the last three minutes, the broker's own
feed), `TopstepX` (the user's session), `Yahoo` (the vendor's bar) or
`cached bars` (an old vendor bar). `unrealized` and `ticksToStop` follow it.

`POST /api/v1/positions/{account}/{symbol}/plan` with `{"stop": 7755,
"target": 7790, "thesis": "..."}` records the plan. The whole plan is
replaced (pass every field to keep); a stop alone derives the risk at the
position's size, a risk alone derives the stop snapped to tick; an empty
body clears it. On a leg of a copied decision the plan goes on every leg
unless `"allLegs": false`. The plan is written with source `api`: the
position's sheet shows it dashed until the trader saves it, the same way
a tag or label written through the API is marked. The MCP tools
`list_positions` and `set_position_plan` are the same calls. Write only
what the trader stated; a stop they did not give is not a plan.

### Accounts and balances

`GET /api/v1/accounts` lists your accounts (`?show_retired=1` includes
retired ones) with the **running balance** each has, computed as the
`/accounts` page does: the latest balance anchor at or before today plus
the net of the trades closed after it. `balance` is `null` for an account
with no anchor; `dayNet` is today's net from the daily stats, `null` on a
day without trades.

```jsonc
{
  "items": [
    {
      "id": 7, "name": "APEX-100001-28", "firm": "apex", "type": "prop_funded", "typeLabel": "Prop · Funded",
      "currency": "USD", "active": true, "retired": false, "excludedFromStats": false, "needsSetup": false,
      "copyGroup": "apex", "importSource": "motivewave_csv",
      "balance": {"balance": 51230.5, "asOf": "2026-10-15", "anchorDate": "2026-10-01", "unchangedSinceAnchor": false,
                  "anchors": [{"id": 3, "date": "2026-10-01", "balance": 50000, "note": "payout reset"}]},
      "dayNet": 120.5
    }
  ]
}
```

`POST /api/v1/accounts/{id}/balances` (**device key**) with
`{"date": "2026-10-15", "balance": 51230.5, "note": "statement"}` records
an anchor (`date` defaults to today; a second anchor on the same day
replaces the first) and answers `201` with the account and its new
balance block. `DELETE /api/v1/accounts/{id}/balances/{anchorId}` removes
one (`204`; `404` when the anchor is not that account's).

### Equity and drawdown

`GET /api/v1/equity` is the dashboard's equity tile and the analytics'
drawdown in one object, for the same `filter=` as the stats routes
(default: active accounts not excluded from stats) and an optional
`from`/`to` (default: the whole history):

```jsonc
{
  "asOf": "2026-10-15",
  "equityNow": 83410.25,
  "byType": [{"type": "live", "label": "Live", "total": 32180.0, "accounts": 1},
             {"type": "prop_funded", "label": "Prop · Funded", "total": 51230.25, "accounts": 2}],
  "included": 3, "excluded": 1,                       // accounts with and without an anchor
  "series": [{"date": "2026-09-01", "equity": 80000}, {"date": "2026-09-02", "equity": 80410.5}],
  "drawdown": {"maxDrawdown": 1840.0, "maxDrawdownPeakAt": "2026-09-22T19:58:00Z", "maxDrawdownTroughAt": "2026-09-25T20:10:00Z",
               "maxDrawdownRecoverAt": null, "maxDrawdownDays": 15, "currentDrawdown": 320.0, "underwater": true, "peakAt": "2026-09-22T19:58:00Z"},
  "dayChange": 120.5
}
```

`series` is the per-day running equity across the included accounts;
`drawdown` walks the cumulative net of the closed trades in the range
(money, not equity: an account without an anchor still counts). For one
account's drawdown use `filter=acc:N`; `/analytics/summary` carries the
same block.

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
`filter`, `account_id`, `refresh`) plus `unit` (`trades` | `sets`), same
defaults (last 30 days), same
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
  "compliance": [ { "strategy": "ORB fade", "trades": 18, "avgPct": 0.83, "complete": 11, "completeNet": 410.0, "incompleteNet": -62.5 } ],  // playbook checklists
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
all null until the bars are cached). Trade rows also carry `setId` /
`setSize`: copies of one decision fanned across accounts by a copytrader
share a `setId` (same underlying, same direction, entries within two
seconds, two or more accounts); group by it instead of guessing from
timestamps. `unit=sets` on the summary counts each set once with its money
summed (`unit` and the raw `trades` count are echoed in the response).
Errors as for `/stats/daily`. `src=confirmed` counts only the tags,
strategies and labels the trader set or confirmed in the app: values written
through the API or MCP (recorded with source `api`) stay out until the trader
saves the note in its editor.

### Me and settings

`GET /api/v1/me` says who the key belongs to and what the server is, in
one call - the app's first request after pairing and its Settings screen:

```jsonc
{
  "user": {"id": 1, "username": "trader", "displayName": "Trader", "email": "trader@example.com"},
  "server": {"url": "https://YOUR_HOST", "version": "v2.17.0", "timezone": "America/Chicago",
             "features": {"ai": true, "charts": true, "guardrail": true, "push": false}},
  "settings": {"guardrail": true, "autoPlan": 0, "aiLanguage": "",
               "aiProfile": {"style": "daytrader"}},
  "key": {"kind": "device", "name": "the trader's iPhone", "createdAt": "2026-10-15T14:02:11Z"}
}
```

`features` says what this server can do: `ai` (an AI provider is set up),
`charts` (a market-data source is configured, so chart images and session
context exist), `guardrail` (the intraday worker runs), `push` (phones can
register for notifications). `settings.aiLanguage` is the review language
("" for English) and `settings.aiProfile` the trader profile from Settings >
AI (`style`, `focus`, `markets`, `houseRules`; see the review method below),
both read-only here. `settings.autoPlan` is what a new open
position gets without a click: `0` nothing, `1` the AI's stop and target
as a suggestion on its sheet, `2` the suggestion written into the plan.

`PUT /api/v1/settings` (**device key**) changes the switches; present
fields change, absent ones are kept:

```bash
curl -X PUT https://YOUR_HOST/api/v1/settings -H "Authorization: Bearer tj_..." \
  -H "Content-Type: application/json" -d '{"guardrail": true, "autoPlan": 1}'
# 200 {"settings": {"guardrail": true, "autoPlan": 1}}
```

An agent key gets `403`; an `autoPlan` outside 0..2 gets `400`.

### Today

`GET /api/v1/today` is the day in one object, for a screen or a widget
that shows "how is it going right now" without six calls (`?refresh=1`
pulls the latest broker fills first):

```jsonc
{
  "day": "2026-10-15", "timezone": "America/Chicago", "v": 137, "asOf": "2026-10-15T14:31:00Z",
  "session": {"rthStart": "2026-10-15T13:30:00Z", "rthEnd": "2026-10-15T20:00:00Z", "exchangeTz": "America/Chicago", "inGlobex": true, "inRth": true},
  "stats": {"trades": 4, "wins": 3, "losses": 1, "net": 120.5, "gross": 138.5, "commission": 18.0, "winRate": 0.75, "winDays": 1, "lossDays": 0, "bestDay": 120.5, "worstDay": 120.5, "largestWin": 71.0, "largestLoss": -33.3},
  "unrealized": {"total": 13.75, "stale": false, "priced": 1, "unpriced": 0},
  "positions": [ /* as GET /positions */ ],
  "plan": {"plan": { /* as GET /plan */ }, "outcome": { /* checks so far */ },
           "tradesUsed": 4, "maxTrades": 6, "lossUsed": 84.2, "maxLoss": 300, "worstAccount": "APEX-1"},
  "decisions": {"count": 2, "wins": 2, "losses": 0, "open": 1},
  "nudges": {"unseen": 1, "badge": 1, "items": [ /* newest first, at most 10; as GET /nudges */ ]},
  "lastFillAt": "2026-10-15T14:28:04Z",
  "lastTrade": {"id": 4412, "symbolRoot": "MES", "direction": "short", "netPnl": 33.3, "exitTime": "2026-10-15T14:28:04Z", "account": "APEX-1"},
  "curve": [{"t": "2026-10-15T13:52:10Z", "net": 71.0}, {"t": "2026-10-15T14:28:04Z", "net": 120.5}],
  "equity": {"known": true, "now": 83410.25, "byType": [{"type": "live", "label": "Live", "total": 32180.0, "accounts": 1}],
             "dayChange": 120.5, "currentDrawdown": 320.0, "underwater": true}
}
```

`session` is the CME day in UTC instants (regular hours 08:30 to 15:00
Chicago) with whether Globex and regular hours are trading now. `stats`
are today's daily-stats totals. `unrealized` sums the priced open
positions (`stale` when a price is older than five minutes); positions
on hidden accounts are neither listed nor summed. `plan`
carries the day plan with its outcome so far plus the two budgets:
decisions made against `maxTrades`, and the worst account's realised
loss against `maxLoss` (`lossUsed` is positive money). `decisions` is
the day per decision, a copy set counting once: closed decisions with
their wins and losses by the sign of the set's net, and `open`, the
decisions among the open positions; a client that groups its trades by
set shows these in place of `stats` counts. `nudges.unseen`
counts today's nudges not yet acknowledged, `badge` only those from the
last two hours. `curve` is the cumulative net after each closed trade of
the day. `equity` is `GET /equity` reduced to what fits on a card.

### Events (server-sent)

`GET /api/v1/events` keeps a `text/event-stream` open and sends, after
every committed change of your data, a `version` event carrying the same
number as `/version`; on seeing one, refetch `/today` (or whatever you
show). A `: ping` comment every 25 s keeps proxies from closing the
connection; the stream also starts with `retry: 3000` so a standard
EventSource reconnects after three seconds. Compare versions for
inequality only. The stream also counts as presence: opening it nudges
the broker syncs like a `/version` poll does.

```
retry: 3000

event: version
data: {"v":137}

: ping

event: version
data: {"v":138}
```

### Nudges

The guardrail's nudges (Settings > Guardrail: "no stop recorded", "near
the max loss", "stop time", and the coach's live warnings) are rows the
phone can list and acknowledge. `GET /api/v1/nudges?day=today` lists a
day's nudges newest first:

```jsonc
{"day": "2026-10-15", "unseen": 1, "items": [
  {"id": 912, "day": "2026-10-15", "key": "max-loss-near", "ref": "7", "severity": "warn",
   "title": "Near the max loss on APEX-1", "detail": "Down 240.00 of the 300.00 the plan allows today.",
   "lesson": "Stop after two losers.", "href": "/journal/2026-10-15", "route": "day/2026-10-15",
   "createdAt": "2026-10-15T14:30:12Z", "deliveredAt": null, "seenAt": null}
]}
```

`route` is where the nudge leads in the app (`today`, `day/<date>`,
`trade/<id>`, `position/<account>/<symbol>`), derived from `href` on the
server so both clients agree. `POST /api/v1/nudges/{id}/seen` marks one
acknowledged (`{"id": 912, "seen": true}`; `false` when it was seen
before or is not yours); `POST /api/v1/nudges/seen` with `{"ids": [...]}`
marks several. `deliveredAt` is set when a web page took the nudge (that
also counts as seen); `seenAt` when the trader acknowledged it anywhere.

### Levels (the day map)

`GET /api/v1/levels/{market}?day=YYYY-MM-DD` is the day map of one market
(`ES`, `NQ`, `YM`, `RTY`, `CL`, `GC`; micros fold into their market) for a
trading day, today by default: the reference levels, each with a `source`
(`bars`: the journal's own math from its bar cache; `chart`: the session map
a MotiveWave chart posted; `gex`: the gamma engine), the `zones` they fold
into ranked by score, the `expected` range (the session anchor ± the daily
ATR), the `atr`, and the `dayType` (inside, outside, NR7 or wide prior day,
strong or weak close, the gap, the overnight against the prior range, a lean
with its reasons). Levels are descending by price. Kinds: `pdh`, `pdl`,
`pdc`, `onh`, `onl`, `orh`, `orl`, `poc`, `vah`, `val`, `vwap`, `vwap_band`,
`atr`, `pivot`, `camarilla`, `period`, `swing`, `gap`, `round`, `gamma`. The
scores rank, they do not forecast. A past day answers the map as it stood
at that session's close when one was recorded.

`POST /api/v1/levels/{market}` is the feed a chart posts: the contract, an
RFC 3339 `asOf`, the `mark` (last price) and optional `session`, `vwap`,
`atr` and `study` blocks; the full contract is in
`docs/api-handoff-002-levels-feed.md`. The trading day is derived from
`asOf` (17:00 Chicago rolls to the next day). Every accepted post is kept
for thirty days; the newest `asOf` per market and day is what the map
shows, and on the same second the post carrying more fields. The feed is
shared across the instance's users: a chart's session map is market data,
so a post under any user's key overlays every user's map (the posting key
is recorded on the post). `202 {"ok": true, "market": "ES", "tradeDay": "...", "receivedAt":
"...", "newest": true}`; `400` on a shape problem (missing fields, a clock
off by more than five minutes forward or three days back, a price that is
not positive), `413` over 16 KB. Any key may post. The MCP tool
`get_levels` answers the same map.

### Devices and push

The phone app registers for push after pairing; the server then sends it
what deserves attention (the guardrail's nudges, a closed trade, a silent
sync after any change, the Session Live Activity). This section is the
platform-neutral contract: a second client would implement the same.

**Server side.** The server speaks APNs over HTTP/2 with the developer
account's `.p8` key (`apns:` in config.yaml, or `TJ_APNS_KEY`). `GET
/api/v1/me` says in `features.push` whether this server can send; phones
register either way and their settings are kept, so push starts the
moment a key is configured.

**Registration** (`POST /api/v1/devices`, **device key**), once per
(server, token), and again when the token or the settings change:

```jsonc
{"token": "6f1a…", "name": "the trader's iPhone", "sandbox": true, "bundleId": "nl.w4j.tradingjournal",
 "prefs": {"kinds": {"nudge": {"timeSensitive": true}, "tradeClosed": {"sound": "none"}},
           "noActivity": false, "finishedAsNotification": false, "quietOutsideRth": false}}
// 200 {"device": {...}, "push": true}
```

`sandbox` is true for a Debug build installed from Xcode, whose tokens
belong to Apple's sandbox service; TestFlight and App Store builds use
production. One key serves both, so a phone says which. A token Apple
reports as gone (`410`) is forgotten. `GET /api/v1/devices` lists the
user's phones; `DELETE /api/v1/devices/{token}` forgets one (sign-out).

**Settings** (`prefs`) are the phone's notification preferences, owned by
the app and sent whole, with the registration and on every change (`PUT
/api/v1/devices/{token}/prefs`). The server filters before each push;
iOS cannot drop a push once it arrived. Kinds: `nudge` (a guardrail
nudge: the trader is needed; time-sensitive by default, the app's
`tj-nudge.caf` sound), `tradeClosed` (a trade closed; passive, silent by
default), `planMissing` (the open is near and no plan is written; opt-in,
off until the phone sends it). Per kind: `off`, `sound` (`tj`, `default`,
`none`), `timeSensitive`. `noActivity` turns the Session activity off for
the phone; `finishedAsNotification` keeps a closed trade a notification
while the activity shows (otherwise it rides the activity as its alert);
`quietOutsideRth` drops the closed-trade alerts outside regular hours.

**What is pushed.** Three things, each filtered by the phone's settings:

- A **nudge**, the moment the guardrail records it (`kind: "nudge"`):
  title = the nudge's title, subtitle = the contract and "open" for a
  position nudge or the day ("Wed 15 Oct") otherwise, body = the detail
  with " — Your lesson: …" when the trader has one. Category `TJ_NUDGE`,
  or `TJ_POSITION` when the nudge is about an open position;
  `thread-id: nudges`, `collapse-id: nudge:<id>`, time-sensitive and the
  `tj-nudge.caf` sound by default, `mutable-content`, badge = the unseen
  count. Custom keys: `kind`, `id`, `key`, `route`, `day`, `lesson`.
- A **closed decision**, on the tick after it closed (`kind: "trade"`;
  only trades closed in the last twenty minutes, so an import of a
  finished session announces nothing). A lone trade: title `MES short`,
  subtitle the account, body `+71.00 · 14:29`. A copy set is told once,
  not once per account: title `MES short ×5`, subtitle `5 accounts`, body
  the summed net and the first exit; a leg that lands a tick later
  re-tells the whole decision under the same `collapse-id`
  (`trade:<first leg>`), replacing the banner. Category `TJ_TRADE`,
  `thread-id: trades`, passive and silent by default. Custom keys:
  `kind`, `tradeId` (the first leg), `route` (`trade/<id>`),
  `symbolRoot`, `direction`, `netPnl` (the decision's), `accountId`,
  `legs`, and `setId` for a set. Dropped outside regular hours when
  `quietOutsideRth` is set.
- A **silent sync** after every change of the data (APNs `background`,
  `content-available`, priority 5): `{"kind": "sync", "v": 138, "badge":
  1, "unseen": [912]}`. The app removes its delivered nudge
  notifications whose id is not in `unseen`, sets the badge and refreshes
  what it shows. Each sync carries the whole waiting state, so a delayed
  or merged sync loses nothing.
- Once a trading day, between 08:15 and 08:30 Chicago, a phone that opted
  into `planMissing` hears "No plan for today" (category `TJ_NUDGE`,
  `route: plan`) when the guardrail is on and no plan is written.

A server start announces nothing: the trades of the last minutes are
taken as known. Every push a phone did not get is logged with why.

**Live Activities.** The app reports its push-to-start token and every
running activity with its update token, whole, after each change:
`PUT /api/v1/devices/{token}/activity` with `{"startToken": "…",
"activities": [{"id": "…", "token": "…", "kind": "session"}]}`. What the
server pushes to them is described next.

**The Session activity.** One Live Activity per trading day (attributes
type `SessionActivityAttributes`: `{"day", "tz", "rthStart", "rthEnd", "exchangeTz"}`,
the hours as unix seconds and `exchangeTz` the exchange's zone, so a face can show
the session in exchange time and the phone's), pushed as ActivityKit payloads (APNs `liveactivity`,
topic `<bundle id>.push-type.liveactivity`) with this content state,
every field always present, under 4 KB:

```jsonc
{"realized": 120.5, "unrealized": 13.75, "trades": 4, "wins": 3, "losses": 1,
 "decisions": 2, "setWins": 2, "setLosses": 0,                       // the same per decision: a copy set counts once
 "maxTrades": 6, "lossUsed": 84.2, "maxLoss": 300,                   // 0 when the plan has no cap or budget
 "positions": [{"a": 7, "s": "MESZ6", "d": "long", "q": 3, "avg": 7766.25, "last": 7767.25, "u": 13.75, "tts": 49, "beyond": false, "set": [7, 9]},
               {"a": 9, "s": "MESZ6", "d": "long", "q": 3, "avg": 7766.50, "last": 7767.25, "u": 11.25, "tts": 48, "beyond": false, "set": [7, 9]}],
 "openCount": 2, "openSets": 1, "nudges": 1, "lastFill": 1760530000, "updated": 1760530060, "stale": false, "alert": ""}
```

`positions` are legs: `set` is the copied decision a leg belongs to, the
sorted account ids of every leg (its own included, the same value on each)
and empty for a position on its own, so a phone that groups its trades by
set can fold the legs into one row. The legs are ordered by the decision
with the most unrealised at stake, kept together, and the state carries
the legs of at most three decisions (twelve legs in all), whole sets
only. `openCount` counts legs, `openSets` decisions.

The server **starts** it by push-to-start on the first fill of the day
(a closed trade or an open position, during Globex hours), once per
phone and day, with the alert "Session" and a one-line body; the phone
reports the new activity's update token through `PUT …/activity`. It
**updates** it after every change of the data (priority 10) and every
five minutes while anything is open (prices move without fills, priority
5), always with a stale date ten minutes out, so a phone that misses an
update greys the card. A **closed trade** rides the activity as its
alert (title `MES short`, body `+71.00 · APEX-1`, the kind's sound
inside the alert, `alert` set in the state) plus a badge-only push, so a
finish is one thing on the Lock Screen, not two; `finishedAsNotification`
keeps it a notification. It **ends** once the day is flat and ten
minutes have passed since the last fill after 15:00 Chicago, or at the
16:00 Globex break, with a dismissal date fifteen minutes out; a later
fill starts it again. `noActivity` turns all of this off for a phone; a
refused activity token is dropped alone.

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

The server carries the review method, so a client needs no skill of its own:

- **Instructions** at `initialize`: who the agent is, where the method is,
  the marker line and the write rules.
- **Prompts** (one per mode, listed in Claude Desktop's connector menu):
  `review_day(date?, force?)`, `review_entry(entryId, force?)`, `today`,
  `review_week(week?)`, `plan_day(day?)`, each also taking `language?`
  (answer this one in another language than the trader's setting). Each
  returns one user message with the mode's steps, the arguments filled in,
  the trader's profile, their review language and the marker.
- **Resources**: `trading-journal://method` (the method as markdown, in the
  trader's language, with their profile), `trading-journal://method/labels`
  (the vocabulary), `trading-journal://lessons` (the open lessons grouped by
  mistake plus the last ninety days), `trading-journal://playbooks` (the
  active strategies with their playbooks) and `trading-journal://profile`
  (the trader's style, the evidence they weigh most, their markets, their
  standing instructions and language, plus the block to put in front of a
  review).
- **Tools, 27 in all.** Reading: `server_info` (the instance, the server
  version and timezone, the trader's language and profile, the marker, the
  method version and where to read it), `get_review_method(language?)` (the
  method for clients that read neither instructions nor resources, ChatGPT
  among them),
  `list_lessons`, `list_trades`, `get_trade`, `list_positions`,
  `list_journal`, `get_journal_entry`, `list_strategies`, `get_strategy`,
  `get_chart_image` (the rendered PNG as image content), `get_daily_stats`,
  `get_analytics_summary`, `get_coach_checks`, `get_day_plan`,
  `get_weekly_digest` and `get_levels`. Writing: `prepare_day` (cluster a
  day's copytrader fan-out, create the entries to review, the daily-review
  entry point), `create_journal_entry`, `append_note` (a trailing
  `<!-- tj-ai:lesson … -->` line is taken off the note and recorded in the
  lessons ledger), `assign_strategy`, `set_tag` and `add_labels` (all
  only-if-unset, never overwrite), `set_day_plan` and `set_position_plan`
  (recorded with source `api` until the trader saves), and the library:
  `create_strategy` and `update_strategy` (name, playbook, checklist rules
  and drawings or chart images as base64; update replaces the name, body or
  checklist it is given and appends the rest, so it is the one tool that
  declares itself destructive and the client asks). Every tool mirrors a
  REST endpoint documented above; the `/mcp` tool list carries the
  schemas.

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

The **`trading-journal-analyst`** skill (`.claude/skills/trading-journal-analyst/`)
is a pointer: it connects, calls `server_info`, reads the method from the
server and follows it, so the method exists once, on the server, versioned.

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
| `409` | Conflict - a singleton entry already exists (carries `entryId`); or, for prepare-day only, the user's in-app AI day review is running |
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

- Keys are **full-access** to their user's data (no per-key scopes yet);
  the one distinction is the key's kind. An **agent key** writes additively
  only (create an entry, append a note, set a tag or strategy only if
  unset, save a plan as `api`): it never edits or deletes. A **device key**
  (a paired phone) may also edit and delete journal entries, add and
  delete balance anchors, change settings and manage push devices, and its
  plan and journal writes count as the trader's own.
- Nothing can create or modify trades, executions or accounts: those come
  from the imports and the live sync.
- Trades are read-only and the list is page-based (fixed page size 50).
- No rate limiting (intended for a private network / trusted agents).
- The PNG is a single panel; the JSON `chart` field carries every panel.
- Daily stats come from the `daily_stats` cache (rebuilt after every import
  or recompute) - the Calendar's numbers - and are capped at 400 days per
  request.
- `openapi.json` needs a key like everything else; while signed in to the
  web UI the same document is at `/settings/api-docs/openapi.json`, and this
  reference is rendered under **Settings → API docs**.
