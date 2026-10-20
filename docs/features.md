# Features

[‹ README](../README.md)

What the journal does, page by page. The short version is the Features list in the README;
this page has the rules and the details behind each bullet.

## Trades

**Fills in, trades out.** Brokers and platforms deliver executions, not trades. The importers
store every fill as it arrived, and a per-account, per-symbol FIFO position builder derives the
trades: partial closes, scale-ins, a long-to-short flip in one fill (two trades), commissions
charged per side at the firm's rate on both legs of the round turn. Futures P&L goes through the
instrument's tick size and tick value, never a raw price difference; an unknown symbol root is
created as a stub and its trades show no P&L until an admin sets its ticks on `/commissions`,
after which everything is recomputed.

**Copy sets.** One decision fanned across accounts by a copytrader (same underlying, same
direction, entries within two seconds, two or more accounts) forms a set. `/trades` can list by
set with the accounts folded underneath and the set's R and price path (heat, best seen and
the exit, averaged over the legs) on the row, the set's sheet compares the fills leg by leg, the
journal's day, week and group notes list trades the same way the log does (one row per decision
when the log is by set), and analytics can count per decision instead of per account. Accounts a copier drives together can
share a **copy group** on `/accounts`, which lets their legs count as one decision up to thirty
seconds apart.

**The trade sheet.** A click on any row opens the trade over the page: fills, entry and exit,
net and gross P&L, R, the plan block (stop, risk, target, MAE, MFE, exit efficiency), the
journal note or a button to write one, and the chart. Charts load the trade's whole trading day
at the finest cached size and open on the stretch around the trade; **Day** and **Trade** switch
the view and the bar-size selector rolls the same bars up. With a TopstepX connection the shared
bar cache takes the exchange feed first for the recent days of the roots you trade (the exact
contract, no delay, true volume) and fetches one-second bars around every trade shorter than ten
minutes, so the chart offers 1, 5 and 15 second sizes and the excursions are measured from them.
`j`/`k` walk the trades while the sheet is open, `g`/`n`/`b` tag the trade good, neutral or bad,
and the sheet owns browser history, so Back returns to the untouched page.

**Trade log filters.** Symbol root, direction, outcome, account, status (open, closed or both),
date presets and a free range; every filter is in the URL, so a view can be bookmarked or sent
to the API with the same parameters.

## Open positions

A position is not a trade yet (the trade is written when the cycle closes), but it is where the
plan belongs. `/trades` opens with an "Open now" strip above the table and the dashboard card
lists them; a click opens the position's sheet with its fills, average entry, last price,
unrealised P&L and a plan row: stop, risk, target and a one-line thesis. A stop sets the risk at
the current size, a risk sets the stop. The plan is anchored on the fill that opened the
position, so it survives rebuilds and the CSV replacing an e-mail fill, follows a scale-in (risk
recomputed, stop kept) and moves onto the trade the moment the position closes.

A position with no plan shows a dashed "set plan" chip; one held for days is marked "carried?",
since that usually means missing fills. Open legs of one copied decision cluster like closed
sets: the sheet lists the other legs, and a plan saved with "all legs" puts the stop price on
every leg with each leg's risk from its own entry and size. A leg that lands later inherits the
set's plan, marked "from set" until the trader saves it. Positions on accounts hidden from the
stats stay out of every view.

**AI stop and target** on the sheet asks the trader's own model where the levels belong, from
the bars ending now (the chart image too, for vision models), the day plan and what its max loss
still allows, the lab's best distances for the instrument, the strategies and recent lessons. It
answers with a stop, a target (a second one optional), the reasons and a trail rule, which the
app checks (right side of the entry, within the bars, snapped to tick) and prices itself. The
suggestion is shown dashed; **Use as plan** writes it with source `ai`, nothing else touches the
plan, and after the close the suggestion stays on the trade sheet next to what the trader chose.
Settings > AI chooses what a new open position gets without a click: nothing (the default), the
suggestion waiting on its sheet, or the suggestion written straight into the plan. Each position
is judged once; a carried position is left alone.

