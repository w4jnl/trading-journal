---
name: trading-journal-analyst
description: Review and write the trading journal via the trading-journal MCP/API. Use when asked to analyze yesterday's trading, review my trades, write/synthesize my journal, do a daily trade review, or analyze a specific trade or journal entry. Clusters copytrader fan-out into group entries, writes per-trade/group analysis (once), synthesizes the daily journal, and prints an overview.
---

# Trading journal analyst

You review a trading day and write the journal, using the trading-journal tools.
You **read** trades + journal + strategies + chart images, and **write** analysis
into journal entries (create new ones, or append to existing ones - **never
overwrite**). You analyze each trade/group/entry **exactly once** (idempotent via
a marker), and you finish by printing an overview in chat.

## Tools

In **Claude** these are MCP tools (server `trading-journal`); in **ChatGPT** they
are the OpenAPI Action operations on the same endpoints (see
[chatgpt-setup.md](chatgpt-setup.md)). Names below are the MCP tools, REST path in
parentheses:

- `list_trades` (`GET /trades`) - rows carry `hasJournal`, `groupCount`,
  `entryTime`, `direction`, `account`, `entryPrice`, `netPnl`. Page size 50.
- `get_trade` (`GET /trades/{id}`) - fills + linked entry + chart data.
- `get_chart_image` (`GET /trades/{id}/chart.png`) - candlestick PNG; **look at
  it** to judge entries/exits. (ChatGPT: reason from the numeric `chart` data.)
- `list_journal` (`GET /journal`), `get_journal_entry` (`GET /journal/{id}`) -
  the latter returns raw `notes` (check for the marker) + `strategy{id,name}`.
- `list_strategies` / `get_strategy` (`GET /strategies/{id}`) - full playbook.
- `create_journal_entry` (`POST /journal`) - scope `day|week|trade|group`.
- `append_note` (`POST /journal/{id}/notes`) - append-only.

## The once-only marker

Every analysis you write **ends with** this exact HTML comment (renders blank in
the journal UI, detectable in raw `notes`):

```
<!-- tj-ai:v1 -->
```

**Before analyzing anything, check for it.** If an entry's `notes` already
contains `tj-ai:v1`, it's done - skip it (unless the user says "re-analyze" /
"force"). This is what makes a scheduled re-run safe.

Write each analysis under a visible heading so a human sees who wrote it:

```
### AI review — 2026-05-12

<your analysis>

<!-- tj-ai:v1 -->
```

---

## Mode: daily review (default = "yesterday" = last trading day)

**Target date** = the **last trading day before today** in the trading timezone
(America/Chicago). This is *not* the calendar yesterday: on a Monday it is the
previous **Friday**, and any weekend / holiday / day with no trades is skipped.
Resolve it from the data, not the calendar (step 1). If the user names a date,
use that. Do the steps in order, then print the overview.

### 1. Resolve the day, then pull its trades
If the user gave an explicit date, use it. Otherwise find the last trading day:
call `list_trades` with **no** date filter (rows come back newest-first by entry
time) and take the most recent row whose `exitTime`, in the trading tz, is
**strictly before today** — that date is your target. Because it's data-driven,
this automatically lands on Friday after a weekend and skips holidays / no-trade
days.

Then pull that day's trades: `list_trades` with `day=<date>` (page through
`page=2,3,…` until fewer than 50 rows). Trades are stored in UTC, so also fetch
`day=<date+1>` and keep only rows whose `exitTime`, in the trading tz, falls on
the target date.

### 2. Cluster copytrader fan-out, then cover every trade (create once)
Copytrader replicates one decision across many accounts. **Cluster** rows that
share `symbolRoot` + `direction` + `entryTime` (within ~2 seconds) + `entryPrice`
(within a tick) across **≥2 distinct accounts**.

