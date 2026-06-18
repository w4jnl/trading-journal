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

- `prepare_day` (`POST /journal/prepare-day`) - **the daily entry point.** Clusters
  the day's copytrader fan-out into **group** entries + solos into **trade**
  entries, CREATES the missing ones (idempotent), and returns them with
  `entryId, scope, created, alreadyAnalyzed, label, symbolRoot, direction, netPnl,
  tradeIds`. The server does the clustering and id handling - you only analyze the
  entries it returns. Defaults to the last trading day before today.
- `server_info` - service + instance label (e.g. prod/dev) + the authenticated
  user; call first if several connectors might be configured.
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
- `assign_strategy` (`POST /journal/{id}/strategy`) - set an entry's strategy
  **only if it has none** (never overwrites), on any entry when one clearly fits.
  Trade/group entries only (a day/week is rejected).
- `set_tag` (`POST /journal/{id}/tag`) - set `good|neutral|bad` **only if the
  entry has none** (never overwrites), on any entry when your analysis warrants a
  clear verdict.

## The once-only marker

Every analysis you write **ends with** this exact HTML comment (renders blank in
the journal UI, detectable in raw `notes`):

```
<!-- tj-ai-mcp:v1 -->
```

**Before analyzing anything, check for it.** If an entry's `notes` already
contains `tj-ai-mcp:v1`, it's done - skip it (unless the user says "re-analyze" /
"force"). This is what makes a scheduled re-run safe.

Write each analysis under a visible heading so a human sees who wrote it:

```
### AI review — 2026-05-12

<your analysis>

<!-- tj-ai-mcp:v1 -->
```

---

## Mode: daily review (default = "yesterday" = last trading day)

`prepare_day` does the date resolution, copytrader clustering, and entry creation
on the **server** - you never cluster trades or pass raw trade ids yourself; you
only analyze the entries it returns. The target date defaults to the **last
trading day before today** (trading tz - so on a Monday it's the previous Friday,
and weekends/holidays/no-trade days are skipped); pass an explicit `date` to
override. Do the steps in order, then print the overview.

### 1. Prepare the day
Call `prepare_day` (no args for the default last-trading-day, or
`date="YYYY-MM-DD"`). It clusters copytrader fan-out into group entries + solos
into trade entries, creates the missing ones, and returns `entries[]` - each
`{entryId, scope, created, alreadyAnalyzed, label, symbolRoot, direction, netPnl,
tradeIds}`. You work only from these entry ids.

### 2. Analyze each entry (once)
Fetch your strategies once with `list_strategies` (id + name + full playbook) so
you can match a trade to one.

For every returned entry with `alreadyAnalyzed: false`:
- `get_chart_image(entryId=…)` and **look at** the candles/markers;
  `get_journal_entry(entryId)` for its `notes` + any assigned `strategy`.
- **Classify (any entry, only-if-unset, only-if-fitting):** set a strategy and/or
  tag on an entry that has none yet — whether you just created it (`created: true`)
  or it pre-existed — but only when each clearly applies:
  - `assign_strategy(entryId, strategyId)` if one of your strategies *clearly*
    fits the trade; if nothing clearly fits, leave it unassigned — don't force it.
    Trade/group entries only (a day/week spans mixed setups and is rejected).
  - `set_tag(entryId, "good"|"neutral"|"bad")` if your analysis warrants a clear
    verdict; if it's ambiguous, leave it untagged.
  You **never overwrite** a strategy or tag that's already set (the user's or a
  prior run's) — both calls refuse to, as a backstop — so you only fill in blanks.
- Judge the trade against the relevant playbook (`get_strategy(id)` for the one
  you just assigned or that was already set), then
  `append_note(entryId, note=<your analysis ending in the marker>)`.

Skip entries where `alreadyAnalyzed: true` (unless the user said force) - that is
the once-only guarantee. You never pass trade ids; only the `entryId`s that
`prepare_day` returned.

### 3. Synthesize the daily journal
From the day's entries + their `netPnl`, write a day-level synthesis (themes,
what repeated, net result, one thing to fix tomorrow):
- `create_journal_entry(scope="day", date=<the date prepare_day returned>,
  body=<synthesis + marker>)`.
- If it returns `{alreadyExists:true, entryId}` and that entry's `notes` lack
  `tj-ai-mcp:v1`, `append_note(entryId, <synthesis + marker>)`; otherwise leave it.

### 4. Overview in chat
Print a concise digest (don't just say "done"):
- Net / gross P&L, # trades, win rate; # group + # trade entries (created vs
  already there); best and worst.
- 2-4 recurring observations (timing, sizing, strategy adherence, copytrader
  consistency across accounts).
- Anything needing attention (a stub instrument with no P&L, entries skipped as
  already analyzed).

If a tool returns "not found" for an id, re-fetch (`prepare_day` / `list_journal`)
and use the exact value - do **not** invent ids or conclude a backend problem
(trade ids, entry ids, and account ids are different namespaces). If several
trading-journal connectors are configured, call `server_info` first and use one
instance for the whole run.

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

Set `tag` to your honest read (`good`/`neutral`/`bad`) on any untagged entry (it
fills only when unset); leave it untagged when the read is ambiguous.

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
- **ChatGPT**: a Custom GPT with an Action on the REST API — paste
  [chatgpt-instructions.md](chatgpt-instructions.md) into the GPT's Instructions as-is;
  full steps in [chatgpt-setup.md](chatgpt-setup.md).

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
