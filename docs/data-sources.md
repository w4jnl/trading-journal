# Data sources

[‹ README](../README.md)

How fills reach the journal. Four paths, one import pipeline: every path ends in the same
`executions` table, and the FIFO position builder derives the trades from there. The MotiveWave
CSV export is authoritative wherever it exists; the live paths exist so that the journal is
current during the session.

| Source | How | Latency | Accounts it covers |
| --- | --- | --- | --- |
| MotiveWave CSV export | drop in `imports/inbox/` or upload at `/import` | when you export | every account MotiveWave trades: Apex and Lucid (Rithmic), AMP (CQG), IBKR |
| MotiveWave fill e-mail | MotiveWave's "Send Email On Filled Order" to the journal's SMTP listener | seconds | the same accounts, as provisional fills until the CSV lands |
| TopstepX API | the trader's TopstepX username and API key in Settings | minutes (15 in RTH, 60 in Globex, plus on page visits) | TopstepX |
| IBKR Client Portal Gateway | a gateway URL per IB login in Settings | minutes, same cadence | Interactive Brokers |
| TopstepX CSV export | the orders or trades export, dropped or uploaded | when you export | TopstepX, before the API or for history |

## CSV import

Two entry points, one code path: the upload at `/import` (drag-drop or file picker) and a watch
folder, `imports/inbox/`, watched with fsnotify and swept at every start. The format is detected
from the header row: `Account,Ticket,…` is MotiveWave, `Id,AccountName,…` is a TopstepX orders
export, and the TopstepX trades export is recognised too. Rows are deduplicated on the broker's
ticket or order id, so a file can be imported again, whole or partial, without double counting.

After processing, a file always leaves the inbox: success moves it to
`imports/processed/<username>/<timestamp>_<filename>`, failure to
`imports/failed/<username>/<filename>` with a sibling `<filename>.error.txt`. No file is silently
lost.

**Accounts and instruments are created as they appear.** An unknown `(source, account ref)`
pair becomes an account row; the firm is guessed from the ref (`APEX-` is Apex, `LFE`/`LFF` is
Lucid, `U######` is IBKR, a MotiveWave source naming AMP is AMP, a TopstepX file is Topstep) and
the account is flagged for setup when anything is unresolved. An unknown symbol root becomes a
stub instrument with no tick specification; its trades exist but show no P&L until you fill the
ticks in, and a recompute follows. CQG's `EP` is read as `ES` on AMP accounts.

**Per-user inboxes.** `imports/inbox/<username>/` routes a drop to that user; the name must match
an active user. A file dropped directly into `inbox/` belongs to the bootstrap user, which keeps
a single-trader setup simple:

```
imports/
  inbox/
    alice/   foo.csv          <- alice's account
    bob/     bar.csv          <- bob's account
    leftover.csv              <- the bootstrap user
  processed/
    alice/   2026-…_foo.csv
    bob/     2026-…_bar.csv
    2026-…_leftover.csv
  failed/    (mirrors processed/)
```

### MotiveWave CSV

Columns `Account, Ticket, Underlying, Symbol, Exchange, Time, Action, Quantity, Price, Source`,
one row per fill, several accounts in one file. `BOT` is a buy, `SLD` a sell; the time carries
its offset and is stored in UTC. The export has no commission column: commissions come from the
per-side rate table on `/commissions`, by firm and instrument class.

### TopstepX CSV

The orders export has a UTF-8 byte order mark and is order-level: the importer keeps the rows
with `Status = Filled` and takes `ExecutePrice` and `FilledAt` as the fill. `Side` is the side
of the position book (`Ask` sells, `Bid` buys). The trades export (one row per half-turn) is
read too, aggregated per order.

## MotiveWave fill e-mail

MotiveWave can send an e-mail on every order fill (Preferences > Orders > Send Email On Filled
Order). The journal acts as MotiveWave's SMTP server (`mw_email` in the config, an embedded
listener on `:2525` by default), so the notifications land here instead of at a mail provider:
near-real-time fills for the accounts that have no API connector, which is every Rithmic prop
account and AMP/CQG.

Two layers, both on by default when the listener is enabled:

- **Capture.** Every message is archived verbatim as `.eml` under `mw_email.archive_dir`
  (default `data/mw-raw/`). Duplicate deliveries are dropped by content hash; parse or storage
  problems are logged, never bounced back to MotiveWave.
