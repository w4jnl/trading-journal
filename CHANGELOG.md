# Changelog

The server and the phone app share one version: a release tag names the server, and the app
built against it carries the same marketing version.

## Unreleased

### App (iOS)

- Journal notes render pipe tables (the GEX annotation's entry and exit columns) as a table
  instead of a run of bars, and drop the HTML comment the annotation carries for the machine,
  as the web does.
### Web

- The sets view of the trade log draws the price path glyph (heat, best seen, the exit) on the
  set's row too, averaged over the measured legs with the money summed; the tooltip says when
  some legs are not measured yet.
### App (iOS)

- An app lock, off by default: Settings > Privacy > Require Face ID (Touch ID, or the passcode
  when nothing is enrolled). The app asks when it comes back after the grace you pick
  (Immediately, 30 seconds, 1, 5, 15 minutes, 1 hour, 24 hours, or Until tomorrow, which asks
  once a day; 1 minute by default) and, unless "Lock on launch" is off, at every cold start;
  with it off, a cold start measures the grace from the last time the app went to the
  background. The passcode is the fallback, so the lock can never shut you out. In the app switcher the content is covered without a prompt. Turning the
  lock on or off takes one authentication. The device key and the server are untouched: this
  guards the unlocked phone in someone else's hand, nothing more.
- "Hide amounts until unlocked": money in the widgets, the Session Live Activity and the
  notification cards is redacted until the phone itself is unlocked. No prompt of its own.

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
