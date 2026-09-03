# API handoff 001 — daily P&L and trade stats for a consumer app

Implementation brief for the agent building an app on top of the
trading-journal (`w4jnl/trading-journal`). The journal exposes a
per-user-key JSON API at `/api/v1`; this document is the **contract** for
the two endpoints such an app needs. Field names and semantics below are
final — the implementation is being built to match this document, not the
other way round. Questions or contract complaints come back to the journal
side; do not work around the API (in particular, never re-derive daily
numbers by summing `/api/v1/trades` yourself — see *Semantics*).

**Date:** 2026-09-02 · **From:** trading-journal · **Concerns:** a consumer
app showing daily P&L + trade stats · **Version:** API v1; the two endpoints
ship in the first journal release after v2.9.1 · **Status:** implemented
(branch `feature/api-daily-stats`, PR pending merge); a sample captured from
a live instance is at the end of this document.

**Goal:** the consumer renders, for any date range and optional account
filter, the same per-day P&L / trade-count / win-loss numbers the journal's
own Calendar shows — and learns about new data (an import, a recompute)
within one poll interval instead of re-pulling on a timer.

## Connection

| Environment | Base URL | Notes |
|---|---|---|
| Dev | `http://127.0.0.1:8080` | the journal run locally (`server.port` in `config.yaml`) |
| Prod | `https://YOUR_HOST` | behind Traefik; the API is still key-authenticated app-side on every path |

**Auth applies on every path**, including the discovery index and the
OpenAPI spec. Generate a key in the journal web UI: **Settings → API keys →
Generate key** (shown exactly once; only a hash is stored). Keys look like
`tj_` + a URL-safe base64 secret. Send it on every request as
`Authorization: Bearer tj_…` (preferred) or `X-API-Key: tj_…`. Query-string
keys are rejected. Failures are `401` with `{"error": "…"}`:

| Situation | `error` |
|---|---|
| No key, or it doesn't start with `tj_` | `missing or malformed API key` |
| Unknown or revoked key | `invalid API key` |
| The key's user has been deactivated | `account disabled` |

Keys are full-access to their user's data (no scopes) and there is no
rate limiting — the API is meant for a private network / trusted apps.
Everything you read is scoped to the key's user.

## The endpoints that matter

### `GET /api/v1/stats/daily`

Per trading day: trade count, winners/losers, net/gross/commission, win
rate, largest win/loss — plus totals over the range. Backed by the
journal's `daily_stats` cache, i.e. **the exact numbers the Calendar
page shows** (same account rules, same day bucketing).

Query parameters (all optional):

| Param | Format | Meaning |
|---|---|---|
| `from` | `YYYY-MM-DD` or `today` \| `yesterday` \| `last` | first day, **inclusive**, in the journal's trading timezone. Default: `to − 29 days` |
| `to` | same | last day, **inclusive**. Default: today (trading timezone) |
| `day` | same | shorthand for `from=to=day`; `day=today` is the intraday check |
| `filter` | `acc:<id>` or `type:<slug>`, repeatable | restrict to these accounts (OR-union). `type` slugs: `live`, `prop_live`, `prop_funded`, `prop_eval`, `paper` |
| `account_id` | integer | legacy alias for one `filter=acc:<id>` |
| `refresh` | `1` | pull the latest broker fills first and wait for them (bounded, ~15 s) before computing — for "right now" reads |

The keywords are resolved **server-side in the trading timezone**, so you
never need to know what "today" is there (`last` = the last trading day
before today with closed trades).

Span cap: **400 days** per request (page by month or quarter for history).

Response — a bare object (contract example; `//` comments are the
contract, not part of the JSON):

```jsonc
{
  "from": "2026-08-03",             // effective range, echoed even when defaulted
  "to": "2026-09-01",
  "timezone": "America/Chicago",    // the trading timezone every "date" is in
  "totals": {
    "trades": 41, "wins": 24, "losses": 15,
    "net": 1834.5, "gross": 2011.0, "commission": 176.5,
    "winRate": 0.615,               // wins / (wins + losses); null when both are 0
    "winDays": 12, "lossDays": 6,   // days with net > 0 / net < 0 (breakeven days in neither)
    "bestDay":  { "date": "2026-08-14", "net": 612.0 },   // null when there are no items
    "worstDay": { "date": "2026-08-21", "net": -388.0 },
    "largestWin": 402.5,            // single best trade in the range; null when no winning trade
    "largestLoss": -210.0           // single worst trade (negative); null when no losing trade
  },
  "items": [                        // one entry per day WITH closed trades, ascending by date
    {
      "date": "2026-08-03",
      "trades": 3, "wins": 2, "losses": 1,
      "net": 120.0, "gross": 132.5, "commission": 12.5,
      "winRate": 0.667,
      "largestWin": 95.0,
      "largestLoss": -40.0
    }
  ]
}
```

