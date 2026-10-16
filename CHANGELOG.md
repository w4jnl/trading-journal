# Changelog

The server and the phone app share one version: a release tag names the server, and the app
built against it carries the same marketing version.

## Unreleased

### App (iOS), 2.18.2

- The first note on a day or a trade takes the strategy, the mistakes and the emotions, as the
  web app's note does; before, only a saved note's Edit screen had them.
- AI, for a user with a provider in the web app's Settings: Analyze on a note and on a trade
  without one (the note shows the review running and its suggestions when it lands), the day
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