The model does not get a bare slice of bars. It gets the **session** as a trader reads it,
computed from the cached one-minute bars as of the moment (prior RTH range and close, overnight
range, opening range, session extremes, VWAP, ATR, the last swing highs and lows, the structural
levels nearest the last price) plus a multi-timeframe window: 15 m over the last sessions, 5 m
over the last hours, 1 m over the last minutes. The trade review gets the same, as it stood at
the entry. A **session warm-up** keeps today's one-minute bars fresh for every root with a fill
in the last days (every two minutes while Globex trades) and the two previous days complete,
which is also what makes the sheet's distance to the stop and the guardrail's "price is through
your stop" current.

## Journal

Entries attach to one of four scopes: a single trade, a whole day, an ISO week, or a group of
trades you assemble (`⌘D` on a multi-trade selection). Every view has the same shape: a tag
(good, neutral, bad), a mood on day notes, mistake and emotion labels from a fixed vocabulary
(so analytics can tell you what "moved the stop" costs), a strategy on trade and group notes, a
markdown editor (paste screenshots, drag-drop images, built-in templates, recall of prior
entries, `⌘P` live preview, `⌘S` save) and the related-trades panel. Images are stored as
attachments next to the database. The feed filters by scope, tag, mood, strategy, label and
period, and a filter on a value also lists the entries where the AI review suggests it (shown
as a dashed chip). Exports to Markdown and PDF take the same filters.

**Strategies and playbooks.** The Library holds your strategies, each with a playbook and a
checklist (its rules, one per line). Trade and group notes that use a strategy tick the rules,
the note shows the score and the misses, and analytics reports compliance per strategy with the
net of complete versus incomplete trades. The AI can **draft a strategy's playbook and checklist
from your own trades** (the numbers measured by the app, your notes, where you trade differently
from what you wrote) and **find strategies in your untagged trades** (grouped by instrument,
time of day and holding time, named and judged keep / fix / drop); nothing changes until you
apply a draft.

**Who set a value is recorded.** A page save marks what the note holds as the trader's; values
written through the REST API or MCP are marked `api` until you save the note; a suggestion the
AI made is only ever a suggestion until you accept it. Analytics can count only what you set or
confirmed ("Confirmed by me").

## The day: plan, guardrail, coach

**Day plan.** The day view opens with a pre-market plan: bias, levels, a cap on decisions, a
stop time, a max loss per account, notes. It is read rule by rule against the day's trades and
flagged when it was written after the first trade; the dashboard shows today's plan as its own
card once written and nudges when today has none. For today or a later day **Draft the plan**
has the AI propose the cap, stop time, max loss, levels and focus from your norms, the last
trading days' coach findings and plan checks, what an account usually loses on a losing day,
your open lessons and the gamma map; the draft prefills the form for you to change and save.
The bias stays yours.

**Guardrail.** With the guardrail on (Settings), the server re-reads the day as fills arrive
and nudges you once, as a toast and a browser notification while the tab is in the background,
and as a time-sensitive push on the phone: when the coach flags a trade that just closed (a
re-entry right after a loss, a size-up after a loss, a stop run through, trading against the
plan), when the plan's decision cap is reached, when an account nears or hits its max loss, at
the plan's stop time, when a position has been open for a few minutes with no stop recorded,
and when the stops on what is open risk more than the day plan's max loss still allows. Each
nudge quotes your own open lesson about it. No AI model is involved, and the day page lists them.

**Coach.** Rule-based checks over the day's trades (re-entry after a loss, size-up after a
loss, a stop run through, a winner cut short, a loser held, overtrading, late entries) with the
numbers and a one-click label. The same checks reach the AI reviewer and the API.

## Analytics, lab, digest

**Analytics** surfaces findings (best and worst day of week and hour, most-traded against
most-profitable symbol, profit-factor and R-multiple flags), measures every trade's path from
cached bars (max heat, best seen, how much of it the exit kept), breaks the numbers down by
symbol, direction, strategy, account, label and time of day, and links each row to the matching
`/trades` view. Equity and drawdown come from balance anchors you record per account.