Errors (`{"error": "…"}`): `400` for a malformed `from`/`to`, `to` before
`from`, or a span over 400 days — the message says which. `401` per the
auth table. There is no `404` for "no trading that day": an empty range is
a normal `200` with `"items": []` and zeroed totals (`winRate`, `bestDay`,
`worstDay`, `largestWin`, `largestLoss` all `null`).

### `GET /api/v1/version`

```json
{ "v": 137 }
```

An opaque per-user data version. It changes after every mutation of the
user's data that commits (imports, recomputes, journal writes, account
changes). Response carries `Cache-Control: no-store`.

## Semantics you must not re-derive

- **A trade belongs to the day of its exit** (last closing fill),
  converted to the trading timezone in the `timezone` field. Not entry
  time, not UTC. The same trade can therefore sit on a different day than
  a UTC-based grouping of `/api/v1/trades` would put it — trust the
  endpoint.
- **Closed trades only.** Open positions have no P&L and are not counted.
- **Account rule (the Calendar rule).** Retired accounts never count.
  Accounts the user marked *excluded from stats* (typically prop evaluation
  accounts) are dropped from the default numbers **unless** the request
  names accounts via `filter=` — an explicit filter overrides the flag.
  Do not "helpfully" add excluded accounts back.
- **Win / loss / breakeven.** `wins` = trades with net > 0, `losses` = net
  < 0. Breakeven trades and trades whose instrument has no tick spec yet
  (net unknown) count in `trades` but in neither bucket and contribute 0
  to the money totals. Hence `wins + losses ≤ trades`, and `winRate`
  excludes breakevens.
- **`null` means absent, never zero.** Render it as "—". `largestWin`
  null = no winning trade that day; `winRate` null = no decided trades.
- **Sparse `items`.** Days with no closed trades are omitted, not zero
  rows. Fill gaps client-side if you chart a continuous axis (weekends and
  holidays are simply missing).
- **Money is in account currency as raw JSON numbers** (USD for all
  current instruments); `commission` is a positive cost, `net = gross −
  commission` per trade summed over the day. `largestLoss` is negative.
  Sums are floating-point (`11617.299999999992` is a legitimate value) —
  format to two decimals for display, exactly as the journal itself does.
- **Cache freshness.** `daily_stats` is rebuilt after every import /
  recompute, so the endpoint reflects the last import — exactly like the
  Calendar. Your own reads trigger those imports from the brokers (see
  *Change detection*); use `/api/v1/version` to know when to refetch.
- **Timezone changes.** If the journal's configured trading timezone is
  ever changed, historical day keys shift only after the user recomputes;
  a consumer should treat `timezone` as part of the data, not a constant.

## Change detection

