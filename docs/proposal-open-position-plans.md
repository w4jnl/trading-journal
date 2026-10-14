# Proposal: plan, stop, target and risk on an open position

Status: proposal, no implementation. 2026-10-14.

## Why it does not work today

- A position has no stored identity until it closes. `ListOpenPositions` groups
  live executions by (account, symbol) with a non-zero net; nothing is persisted.
- Every plan field (`trades.planned_stop`, `planned_risk`, `planned_target`)
  lives on a `trades` row, and the builder only writes closed cycles
  (`builder.go:458`, "Open positions at end of replay don't produce trades").
- Journal entries of scope trade/group need a trade id, so no note can hang on
  an open position either.
- Trade ids and set ids change on every rebuild. The builder already carries
  plans and journal links across rebuilds by the OPENING EXECUTION ID, then by
  entry_time (`findRebuiltTrade`, `FindTradeByEntryExecution`).
- The MotiveWave e-mail path (the fastest live source) inserts a provisional
  execution `MWEMAIL-<order>` and the later CSV import DELETES it and inserts a
  new row (`DeleteProvisionalByOrderID` in `importer/service.go:397`). The
  execution id of the opening fill is therefore NOT stable for e-mail-sourced
  positions; the (account_id, order_id) pair is.
- PR #104's "Suggest stop & target" was removed (ccb64b1) because it ran on
  closed trades only, i.e. in hindsight. On an open position the same idea is
  legitimate: the outcome does not exist yet.

## 1. Data model: the plan belongs to the position cycle, not the trade

New table `position_plans` (one row per position cycle = from flat to flat):

```
position_plans
  id            INTEGER PK
  account_id    -> accounts
  symbol        TEXT        -- contract, e.g. ESU6
  open_exec_id  -> executions (the fill that took the account from flat)
  open_order_id TEXT        -- same fill's order id, NULL for sources without one
  opened_at     DATETIME    -- same fill's exec_time (third anchor)
  stop          REAL        -- price; NULL = none recorded
  risk          REAL        -- money at risk as entered or derived
  target        REAL
  thesis        TEXT        -- short pre-trade note (why, what invalidates)
  source        TEXT        -- 'trader' | 'api' | 'ai' (accepted suggestion)
  trade_id      -> trades   -- NULL while open, set by the builder at close
  created_at, updated_at
```

Rules:

- Anchors, in order: `open_exec_id`; else `(account_id, open_order_id)`; else
  `(account_id, symbol, opened_at)` within the same tolerance the builder uses
  for `FindTradeBySignal`. The supersede step in `ingest()` re-points
  `open_exec_id` to the new CSV row for the same `(account, order_id)`, so the
  common path never needs the fallbacks.
- `stop` is canonical; `risk` is recomputed from `stop` x point value x |net qty|
  whenever the position's qty changes (scale-in, partial exit), exactly as
  `RestoreTradePlan` does today. A plan entered as a risk amount (no price)
  keeps `risk` and derives `stop` snapped to tick, same as `TradePlanSave`.
- At close the builder, which already restores plans by opening fill, looks up
  the `position_plans` row for the cycle, copies stop/risk/target onto the new
  `trades` row and sets `trade_id`. `trades.planned_*` become a materialised
  copy; `position_plans` is the single source of truth. Editing the plan of a
  closed trade (`TradePlanSave`) writes through to the same row. Legacy trades
  with plans but no row get backfilled once from their entry execution.
- Open cycles that survive a rebuild keep their plan because the anchor is the
  fill, not a trade id.
- A position older than, say, 3 days is flagged "looks carried or a data gap"
  on the plan form (the open-positions query comment names this case).

Point value comes from `positions.PointValue`; stub instruments show "set tick
specs first" instead of a risk figure, matching how P&L is left NULL today.

## 2. Where it shows: a position is a first-class row everywhere trades are

### /trades: an "Open" strip above the table