- **Ingest.** Complete fill notifications become **provisional executions** at once. MotiveWave
  mails cumulative order snapshots (a 3-lot filling 1+2 sends "Quantity: 1" then "Quantity: 3"),
  so provisional rows are upserted per `(account, order id)`, never appended per e-mail.

At every start the archive is replayed through the same ingest path, oldest first, in the
background, so a fill whose live ingest was missed still lands. Each message is attempted once;
terminal outcomes are recorded in `<archive_dir>/replay-state.log`, and deleting that file
forces a full re-pass (useful after an upgrade whose parser recovers messages an older one
could not read).

An e-mail carries no user identity, only the broker account ref, so a fill is booked under
whichever user already owns that account. Refs no user owns are not created from e-mail: the
fill stays archive-only, with a warning in the log and a card on `/accounts` for admins, until
the account's first CSV import creates it. The CQG connection reports its internal account id in
e-mails while the CSV carries the AMP account number; set the account's **acct id in emails** on
`/accounts` and fills addressed to it resolve.

**The CSV stays authoritative.** The Executions export carries the same order id as the
e-mails, so importing it replaces the matching provisional rows exactly, and an end-of-day
re-import never double counts. `/admin/transactions` shows each fill's import method; provisional
rows carry an `email · provisional` badge until a CSV supersedes them. Accounts live-synced
through a broker API skip e-mail ingest.

Setup:

1. Enable `mw_email` in `config.yaml` (see `config.example.yaml`) and restart. LAN or VPN only;
   never expose the listener publicly.
2. MotiveWave Preferences > Email: SMTP host is the journal host, the port matches your
   `tls_mode` (MotiveWave infers encryption from the port: 25 plain, 465 implicit TLS, 587
   STARTTLS; 2525 is treated as plain), username and password as configured. MotiveWave applies
   mail settings only after a full restart.
3. MotiveWave Preferences > Orders: enable *Send Email On Filled Order*.

Inspect any captured message, and what the parser makes of it, with
`./trading-journal mwmail parse data/mw-raw/<file>.eml`.

**From another machine.** When MotiveWave runs on a desk machine and the journal on a server,
mwrelay (by the same author, to be published) is a small store-and-forward SMTP relay
for exactly these e-mails: MotiveWave mails it on localhost, it queues each message in SQLite
and forwards it unchanged to every configured journal, directly or over an SSH tunnel, at least
once. Its menu bar companion shows the day's P&L from the journal's API.

## TopstepX API

A trader connects their own TopstepX username and API key under Settings > TopstepX; the key is
stored AES-256-GCM encrypted under `secrets.encryption_key`. The journal then pulls trades (half
turns aggregated per order into the same import pipeline) on a schedule gated to CME hours:
every 60 minutes while Globex trades, every 15 minutes inside the RTH window, on page visits
(throttled) and when a consumer app or MCP client reads trades or the data version. New fills
bump the data version like any import. The same connection serves exact-contract bars for the
charts (see Market data).

## IBKR Client Portal Gateway

A gateway belongs to a journal user, one per IB login, and holds no secret: its session is
established by logging in on the gateway's own browser page with 2FA. The journal polls the
rolling 7-day trade window on the TopstepX cadence, keeps the session alive (tickle),
reauthenticates dropped brokerage sessions silently and shows "needs login" when the SSO
session expires. MotiveWave rows for a live-synced account are filtered from imports past the
per-account cutover, so the two paths never double count. `ibkr.allowed_gateway_hosts` is the
SSRF boundary for the hosts non-admin users may register.

## Market data

Charts, excursions, the lab and the AI briefings read OHLCV bars from a shared cache in the
database. Bars come from Yahoo Finance (continuous front-month futures, no key) and, when a
TopstepX connection exists, from the exchange feed for the exact contract. A worker warms the
cache after each import, a nightly pass completes the last days, a session warm-up keeps today's
one-minute bars fresh for every root you traded lately, and old intraday bars are pruned after
`market_data.bar_retention_days`.

## Reconciling

`/admin/transactions` edits, soft-deletes and restores individual fills, or deletes a whole
matched trade in one action: every fill of the trade is tombstoned and the contract is rebuilt,
so neighbouring trades keep their entry times and journal links. Deleted fills can be listed and
restored. A trade that shares a fill with a neighbour (a flip) is flagged `flip-linked`.
`/reconcile/copy` compares the legs of copy sets across accounts.