Poll `GET /api/v1/version` (the journal's own pages poll every 10 s) and
refetch your stats when the value **differs** from the last one you saw.
Your polling is also what keeps the data fresh: every read of `/version`,
`/stats/daily` or the trades endpoints nudges the journal's broker syncs
(TopstepX, IBKR) for the key's user, throttled to at most one sync per
`api_sync_throttle_sec` (default 60 s) - so you never need to open the web
UI for new fills to arrive. The sync is asynchronous: new fills land a few
seconds after the nudge and change `/version`, which your loop then sees.
Rules: compare for inequality only — the counter is process-local and
restarts at zero when the journal restarts, so "changed" must never be
read as "increased". It is coarse (any mutation of the user's data bumps
it), so a bump is a hint to refetch, not a diff. Poll `version`, not
`stats/daily`: the former is an in-memory read with no database work.

## Conventions & stability

- JSON, camelCase, no envelope. Single resources are bare objects, lists
  are `{"items": [...]}`, errors are `{"error": "…"}` with a 4xx/5xx status.
- The stats vocabulary `trades / wins / losses / net / gross / commission`
  is the same object the journal endpoints return as `summary` on
  `GET /api/v1/journal` — one vocabulary across the API. (Per-trade money
  on `/api/v1/trades` is `netPnl` / `grossPnl` / `commission`, a
  different object.)
- Changes to v1 are **additive**: new fields may appear (ignore unknown
  keys); existing field names, types and null rules do not change. Pin
  `/api/v1` in your client.
- Dates (`date`, `from`, `to`) are calendar dates in the trading timezone;
  timestamps elsewhere in the API are RFC 3339 UTC.
- Machine-readable spec: `GET /api/v1/openapi.json` (OpenAPI 3.1, needs the
  key; the `servers[0].url` comes back absolute for the host you called),
  operations `getDailyStats` and `getVersion`. `GET /api/v1` is the
  discovery index. Full human reference: `API.md` in the journal repo,
  also served in-app at **Settings → API docs**.

## Testing

```bash
HOST=https://YOUR_HOST      # or http://127.0.0.1:8080
KEY=tj_...
A="Authorization: Bearer $KEY"

# Last 30 days, all counted accounts
curl -s -H "$A" "$HOST/api/v1/stats/daily" | jq '.from, .to, .timezone, .totals, (.items | length)'

# One calendar month, one account
curl -s -H "$A" "$HOST/api/v1/stats/daily?from=2026-08-01&to=2026-08-31&filter=acc:3" | jq '.totals'

# Contract checks
curl -s -H "$A" "$HOST/api/v1/stats/daily?from=2026-08-01&to=2026-08-31" \
  | jq -e '.items | all(.trades >= .wins + .losses)'          # true
curl -s -o /dev/null -w "%{http_code}\n" -H "$A" "$HOST/api/v1/stats/daily?from=nope"   # 400
curl -s -o /dev/null -w "%{http_code}\n" -H "$A" "$HOST/api/v1/stats/daily?from=2020-01-01&to=2026-01-01"  # 400 (span)

# Change detection
curl -s -H "$A" "$HOST/api/v1/version"
```

For a local integration test, fake the journal with a static server that
returns the example JSON above plus a `400` and a `401`, and drive your
client against it — the shapes here are exact.

## Acceptance

For any month and account filter, the consumer's month totals (`net`,
`trades`, `wins`, `losses`) equal the journal Calendar's month footer for
the same month and filter, to the cent. After a CSV import into the
journal, the consumer picks up the new day within one poll interval
without a manual refresh. Days with `null` fields render as "—", never as
0. Report any discrepancy back to the journal side with the exact request
URL and the date in question.

## Appendix — captured from a live instance

`GET /api/v1/stats/daily?from=2026-01-01&to=2026-09-02` against the run-skill
smoke instance (the repo's sample CSV exports imported; 18 prop-eval
accounts, none retired or excluded), captured 2026-09-02, abridged to the
first two of six items:

```json
{
  "from": "2026-01-01", "to": "2026-09-02", "timezone": "America/Chicago",
  "totals": {
    "trades": 474, "net": 11617.299999999992, "gross": 12879.49999999999, "commission": 1262.1999999999998,
    "wins": 436, "losses": 38, "winRate": 0.919831223628692,
    "winDays": 5, "lossDays": 1,
    "bestDay": { "date": "2026-05-08", "net": 7359.9700000000275 },
    "worstDay": { "date": "2026-05-06", "net": -1007.3599999999634 },
    "largestWin": 8857.5, "largestLoss": -1861.9399999999546
  },
  "items": [
    { "date": "2026-05-01", "trades": 79, "net": 342.66000000004516, "gross": 522.5000000000455, "commission": 179.83999999999997,
      "wins": 77, "losses": 2, "winRate": 0.9746835443037974, "largestWin": 1000.56, "largestLoss": -1861.9399999999546 },
    { "date": "2026-05-04", "trades": 41, "net": 403.29999999988183, "gross": 519.9999999998818, "commission": 116.7,
      "wins": 41, "losses": 0, "winRate": 1, "largestWin": 28.319999999986358, "largestLoss": null }
  ]
}
```

Note the all-win day: `losses: 0` gives `winRate: 1` and `largestLoss: null`
— null because there was no losing trade, not because the loss was zero.
`GET /api/v1/version` on the same instance returned `{"v":2}`.
