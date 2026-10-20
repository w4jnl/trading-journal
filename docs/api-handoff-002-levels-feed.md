# API handoff 002 — the levels feed from MotiveWave

Implementation brief for the agent building the **W4J Journal Feed** study in
the MotiveWave extensions jar (`Motivewave-Daily-ATR`, the `w4j-mw-extensions`
mono-repo). The journal (`w4jnl/trading-journal`) is adding a Levels page, the
day map: one level set per market per trading day, computed by the journal
from its own bar cache and overlaid by what a running chart knows better. This
document is the **contract** for that overlay. Field names and semantics are
final; the journal side is built to match this document. Questions and
contract complaints come back to the journal side; do not work around the API.

**Date:** 2026-10-20 · **From:** trading-journal · **Concerns:** a MotiveWave
study posting the session map · **Version:** API v1; the endpoint ships in the
first journal release after v2.19.0 (branch `levels/day-map`) · **Status:**
contract fixed, journal implementation in progress; the design is in the
"Levels: the day map" doc.

**Goal:** while a 1-minute ES or NQ chart runs, the study posts the session map
the chart has (prior session, overnight, opening range, VWAP, volume profile,
daily ATR bands, the last price) once a minute. The journal keeps the newest
per market and trading day, draws it on the Levels page with source `chart`,
and falls back to its own bar math when nothing has arrived.

## Connection

Same as handoff 001: the API at `/api/v1`, every path key-authenticated,
`Authorization: Bearer tj_…` (preferred) or `X-API-Key: tj_…`. A key of kind
`agent` is the right one here (Settings → API keys → Generate key); a device
key also works. The study posts to the primary journal, and optionally mirrors
to a dev instance the way GexLevels mirrors marks (separate URL, token and
backoff; a broken dev instance never stalls the primary).

| Environment | Base URL |
|---|---|
| Dev | `http://127.0.0.1:8080` (the journal run locally) |
| Prod | `https://YOUR_HOST` (behind Traefik, key still required) |

## The endpoint

### `POST /api/v1/levels/{market}`

`market` is the underlying root the journal keys markets by: `ES`, `NQ`, `CL`,
`GC`, `YM`, `RTY`. A chart on a micro (MES, MNQ) posts under its full-size
root; the journal folds micros into the same map, as copy sets already do.

Body: JSON, at most 16 KB. **Every block is optional except `contract`,
`asOf` and `mark`**: send what the chart knows, leave out what it does not.
All prices are futures points of the posted contract. Times are RFC 3339 UTC.

```json
{
  "contract": "ESZ6",
  "asOf": "2026-10-20T13:31:00Z",
  "mark": 6786.75,
  "session": {
    "pdh": 6801.25, "pdl": 6742.00, "pdc": 6788.50,
    "onh": 6795.75, "onl": 6771.25,
    "poc": 6780.25, "vah": 6792.00, "val": 6768.50, "profileApprox": true,
    "or": { "minutes": 15, "high": 6791.00, "low": 6779.50, "final": true }
  },
  "vwap": { "value": 6784.10, "sd1": 4.6, "sd2": 9.2 },
  "atr": {
    "daily14": 58.25, "method": "smma",
    "anchor": 6788.50, "anchoredAt": "rth",
    "mults": [0.25, 0.5, 0.75, 1.0]
  },
  "study": { "name": "W4J Journal Feed", "version": "1.0", "barSize": "1m" }
}
```