- Rendered whenever the filtered accounts hold at least one open position:
  one row per position, visually distinct (left accent, "LIVE" pill), with
  Dir, qty, avg entry, last price, unrealised, Stop, Target, Risk $ and
  distance to stop in ticks. A position with no stop shows a dashed "set plan"
  chip in the Stop cell, not an empty cell.
- The row key is `data-position-key="acc:sym"`. The strip is a live region:
  rows swap in place on the 10 s version poll; when a position closes its row
  leaves the strip and the new trade row appears, with a brief flash.
- Filter chip `status=open|closed|all`, default `all` (strip + table).
  `status=open` hides the table and is what the nav badge links to.
- The trades table itself stays closed-only, so analytics, exports and the
  API are untouched.

### Position sheet: `GET /positions?account=N&symbol=ESU6`

Same shell as the trade sheet (`renderTradeDetail`), opened from the strip, the
dashboard and the nav badge:

- Header: account, contract, direction, qty, avg entry, last, unrealised,
  opened at, fills so far (the open lots from `OpenFromFills`).
- Plan form: Stop / Risk $ / Target, mirroring `form.trade-plan` so the
  derivation rules (`sameFloat`, tick snapping, reward:risk hint) are shared.
  Below it the live read-out: ticks to stop, $ at risk at the current size,
  R if the target is hit, and a red "price is beyond your stop" state.
- Thesis: a two-line text field. At close, if non-empty, it becomes the first
  paragraph of the trade's journal note (created when none exists), so the
  pre-trade reasoning is in the review. Until then it is only on the plan.
- Chart: the existing bars with stop and target drawn as horizontal lines,
  which nothing draws today for closed trades either; add it for both.
- Journal: no entry exists yet. The sheet links to today's day note and
  shows the day plan's bias and max loss so the trader sets the stop next to
  their own rules.

### Dashboard

The existing `open_positions` partial gains Stop / Target / Risk columns and
the "set plan" chip; clicking a row opens the position sheet instead of
linking to /trades. The nav badge turns warn-coloured when any open position
has no stop recorded (title: "2 open, 1 without a stop").

## 3. Copy trades: an open set

- Reuse `copyset.Cluster` on the opening fills of the open cycles (same
  underlying via `Key`, same direction, within the 2 s window, two or more
  accounts). The result is an in-memory "open set"; nothing is stored, exactly
  like `trade_sets`.
- In the /trades strip and on the dashboard, legs of an open set collapse into
  one set row (expandable), the same way closed sets fold.
- The set's plan form is `TradeSetPlanSave` re-targeted at open legs: stop
  mode (one price, each leg's risk from its own avg entry) or risk mode (total
  risk split by exposure into one point distance), target identical on all
  legs. Storage stays per leg, one `position_plans` row per account.
  Read-back folds like `planOf` (uniform stop or "mixed" range, summed risk).
- When the legs close, the builder links each leg's plan to its trade, the set
  rebuild clusters them as today, and the set detail already shows per-leg
  stop/risk/R.

## 4. AI on the open position: stop placement and profit taking

This is the centre of the feature, and the one place a level suggestion is
honest: the position is open, the bars end now, and no outcome exists to peek
at. Same contract as the note review: the AI suggests, the trader decides, the
app never places or changes an order.

### Two moments

- **Entry (plan forming).** Right after the opening fill: where the stop
  belongs, where the first and second target belong, risk in $ and R at the
  current size, and one reason line per level.