**Stop and target lab** (`/analytics/lab`) replays your past decisions on the cached 1-minute
bars under a grid of fixed stops and targets, the axes from how much heat your winners took and
how far they ran, with an optional time stop: the average result per decision with its 90 %
bootstrap range and win rate per cell, against what your own exits made, plus the
heat-against-best scatter of winners and losers. **Explain this grid** has the AI recommend a
rule and say how far to trust it.

**Digest** reads one ISO week per decision against the week before: the headline numbers,
one-sentence keep and fix lists, the best and worst decision, the coach's findings tallied
across the days, how the day plans held and the week's review lessons. Downloadable as
markdown, served to the API and MCP, and the basis of the AI's week review.

## AI reviews

**Analyze** on any note (a trade, a copy set, a group, a day or a week, also one not written
yet) or on a whole day writes a review into the journal, briefed for what it reviews (a trade's
entry, risk and exit against its plan, its price path and the bars around it, optionally the
chart image; a copy set's consistency across accounts; a day against its plan; a week from the
digest) and headed by the model that wrote it. It never writes your tag, strategy or labels:
what it proposes shows on the note as dashed pills to accept or dismiss, and the editor marks
them dashed in its pickers. It proposes only what the data can show: the app holds the fills,
never the working orders, so "No stop" is yours to set, never its guess.

Each review names **one lesson**, kept in a ledger (`/journal/lessons`): lessons about the same
mistake are counted while open, the next reviews are told your recent ones and say whether a
trade repeated or applied them, and you mark a lesson learned once it stops coming back.

Reviews run in the background: a review carries on when you close the sheet or leave the page,
the note shows a spinner wherever it is listed and cannot be edited until the review lands. A
whole-day run locks only the notes it is writing. Each user runs it on their own provider key
or on a server-wide default; see [AI](ai.md).

## Dashboard and calendar

The **dashboard** leads with a 60-day cumulative P&L curve, a Today panel with the day's net P&L
as a hero number, today's plan once written, the open positions, equity by account type with the
current drawdown, and a stat strip with sparklines for trades, win rate, R-multiple and streak.
The **calendar** is the month view with per-day P&L heat and weekly summaries; a day pivots to
the trades on it and to its journal page.

Pages refresh themselves: every mutation bumps a per-user data version, open pages poll it every
ten seconds and swap only the changed region (a row that changed flashes in place; rows added or
removed while you are working defer to a click-to-refresh toast). The phone gets the same
version over a server-sent event stream.

## Market pages

**Gamma (GEX)** at `/gex` shows the dealer gamma regime, flip and walls for ES and NQ from a
gex-engine instance (the gamma-exposure service by the same author, to be published), as of now or as of any moment, with the strike
profile and a data-quality strip; trades get a gamma block during day preparation. **Open
interest** at `/oi` lists and serves the daily dashboards an external pipeline drops into a
folder. Both are off unless configured.

## Keyboard and palette

`⌘K` opens the command palette: pages, recent pages, symbol jumps (type `MNQ` to land on
`/trades?symbol_root=MNQ`) and date keywords (`today`, `last week`). Light and dark themes are
persisted per browser, or forced with `?theme=light|dark`.

## On the phone

The iOS app shows Today (realised and unrealised P&L, open positions, the plan, the nudges), the
trade log, the journal with editing, the calendar, accounts with equity and drawdown and the
digest; it receives the guardrail's nudges as time-sensitive pushes, runs a Session Live
Activity in the Dynamic Island and on the Lock Screen during the trading day (one row per
copied decision when trades are grouped by set, as are the widgets' counts), and keeps every
answer for when the server is out of reach. An optional app lock asks for Face ID (the passcode
as fallback) when the app comes back after a grace period you choose, and money on the Lock
Screen can be hidden until the phone is unlocked. See [app/ios/README.md](../app/ios/README.md).