| Field | Meaning |
|---|---|
| `contract` | the chart's contract code, e.g. `ESZ6` |
| `asOf` | the time of the chart's newest bar (or tick) the values were read at; the journal derives the CME trading day from it (17:00 Chicago rolls to the next day) |
| `mark` | the last price on the chart; the journal uses it as the live price for this market |
| `session.pdh`, `pdl`, `pdc` | the prior RTH session's high, low and close (settle-style close) |
| `session.onh`, `onl` | the overnight (Globex) high and low of the current trading day, so far or final |
| `session.poc`, `vah`, `val` | the prior RTH session's volume point of control and value area; `profileApprox: true` when built from bar ranges rather than ticks (as SessionLevels flags it) |
| `session.or` | the opening range of the current RTH: `minutes` (5, 15 or 30), `high`, `low`, `final` false while it is still forming |
| `vwap` | the current trading day's session VWAP and the standard deviations of its bands (`sd1`, `sd2`), in points, so the journal draws value ± sd |
| `atr.daily14` | the daily ATR the chart uses; `method` `smma` or `sma` |
| `atr.anchor`, `anchoredAt` | the price the bands are drawn from and which phase it is: `eth` (the trading day's 17:00 Chicago open, pre-market) or `rth` (the 08:30 Chicago open, once reached) |
| `atr.mults` | the multiples the study draws; the journal draws the same |
| `study` | the study's name, version and chart bar size, for the page's provenance line |

Responses:

| Code | Body | When |
|---|---|---|
| `202` | `{"ok": true, "market": "ES", "tradeDay": "2026-10-20", "receivedAt": "…", "newest": true}` | stored; `newest` false when a payload with a later `asOf` was already held for that market and day (yours is kept in the log, not shown) |
| `400` | `{"error": "…"}` | bad JSON, unknown market, missing `contract`/`asOf`/`mark`, `asOf` more than 5 minutes in the future or older than 3 days, a price that is not a positive number |
| `401` | `{"error": "…"}` | key problems, as in handoff 001 |
| `413` | | body over 16 KB |

### `GET /api/v1/levels/{market}?day=YYYY-MM-DD`

The read side, same release: the journal's merged map for the market and day
(today by default), with every level's `source` (`chart`, `bars` or `gex`) and
the newest feed payload it used under `feed`. The study does not need it; it is
there to verify a post landed (`feed.asOf` equals what you sent) and for the
assistants.

## Semantics the study should respect

- **Cadence.** Once a minute while the chart runs, and once more at the
  session rollover (17:00 Chicago) so the day's final map is on record. Posting
  faster is accepted and pointless. Nothing is posted when nothing changed is
  not required: the journal dedups by `asOf`.
- **Latest wins.** The journal keeps the newest `asOf` per market and trading
  day as the map's chart overlay and logs every accepted post for 30 days.
  Several charts may post for the same market (two ES charts, an MES chart):
  all are accepted, the newest `asOf` shows; on the same second the post
  carrying more fields shows (`newest` in the answer says which). No
  coordination needed, though one feed chart per market is the intent.
- **Shared.** The feed is instance-wide: a post under any user's key overlays
  every user's map (the posting user is recorded). One study instance feeds
  the whole journal.
- **Partial is fine.** Before the RTH open there is no `or`; before the
  profile is built there is no `poc`. Leave the block out rather than sending
  zeros. A zero or negative price is a 400.
- **Backoff.** On a 5xx or a network failure, back off as GexLevels does for
  marks (separate backoff per destination). On a 400, log the body and stop
  sending that shape: it is a bug on one side, not a transient.
- **Reuse, do not copy.** The values should be the ones the drawing studies
  show: SessionLevels for the prior session, the profile and the opening
  range; `SessionLevels.computeDailyAtr` and the anchor logic of
  DailyAtrLevels for the bands; VwapMath for VWAP. The HTTP path of GexLevels
  (client, token header, backoff, dev mirror) is the model; a shared
  JournalClient in the jar that both studies use is the clean cut.
- **Settings block.** Journal URL, API key, market (default from the chart's
  instrument root, micros folded), post interval (default 60 s), opening
  range minutes (default 15), dev mirror URL and token, and a master on/off.
- **No drawing.** A status label at most ("journal: posted 13:31" / "journal:
  backing off"); the drawing studies keep drawing.

## Sample

Captured from a scratch instance (synthetic values around the live ES price,
not a trading record). The post:

```json
{
  "contract": "ESZ6",
  "asOf": "2026-10-20T10:29:33Z",
  "mark": 7733.25,
  "session": {
    "pdh": 7774.75,
    "pdl": 7718.75,
    "pdc": 7734.75,
    "onh": 7753.75,
    "onl": 7721.75,
    "poc": 7741.75,
    "vah": 7756.75,
    "val": 7728.75,
    "profileApprox": true,
    "or": {
      "minutes": 15,
      "high": 7738.75,
      "low": 7727.75,
      "final": true
    }
  },
  "vwap": {
    "value": 7734.0,
    "sd1": 4.5,
    "sd2": 9.0
  },
  "atr": {
    "daily14": 80.5,
    "method": "sma",
    "anchor": 7729.75,
    "anchoredAt": "rth",
    "mults": [
      0.25,
      0.5,
      0.75,
      1.0
    ]
  },
  "study": {
    "name": "W4J Journal Feed",
    "version": "1.0",
    "barSize": "1m"
  }
}
```

The answer: `202 {"ok": true, "market": "ES", "newest": true, "tradeDay": "2026-10-20", "receivedAt": "2026-10-20T10:30:33Z"}`.
The read side afterwards (`GET /api/v1/levels/ES`), cut to the parts the
post changed; `sources.chart` is true, `sources.contract` names the chart,
and every level the post carried has `"source": "chart"`:

```json
{
  "market": "ES",
  "day": "2026-10-20",
  "asOf": "2026-10-20T12:30:35.036252+02:00",
  "last": 7733.25,
  "atr": 80.5,
  "atrMethod": "sma",
  "anchorRth": 7729.75,
  "anchor": "rth",
  "expected": {
    "low": 7649.25,
    "high": 7810.25
  },
  "sources": {
    "bars": true,
    "barsTf": "1m",
    "dailyBars": 109,
    "chart": true,
    "chartAt": "2026-10-20T10:29:33Z",
    "contract": "ESZ6",
    "gex": false
  },
  "levels": [
    {
      "kind": "atr",
      "name": "ATR +1 (RTH)",
      "price": 7810.25,
      "tf": "1d",
      "source": "bars",
      "weight": 2.25,
      "touches": 0,
      "score": 2.25
    },
    {
      "kind": "atr",
      "name": "ATR +0.75 (RTH)",
      "price": 7790.25,
      "tf": "1d",
      "source": "bars",
      "weight": 1.7999999999999998,
      "touches": 0,
      "score": 1.7999999999999998
    },
    {
      "kind": "pdh",
      "name": "prior day high",
      "price": 7774.75,
      "tf": "1d",
      "source": "chart",
      "weight": 4.5,
      "touches": 8,
      "score": 7.875
    },
    {
      "kind": "atr",
      "name": "ATR +0.5 (RTH)",
      "price": 7770,
      "tf": "1d",
      "source": "bars",
      "weight": 1.7999999999999998,
      "touches": 13,
      "score": 3.1499999999999995
    },
    {
      "kind": "atr",
      "name": "ATR +0.25 (RTH)",
      "price": 7750,
      "tf": "1d",
      "source": "bars",
      "weight": 1.2000000000000002,
      "touches": 47,
      "score": 2.1000000000000005
    }
  ],
  "zones": [
    {
      "low": 7724.69375,
      "high": 7748.25,
      "center": 7734.4155022658615,
      "score": 57.93,
      "kinds": [
        "camarilla",
        "vwap_band",
        "round",
        "swing",
        "orl",
        "val",
        "vwap",
        "pdc",
        "orh",
        "poc",
        "pivot"
      ],
      "levels": [
        {
          "kind": "camarilla",
          "name": "Camarilla L3",
          "price": 7724.69375,
          "tf": "1d",
          "source": "bars",
          "weight": 1.5,
          "touches": 19,
          "score": 2.625
        },
        {
          "kind": "vwap_band",
          "name": "VWAP -2 sd",
          "price": 7725,
          "tf": "1m",
          "source": "chart",
          "weight": 1,
          "touches": 20,
          "score": 1.75
        },
        {
          "kind": "round",
          "name": "round 7725",
          "price": 7725,
          "tf": "",
          "source": "bars",
          "weight": 0.75,
          "touches": 20,
          "score": 1.3125
        },
        {
          "kind": "swing",
          "name": "30m swing low 20 Oct 01:00",
          "price": 7727.5,
          "tf": "30m",
          "source": "bars",
          "weight": 1.2000000000000002,
          "touches": 25,
          "score": 2.1000000000000005,
          "at": "20 Oct 01:00"
        },
        {
          "kind": "swing",
          "name": "1h swing low 20 Oct 01:00",
          "price": 7727.5,
          "tf": "1h",
          "source": "bars",
          "weight": 1.5,
          "touches": 25,
          "score": 2.625,
          "at": "20 Oct 01:00"
        },
        {
          "kind": "swing",
          "name": "4h swing low 19 Oct 07:00",
          "price": 7727.5,
          "tf": "4h",
          "source": "bars",
          "weight": 1.9500000000000002,
          "touches": 25,
          "score": 3.4125000000000005,
          "at": "19 Oct 07:00"
        },
        {
          "kind": "orl",
          "name": "opening range low (15m)",
          "price": 7727.75,
          "tf": "1m",
          "source": "chart",
          "weight": 2,
          "touches": 26,
          "score": 3.5
        },
        {
          "kind": "val",
          "name": "prior day value area low (approx.)",
          "price": 7728.75,
          "tf": "1d",
          "source": "chart",
          "weight": 3,
          "touches": 29,
          "score": 5.25
        },
        {
          "kind": "vwap_band",
          "name": "VWAP -1 sd",
          "price": 7729.5,
          "tf": "1m",
          "source": "chart",
          "weight": 1,
          "touches": 33,
          "score": 1.75
        },
        {
          "kind": "swing",
          "name": "1h swing low 19 Oct 14:00",
          "price": 7730.75,
          "tf": "1h",
          "source": "bars",
          "weight": 1.5,
          "touches": 40,
          "score": 2.625,
          "at": "19 Oct 14:00"
        },
        {
          "kind": "vwap",
          "name": "session VWAP",
          "price": 7734,
          "tf": "1m",
          "source": "chart",
          "weight": 2,
          "touches": 49,
          "score": 3.5
        },
        {
          "kind": "pdc",
          "name": "prior day close",
          "price": 7734.75,
          "tf": "1d",
          "source": "chart",
          "weight": 3,
          "touches": 51,
          "score": 5.25
        },
        {
          "kind": "vwap_band",
          "name": "VWAP +1 sd",
          "price": 7738.5,
          "tf": "1m",
          "source": "chart",
          "weight": 1,
          "touches": 54,
          "score": 1.75
        },
        {
          "kind": "orh",
          "name": "opening range high (15m)",
          "price": 7738.75,
          "tf": "1m",
          "source": "chart",
          "weight": 2,
          "touches": 55,
          "score": 3.5
        },
        {
          "kind": "poc",
          "name": "prior day POC (approx.)",
          "price": 7741.75,
          "tf": "1d",
          "source": "chart",
          "weight": 3.75,
          "touches": 57,
          "score": 6.5625
        },
        {
          "kind": "vwap_band",
          "name": "VWAP +2 sd",
          "price": 7743,
          "tf": "1m",
          "source": "chart",
          "weight": 1,
          "touches": 56,
          "score": 1.75
        },
        {
          "kind": "pivot",
          "name": "pivot",
          "price": 7743.833333333333,
          "tf": "1d",
          "source": "bars",
          "weight": 2.25,
          "touches": 54,
          "score": 3.9375
        },
        {
          "kind": "swing",
          "name": "30m swing high 20 Oct 03:30",
          "price": 7745.25,
          "tf": "30m",
          "source": "bars",
          "weight": 1.2000000000000002,
          "touches": 55,
          "score": 2.1000000000000005,
          "at": "20 Oct 03:30"
        },
        {
          "kind": "swing",
          "name": "1h swing high 19 Oct 18:00",
          "price": 7748.25,
          "tf": "1h",
          "source": "bars",
          "weight": 1.5,
          "touches": 49,
          "score": 2.625,
          "at": "19 Oct 18:00"
        }
      ]
    }
  ],
  "dayType": {
    "range": "inside",
    "close": "mid",
    "relation": "inside",
    "bias": "neutral",
    "sentence": "Prior day: inside day; overnight inside the prior range. No lean."
  }
}
```
