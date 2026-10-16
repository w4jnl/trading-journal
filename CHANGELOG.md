# Changelog

The server and the phone app share one version: a release tag names the server, and the app
built against it carries the same marketing version.

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
