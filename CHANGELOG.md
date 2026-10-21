# Changelog

The server and the phone app share one version: a release tag names the server, and the app
built against it carries the same marketing version.

## Unreleased

### Server

- The AI method lives in one place: `internal/aimethod` holds the skills the journal's models work
  by as data (the generic futures day-trader analyst, one skill per entry scope, the lesson and
  verdict contracts, the label vocabulary, a glossary of the evidence) and renders the review
  prompts from them, byte for byte what they were. Every review note's marker and every stored AI
  output now record the method version (`method=2`), so later rubric changes never blur comparisons.
- Reviews send less and read better: every bar is one string ("time open high low close") instead of
  seven lines of JSON, the session windows are trimmed to the prior session and the last hours, a
  note's earlier AI reviews and gamma block are no longer sent back as the trader's words (their
  count is), and a day's review receives the lessons its own trade reviews already drew and is
  asked to build on them. A trade review costs about half the input tokens it did.
- Prompt caching. A model call is now blocks with cache hints: the method (one block for every
  review scope, with the item's kind named in the user turn), the label vocabulary and the
  playbooks lead and are marked cacheable; the item follows. Anthropic gets `cache_control`
  breakpoints and reports the cached tokens; every OpenAI-compatible provider gets the same
  stable-first order for its own prefix caching. A day run's N+1 reviews now share one prefix.
  Usage (input, output, cached) is logged at debug level. xAI (Grok) joins the provider presets.
- The MCP server carries the method, so an agent needs no skill of its own: instructions at
  `initialize`, one prompt per mode (`review_day`, `review_entry`, `today`, `review_week`,
  `plan_day`), the method, the label vocabulary, the lessons and the playbooks as resources, and
  two tools (`get_review_method`, `list_lessons`; 25 in all). `server_info` reports the server
  version and timezone, the trader's review language, the marker and the method version;
  `append_note` records a trailing lesson line in the ledger; `prepare_day`'s note names the
  method. `GET /api/v1/ai/method` answers the same over REST. The `trading-journal-analyst` skill
  and its ChatGPT twin shrink to a pointer at the server.
- The trader profile: Settings > AI holds how the method applies to you (style, the evidence you
  weigh most in order, your markets, standing instructions). It rides as one cached block in every
  review and plan, sizes the evidence the app sends (bar windows by style, the gamma map briefed
  when gamma is not a focus), and reaches every connected agent (`server_info.profile`, the resource
  `trading-journal://profile`, the prompts, `get_review_method`) and the REST API (`/api/v1/me`
  `settings.aiProfile`, `/api/v1/ai/method`). A review can be asked for in another language once
  (`?lang=` in the app, `language` on the MCP prompts and `get_review_method`). `GET
  /api/v1/ai/preview/{id}` shows the request a review would send, block by block with what is
  cached and how big each part is, without calling a model.

- The day map reaches the AI: the plan draft reads each market's strongest zones, the expected
  range and the day type and takes its levels from the zones and the gamma map; the reviews of a
  trade, a group or a day and the levels suggestion on an open position get the map around the
  entry, with the nearest zones above and below, and are asked to judge the location of the entry,
  the exit and the stop against it.
- The chart's mark from the levels feed prices the open positions (dashboard, `/today`, the phone,
  the Session activity, the guardrail's through-stop check) whenever it is the most recent price
  known, within three minutes of the post; `priceSource` says `MotiveWave`. Older marks lose to a
  newer broker or vendor bar, as before.

### Web

- Levels: an importance switch (Strong / + Medium / All, by score) and source chips (bars, chart,
  gex) above the ladder, applied to the ladder, the charts and the table at once and remembered
  per user; the zones stay scored over every level.

## 2.20.0, 2026-10-20

### Server

- **Levels, the day map** (`/levels`, `/levels/{market}`): one set of reference levels per
  market and trading day for the markets traded in the last thirty days. The journal computes
  the structural map from its bar cache (prior day high, low and close, overnight and opening
  range, daily ATR(14) bands from the session's ETH or RTH anchor as the MotiveWave study
  draws them, floor and Camarilla pivots, prior week and month extremes, swing highs and lows
  on 30m to weekly bars, unfilled gaps, round numbers) and merges the gamma engine's walls;
  levels fold into scored zones; a day-type card reads the prior day (inside, outside, NR7,
  wide, strong or weak close, the gap, the overnight against the prior range) into a lean with
  its reasons. A ladder, the strongest zones, daily, weekly and intraday charts with the zones
  drawn, and one button that writes the zones into the day plan's levels. Scores rank, they do
  not forecast; each day's map is kept so a past day opens as it stood. Settings > Levels pins
  the markets to keep on the page besides the ones traded lately.
- `POST /api/v1/levels/{market}` takes the session map a MotiveWave chart posts (contract in
  `docs/api-handoff-002-levels-feed.md`: prior session, overnight, opening range, VWAP bands,
  volume profile, ATR bands and the mark); the newest post per market and day overlays the
  map with source `chart`. `GET /api/v1/levels/{market}?day=` and the MCP tool `get_levels`
  answer the merged map. The feed is shared across the instance's users, and the page refreshes
  its regions when a new post lands. New tables `level_feeds` and `level_maps`.

### Web

- The journal's trade tables (day, week and group notes) follow the Trades | Sets choice of the
  trade log. By set, a copy set is one row per decision with its accounts behind the toggle and
  the summary counts decisions ("Decisions · 5 in 25 trades", wins and losses per decision); by
  trade, every trade is its own row. The rows are the real copy sets, no longer a two-second
  guess at the fan-out.

## 2.19.0, 2026-10-19

### Server

- Push: a copied exit is one notification, not one per account. The closed-trade alert folds
  the legs of a copy set into the decision (`MES short ×5`, `5 accounts`, the summed net); a
  leg that lands a tick later re-tells the decision under the same collapse id, so the banner
  is replaced rather than stacked. The Session activity's trade alert folds the same way.
- Guardrail: "price is through your stop" is said once for the legs of a decision that share
  the stop, naming the accounts; a leg with a stop of its own is still said on its own
  ("no stop recorded" already spoke per decision).
- The Session activity's state carries each open leg's copy set (`set`, the legs' account ids)
  and the day per decision (`decisions`, `setWins`, `setLosses`, `openSets`); the legs are
  ordered by the decision with the most at stake and trimmed by whole sets (three decisions,
  twelve legs). `GET /today` gains `decisions` (count, wins, losses, open) and the coach report
  `decisionWins` / `decisionLosses`.

### Web

- The sets view of the trade log draws the price path glyph (heat, best seen, the exit) on the
  set's row too, averaged over the measured legs with the money summed; the tooltip says when
  some legs are not measured yet.
- Trades, sets view: the entry spread of a decision is shown to the cent. A leg's entry is a
  fill-weighted average, and the difference of two such averages printed its float noise
  (`±0.08333333333303017` for a twelfth of a point); the set detail's spread and tick count
  read from the same rounded value.

### App (iOS)

- "Trades grouped by set" now reaches the Lock Screen and the Dynamic Island, the Today widgets
  and Today's open positions: a copied decision's legs fold into one row (size and unrealised
  summed, the nearest stop, ×N), and the counts are decisions, their wins and losses and open
  decisions. Flipping the switch redraws the running activity and the widgets at once.
- An app lock, off by default: Settings > Privacy > Require Face ID (Touch ID, or the passcode
  when nothing is enrolled). The app asks when it comes back after the grace you pick
  (Immediately, 30 seconds, 1, 5, 15 minutes, 1 hour, 24 hours, or Until tomorrow, which asks
  once a day; 1 minute by default) and, unless "Lock on launch" is off, at every cold start;
  with it off, a cold start measures the grace from the last time the app went to the
  background. The passcode is the fallback, so the lock can never shut you out. In the app
  switcher the content is covered without a prompt. Turning the lock on or off takes one
  authentication. The device key and the server are untouched: this guards the unlocked phone
  in someone else's hand, nothing more.
- "Hide amounts until unlocked": money in the widgets, the Session Live Activity and the
  notification cards is redacted until the phone itself is unlocked. No prompt of its own.
- Journal notes render pipe tables (the GEX annotation's entry and exit columns) as a table
  instead of a run of bars, and drop the HTML comment the annotation carries for the machine,
  as the web does.

## 2.18.6, 2026-10-17

Documentation release; the server's behaviour is unchanged. The app is rebuilt as 2.18.6 with
no code change.

### Repository

- The README follows the shape of the author's other repositories: a hero banner, screenshots
  and a recording of a synthetic instance, Setup in five steps, why this journal and what it is
  not, a Features list that links into `docs/`, Scope and status, How it works with a diagram of
  the parts inside the binary and one of the tools around the journal (MotiveWave studies,
  mwrelay, gex-engine, oi-dashboard). The feature prose moved to `docs/features.md`,
  `data-sources.md`, `auth.md`, `ai.md`, `deploy.md` and `development.md`, corrected where it
  had gone stale.
- `scripts/demo/` boots a throwaway journal with twelve weeks of invented fills, notes and
  plans and takes the README images with Playwright, so they can be remade without anyone's
  real trades. `assets/brand/` holds the mark, the hero and the GitHub social preview with
  their HTML sources.
- CONTRIBUTING.md, SECURITY.md and an issue form; `API.md` lists all 21 MCP tools; the Docker
  image label says Apache-2.0.

## 2.18.5, 2026-10-17

### Server

- Journal filters: picking a tag, strategy, mistake or emotion now also lists the entries where
  the AI review suggests that value and the suggestion is still open (shown as the dashed chip).
  Untagged, Unassigned and None logged keep their plain meaning. The REST journal list follows.

### App (iOS)

- The phone keeps the journal's answers. When the server is out of reach (or the app starts
  without it), Today, Trades, Journal, Calendar, Accounts, the digest and every detail show the
  last known state with "Last known 09:12" over it instead of an error, and load again the
  moment the stream is back: the app notices the loss of reach on the first read that fails,
  not only when the stream's socket times out, and tries again the moment the network returns.
  Pull to refresh still asks the server. After each change of the
  data version the app quietly keeps what the screens will want: the month's calendar, the
  first page of trades and of the journal, the notes and trades of the last two weeks, the
  vocabularies, accounts, equity and the digest. Images (charts, attachments) are kept too.
  At most 150 MB on the phone, answers older than a month leave; Settings shows the size and
  clears it. Signing out forgets everything.
- A note, a day plan or a position plan whose save meets no answer is kept on the phone as a
  draft; the editor opens on it next time, says when it was kept, and offers to discard it.
  Nothing is replayed on its own: the save stays the trader's, with the journal's conflict
  checks.

## 2.18.4, 2026-10-16

The app is unchanged and stays at 2.18.2.

### Server

- Open positions on accounts hidden in Settings (not counted in the stats) now stay out of
  every view: the dashboard and trades-page cards, the sidebar badge, the positions API, Today
  on the phone with its unrealised P&L, the widgets and the Session activity, the AI
  position helpers and the open-position nudges. Before, the account's trades left the
  default lists but its open positions still showed and still added to the phone's open P&L.
- Settings keeps your place: a save far down the page (the guardrail switch, pairing a phone,
  an AI or broker setting) came back at the top of the page after every click. The page now
  returns to where you were; a new API key, shown once, still scrolls into view.

## 2.18.3, 2026-10-16

The first public release: the repository is open source under the Apache License 2.0 at
github.com/w4jnl/trading-journal. The app is unchanged and stays at 2.18.2.

### Server

- Saving which accounts count in the stats (Settings > visibility), retiring an account or
  changing its type now bumps the live version. Before, only a firm change did, so an open
  dashboard, calendar or phone screen kept the old aggregates until something else moved,
  while the phone's widgets, refreshed on their own clock, already showed the new ones.
- Open source: `LICENSE` (Apache 2.0), the fonts' OFL notices next to the web fonts, the test
  data and the iOS wire-test fixtures carry example accounts and placeholder text
  (`app/ios/scripts/scrub-fixtures.py` rewrites a capture), the third-party ProjectX API spec
  is gone.

## 2.18.2, 2026-10-16

### App (iOS), 2.18.2

- The first note on a day or a trade takes the strategy, the mistakes and the emotions, as the
  web app's note does; before, only a saved note's Edit screen had them.
- AI, for a user with a provider in the web app's Settings: Analyze on a note, on a trade
  without one and in the first-note sheet itself (an empty sheet asks for the note and its
  review in one go, a written one is saved first; the note shows the review running and its
  suggestions when it lands), the day
  run on a day's page with its progress and cancel, a stop and target for an open position to
  apply to the plan, and a draft of today's plan to take into the editor. Results arrive over
  the event stream; nothing polls.

### Server

- AI from the phone: `POST /ai/review` (a note by id or by its trade, set, day or week),
  the day run (`POST /ai/day/{date}`, `GET`/`DELETE /ai/day`), levels on an open position
  (`POST`/`DELETE /positions/{account}/{symbol}/levels`, `.../levels/apply`) and the day
  plan draft (`POST`/`DELETE /plan/{date}/draft`); `review` on the journal detail, `ai` on a
  position, `draft` on the plan, `ai.ready` on `/me`. Device key; 409 `ai-off` without a
  provider.
- The dashboard shows today's plan as its own card once it is written: the bias and limits,
  the levels, the notes, and the rules held or broken as the day's trades close, with a link to
  edit it. Before, the plan was one line under today's P&L, easy to miss before the first note.
- `DCA` joins the mistakes vocabulary, and every label carries a description of what its
  evidence looks like: the AI review reads them when it suggests labels, and
  `GET /journal/labels` returns them.

## 2.18.1, 2026-10-16

### App (iOS), 2.18.1

- The Session activity's clock: the session's hours in exchange time and the phone's, and the
  countdown to the close only while the session runs. Before the open it showed the session's
  length frozen, which read like a clock in Central time.

### Server

- `GET /today` `session.exchangeTz` and the Session activity's attributes carry the exchange's
  zone (`America/Chicago`).

## 2.18.0, 2026-10-16

### App (iOS), first build, 2.18.0

- Pairing by QR or code from the web app's Settings; a device key of its own.
- Today live over the server's event stream: P&L, open positions, the day plan's budget, the
  nudges, the cumulative line, equity. Trades with filters, trade detail with the chart,
  journal with markdown and images, the calendar as a heat month of trading days and a day view, accounts with the equity
  curve and drawdown, the weekly digest with the lessons.
- Writes: journal edit, delete, append and the first note on a trade or a day, photos as
  attachments, the AI review's suggestions accepted or dismissed, the day plan, the plan on an
  open position with the risk mirrored live, the trader's switches, balance anchors.
- Push: nudges the moment they happen with Seen and Open, closed trades with their chart and a
  dictated note, the expanded card with the live numbers, the settings mirrored to the server,
  the badge and the silent sync.
- The Session Live Activity in the Dynamic Island and on the Lock Screen, the Today widgets,
  the guardrail switch in Control Center.
- iPad: a sidebar and three columns for the trades and the journal; keyboard shortcuts.

### Server

- `GET /journal/{id}` lists the AI review's open `suggestions`; `POST /journal/{id}/suggestions`
  accepts or dismisses one (device key).

## 2.17.0, 2026-10-15

The API for a phone: pairing and device keys, journal writes, accounts and equity, today and
events, push and the Session activity. See the release notes.