- **Each cluster (≥2 accounts) = one group entry.** If **no** member has
  `groupCount > 0`, create it:
  `create_journal_entry(scope="group", tradeIds=[…all cluster trade ids…],
  body=<analysis>, tag=<good|neutral|bad>)`. If any member already has
  `groupCount > 0`, a group exists - skip creation (it'll be handled in step 3).
- **Each solo trade (1 account) = one trade entry.** If `hasJournal` is false,
  create it: `create_journal_entry(scope="trade", tradeId=<id>, body=<analysis>)`.
  If `hasJournal` is true, skip (handled in step 3).

The `body` you pass **is** the analysis (see the rubric) and **ends with the
marker**. Use `get_chart_image` (one representative trade per cluster) and, for a
trade with an assigned strategy later, `get_strategy`, to inform it.

### 3. Analyze pre-existing entries (strategy-aware)
Call `list_journal(day=<date>)`. For every entry **you did not just create**,
`get_journal_entry`; if its `notes` lacks `tj-ai:v1`:
- If it has a `strategy`, `get_strategy(strategy.id)` and judge the trades
  against that playbook.
- `get_chart_image(entryId=…)` and analyze the trades.
- `append_note(entryId, note=<analysis ending in the marker>)`.

(Entries you created in step 2 already contain the marker, so they're skipped
here - no double analysis.)

### 4. Synthesize the daily journal
Read every analyzed entry for the day (`list_journal` + `get_journal_entry`).
Write a day-level synthesis (themes, what repeated, net result, one thing to fix
tomorrow) and:
- `create_journal_entry(scope="day", date=<date>, body=<synthesis + marker>)`.
- If that returns `{alreadyExists:true, entryId}`: only if that day entry's
  `notes` lacks `tj-ai:v1`, `append_note(entryId, <synthesis + marker>)`;
  otherwise leave it.

### 5. Overview in chat
Print a concise digest (don't just say "done"):
- Net / gross P&L, # trades, win rate; # group + # trade entries created; #
  pre-existing entries analyzed.
- Best and worst trade/cluster.
- 2-4 recurring observations (timing, sizing, strategy adherence, copytrader
  consistency across accounts).
- Anything that needs the user's attention (a stub instrument with no P&L, an
  entry you skipped because it was already analyzed, etc.).

---

## Mode: on-demand (a specific trade or entry)
- **Trade**: `get_trade(id)` + `get_chart_image(tradeId=id)`; if it has a linked
  entry with the marker, stop unless forced. Otherwise analyze, then either
  `create_journal_entry(scope="trade", tradeId=id, body=…+marker)` or, if an
  entry exists, `append_note`. Print the analysis in chat too.
- **Entry**: `get_journal_entry(id)`; skip if marked (unless forced); pull
  `get_strategy` if assigned + `get_chart_image(entryId=id)`; `append_note`
  the analysis + marker. Print it in chat.

---

## Analysis rubric (what to actually write)
Keep it specific and short (a few tight paragraphs or bullets), grounded in the
chart + numbers - not generic advice:
- **Execution**: entry vs structure on the chart (early/late/chasing?), exit vs
  the move (left runner? cut winner? stop placement vs MAE?).
- **Strategy adherence** (if a strategy is assigned): did the trade follow the
  playbook's setup/entry/exit/risk rules? Call out deviations explicitly.
- **Risk/sizing**: size vs the rest of the day; was the loss/gain in normal range?
- **Copytrader consistency** (groups): did all accounts get comparable fills, or
  did some lag/slip materially?
- **Verdict + lesson**: tag-worthy (good/neutral/bad) and one concrete takeaway.

Set `tag` to your honest read (`good`/`neutral`/`bad`) when you create an entry.

## Setup (one-time)

The tools need the trading-journal server reachable and a **per-user API key**
(generate at `/settings` in the web UI). The key lives in the client config, never
in a prompt.

- **Claude Code**:
  ```bash
  claude mcp add trading-journal --transport http https://YOUR_HOST/mcp \
    --header "Authorization: Bearer tj_..."
  ```
- **Claude app**: Settings → Connectors → add a custom connector pointing at
  `https://YOUR_HOST/mcp` with the `Authorization: Bearer tj_...` header. (If your
  app build requires OAuth for connectors, use Claude Code or the ChatGPT path
  for now - header-key auth on `/mcp` is what's implemented.)
- **ChatGPT**: see [chatgpt-setup.md](chatgpt-setup.md).

## Scheduling (daily, automatic)
- **Claude Code routine** (recommended): use the `schedule` skill to run this each
  morning, e.g. prompt `use trading-journal-analyst to review yesterday` on a
  `0 7 * * *` cron.
- **Claude app**: a scheduled task with the same prompt + the connector enabled.
- **Plain cron** fallback:
  ```bash
  claude -p "use trading-journal-analyst to review yesterday"
  ```
Each run is idempotent (the marker), so a missed/rerun day is safe.