- **Management (plan adjusting).** While the position lives: move the stop to
  breakeven or trail it, take partial profit at T1, hold or tighten into a
  level, or close before the day plan's stop time. Each suggestion names the
  trigger it is reacting to ("price closed above the overnight high on the
  5m").

### Inputs the model gets (all computed by the app)

- Position: direction, avg entry, size, unrealised, time in trade, fills so far.
- Price action: the last N 1m bars cut at now, optional chart image, swing
  highs and lows, prior day and overnight high/low, VWAP, the day plan's
  `levels` text (the trader's own marks).
- The trader's rules: the strategy attached to the day note or set note (its
  body often states stop and target rules), the lessons ledger, the plan's
  thesis field.
- Evidence: lab results for this instrument and strategy (best stop and
  target distances from `levels.Grid`), MFE/MAE distributions of past
  decisions (`measureDecisions`), hit rate of T1 at a given distance.
- Constraints: remaining loss budget today (day plan max loss minus realised),
  open planned risk across all legs and sets, stop time.

### Output and storage

- A structured result: stop, T1, T2 with a scale-out fraction, trail rule
  (none / breakeven at 1R / behind the last swing), risk $, R to each target,
  a reason per level and a confidence word. Stored in `ai_outputs` kind
  `position_levels`, ref = the plan anchor, with the timestamp and the last
  price at that moment sealed into it.
- Rendered as a dashed suggestion card next to the plan form. Per level:
  "Use" copies it into the form; "Use all" fills the form; "Dismiss" hides it.
  Saving the form is what writes the plan, with source 'ai' when the values
  came from the card.
- Every plan change writes a `position_plan_history` row (time, stop, T1, T2,
  source), so the sheet shows a timeline of the position: fills, plan moves,
  AI suggestions and price. At close the timeline is frozen on the trade
  sheet and in the trade review.

### How it is wired into the app

This is in-app AI through the app's own provider path, not an MCP feature:

- Client: `h.aiFor(ctx)` as for every review and helper, so the user's own
  provider and key win over the server default, and a user without a provider
  sees no button (`aiEnabled` + `data-ai="off"`, 409 on the route).
- Runner: `h.AITasks` (`internal/ai/tasks.go`), keyed `<user>:levels:<anchor>`,
  polled by `GET /ai/tasks/status?k=`, result in `ai_outputs` like the
  strategy drafts and the lab explanation. The request returns at once.
- Prompt builder: a new `ai_levels.go` next to `ai_analyze.go`, reusing the
  reviewer's bar window and optional chart image from PR #102, the lab's grid
  numbers, `measureDecisions`, the day plan and the lessons ledger. The
  common prompt rules (no "no stop" inference, fills not orders) are shared.
- Output: the model answers in a fixed JSON shape (stop, t1, t2, fraction,
  trail, reasons); the app parses it the way `parseAILesson` does, snaps the
  prices to tick, recomputes risk and R itself, and rejects anything outside
  the bars' range or on the wrong side of the entry.
- Models: one model per user today (`user_ai_settings`). Entry suggestions use
  it as is. Watch mode would add an optional second "fast model" field in the
  AI settings (same provider, cheaper model) with a per-day cap; without it,
  watch mode stays off.
- Sources: the plan written from the card carries `source='ai'`, shown like
  the accepted review suggestions, and the trade review later sees both the
  suggestion and the plan.

### How it runs and what it costs

- On demand from the sheet (set or leg). A double click is one run.
- Optional "watch this position": re-run at each 5m bar close while open,
  surfacing only when the advice changes (stop move, T1 reached). Off by
  default, per user, capped. Results land on the card and, if the trader
  enabled guardrail, as one nudge.
- Deterministic alerts stay out of the AI: "price touched T1", "1R in your
  favour, your rule says breakeven", "price through your stop" come from the
  plan and the quote, instantly and for free. The AI is for the discretionary
  part: where, why, and whether to change it.

### Copy sets

One suggestion per set, keyed on the earliest leg's anchor, with the entry
given as the exposure-weighted average. "Use" fans out like the set plan
form: stop price on every leg, targets identical, risk per leg from its own
entry and size.

### Rules carried over

- Never claim the trader has no stop: the card reads "no plan recorded".
- Never suggest on a closed trade (that is PR #104's hindsight problem).
- The model only writes words and levels; every number it is given is the
  app's own, and the app recomputes risk and R from the levels it returns.

### Scoring the advice (the feedback loop)

The lab already replays cached 1m bars through a stop and target
(`levels.Replay`). With timestamped suggestions on closed trades, a new lab
axis "AI advice" sits next to "Actuals" and the grid: what following the
suggested stop and targets would have returned per trade, hit rate of its
T1, and how often the trader's own levels did better. That makes the
suggestion accountable and tells the trader, over weeks, whether to lean on
it or ignore it. It also feeds the next suggestion ("your T1 at 8 points was
hit 71% of the time on this setup").

### MCP/API (secondary)

The same service behind `list_positions`, `set_position_plan` (source 'api',
dashed until confirmed) and `suggest_position_levels`, so a chat session can
reach it. Nothing above depends on MCP.

## 5. Guardrail: rules that can finally see the position

The worker already wakes on a version bump, and every fill bumps. New rules,
all with the existing 20 min freshness gate and nudge dedup on (day, key,
ref=open_exec):

- `unplanned-open`: a position has been open for more than N minutes (default
  3, user-settable) with no stop recorded.
- `open-risk`: planned risk of all open positions exceeds the remaining day
  loss budget (`day_plans.max_loss` minus realised net).
- `past-stop-live`: last price beyond the planned stop while still open. Only
  from a trusted quote (TopstepX `LatestBar` or IBKR), never from the delayed
  Yahoo path; phrase it as "price is through your recorded stop", because the
  working order may already have filled and not yet arrived.
- `thesis-missing` (opt-in): a plan was set without a thesis.

## 6. Later: read the real working orders

IBKR Client Portal (`/iserver/account/orders`) and the TopstepX API
(`/api/Order/search`) expose working orders. A poll could prefill the plan from
the actual stop/limit orders and mark `source='broker'`, which removes the
assumption entirely for those accounts. MotiveWave e-mail carries fills only.
Not part of this proposal; the model above leaves room for it.

## Status

- Phase 1 (PR #109): `position_plans`, hand-off, position sheet, /trades
  strip, dashboard columns.
- Phase 2 (PR #110): open sets (fan-out, inheritance on every rebuild,
  declared copy groups with a 30 s window). The manual "join a leg" anchor
  is not built yet; the copy group covers the cases seen so far.
- Phase 3 (PR #111): guardrail rules `unplanned-open` and `open-risk`;
  `GET /api/v1/positions`, `POST .../plan`; MCP `list_positions`,
  `set_position_plan`. `past-stop-live` needs a quote in the worker: not
  built.
- Phase 4: the AI suggestion at entry (stop, target, optional T2 and trail,
  checked and priced by the app; "Use as plan" with source 'ai'; frozen on
  the trade sheet after the close). Not built: watch mode (needs the
  second "fast model" setting), the plan history timeline, the lab's "AI
  advice" axis, the MCP suggest tool.

## Delivery

1. `position_plans` + service + builder hand-off at close + supersede re-point
   + backfill. Position sheet with plan form, /trades open strip with
   `status` filter, dashboard columns and badge. Tests: plan survives rebuild,
   survives e-mail-to-CSV supersede, risk follows scale-in, closed trade carries
   the plan and thesis.
2. Open sets (fan-out, fold-back), chart plan lines, MCP/API, guardrail rules.
3. AI suggestion card at entry and in management, plan history timeline,
   watch mode, lab axis "AI advice", MCP tools.

## Open decisions

- Default for the trades page: strip always on, or only with `status=open|all`?
  Proposal: always on when non-empty; it is the thing the trader needs to see.
- Thesis into the journal note at close: automatic, or offered as a one-click
  "import thesis" on the trade sheet? Proposal: automatic, marked as the plan's
  text.
- Should MCP (claude.ai) be allowed to write plans at all? Proposal: yes,
  source 'api', dashed, same as tags today.
