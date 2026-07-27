# mw-smtp-ingest — feature spec

Spec for a new module in the trading-journal app. Follow the existing codebase
conventions for package layout, naming, config, logging, errors, and tests —
this document defines behavior only.

## 1. Purpose

MotiveWave (MW) can send an email on every order fill
(**Preferences → Orders → Send Email On Filled Order**). We point that at an
SMTP listener embedded in the journal app instead of a real mail provider
(MW's SMTP client fails STARTTLS against iCloud; and we don't want a mailbox
in the path anyway). This gives near-real-time fill capture for the accounts
that currently have **no API connector** and rely on manual MW exports:

- 5 × prop accounts on **Rithmic** (driven by the copytrader fan-out)
- 1 × live account at **AMP on CQG**

Topstep and IBKR already sync via their own API connectors — this module must
not touch those flows.

**Role of this data:** a *staging / trigger layer*. Email-derived fills give
immediacy (live position awareness, journal pre-population). The authoritative
record (exact fills, commissions) still comes from the existing manual-export
import; staging rows are reconciled against it later. Email data must never
silently become authoritative P&L.

## 2. Non-goals

- Not a general MTA: no relaying, no delivery to mailboxes, no outbound mail.
- No replacement of existing importers or the manual-export reconciliation.
- No exposure beyond LAN/VPN. This never listens on a public interface.

## 3. Shape

Embedded module (not a separate service), started from the app lifecycle
behind an enable flag, shut down gracefully with the app. Suggested library:
`github.com/emersion/go-smtp` (server) + stdlib `net/mail` for headers. MW
notifications are expected to be plain text; avoid pulling a MIME library
unless captured samples prove otherwise.

## 4. Configuration

Map onto the app's existing config system. Required knobs and defaults:

| Setting            | Default   | Notes                                             |
|--------------------|-----------|---------------------------------------------------|
| enabled            | false     |                                                   |
| listen_addr        | :2525     | bind LAN/Tailscale interface, never WAN           |
| tls_mode           | off       | `off` \| `implicit` \| `starttls`                 |
| tls_cert, tls_key  | —         | required unless tls_mode=off                      |
| smtp_username/pass | —         | if both set, AUTH (PLAIN/LOGIN) is required       |
| archive_dir        | data/mw-raw | verbatim `.eml` archive                        |
| forward_unparsed   | true      | store unparsed messages as staging rows too       |

**MW infers encryption from the port number**: 25 = plain, 465 = implicit
TLS, 587 = STARTTLS. So the deployment mapping is: plain on host port 25
(LAN only), or implicit TLS on host port 465. Implicit TLS = wrap the
listener (`tls.Listen` + `Serve`), *not* the server's STARTTLS config.

**go-smtp gotcha:** to advertise AUTH on a non-TLS session you must set
`AllowInsecureAuth = true` on the server, otherwise MW's login fails
opaquely. Acceptable here because plain mode is LAN/VPN-only by policy.
Also set sane `MaxMessageBytes`, `ReadTimeout`, `WriteTimeout`.

## 5. Message handling (the core contract)

On every received message (`Session.Data`), in order:

1. **Read fully, hash** — SHA-256 of the raw bytes; short form (12 hex) is
   the message id.
2. **Dedup** — if the hash was seen before, log and drop (still reply 250).
   MW/SMTP retries must not create duplicate rows. The dedup set must survive
   restarts (rebuild from the archive dir at startup, or persist hashes —
   whichever fits the codebase).
3. **Archive verbatim** — write `<utc-timestamp>_<hash>.eml` to archive_dir
   *before* any parsing. This is the audit trail and the parser-development
   corpus. Archiving failure is the only acceptable reason to reject a
   message.
4. **Parse** (see §6) and **insert a staging fill** through the existing
   ingest/persistence path.
5. **Always reply 250.** Parse or storage errors are logged, never bounced —
   a bounce surfaces as a cryptic error inside MW and can stall its mailer.
   The raw archive guarantees nothing is lost.

Bursts are normal: one basket action fans out to 5+ accounts ⇒ 5+ nearly
simultaneous emails. Handling must be concurrency-safe and archive names
collision-free (timestamp + hash).

AUTH failures must return the library's auth-failed error so the client gets
a proper 535 (don't swallow it — hung MW mailer otherwise).

## 6. Parsing — capture-first, then tune

The exact MW email format is **unknown until captured**. Therefore:

- Ship the parser as a tolerant, clearly-marked-provisional extractor over
  `subject + body`: side (BUY/SELL/BOT/SLD/BOUGHT/SOLD → buy/sell), qty,
  symbol (futures code `[A-Z]{1,4}[FGHJKMNQUVXZ]\d{1,2}`, fallback to known
  roots ES/MES/NQ/MNQ/…), price (`@ 6,852.25` / `price:` / `avg fill price`,
  strip thousands separators), account (`Account:`/`Acct:` token), order id
  (require an explicit `Order Id/Number/#` qualifier *and* a digit in the
  token — a bare `order` qualifier happily matches "Order Filled"), raw fill
  time string.
- `parsed = true` only if side + qty + price are all present.
- Provide a dev entrypoint (CLI subcommand or test helper, per codebase
  style) that runs the parser on an archived `.eml` and prints the result —
  that's the tuning loop once real samples exist.
- Unit-test the parser against fixture `.eml` files; add every captured real
  sample as a fixture.

## 7. Staging record

One row per email, through the existing storage layer (naming per codebase):

- `source` = `motivewave-email`
- `id` = archive filename / hash (stable; dedup + reconciliation join key)
- `received_at`, `date_header`, `from`, `to`, `subject`, `body_text`
- `parsed` bool; when true: `side, qty, symbol, price, account, order_id,
  fill_time_raw`
- `raw_path` (archive reference)

Semantics: staging fills are per-account (the copytrader clustering that
`prepare_day` does server-side keeps working — don't pre-cluster here).
Email prices are what MW printed; no commissions. Reconciliation against the
authoritative import should match on account + symbol + time window + qty
and mark staging rows superseded — never double-count into P&L.

## 8. Deployment / MW-side setup (for the README)

- MW **Preferences → Email**: SMTP host = journal host LAN IP; port 25
  (plain) or 465 (implicit TLS); username/password = configured AUTH creds;
  any from/to addresses. **MW only applies mail settings after a full
  restart** (Apply → OK → restart → Send Test Email).
- MW **Preferences → Orders**: enable *Send Email On Filled Order*.
- First run in **capture mode** (module enabled, but treat parser output as
  advisory) and collect: a manual fill, a copier fill on *each* prop
  account, an AMP/CQG fill. Two open questions the captures must answer:
  does MW email fills for orders it did not itself originate (copier), and
  does the body carry the account id? If either fails, this feature only
  covers MW-originated orders — surface that finding rather than papering
  over it.

## 9. Acceptance criteria

- [ ] Module off by default; enabling it starts the listener, app shutdown
      stops it cleanly.
- [ ] Every received message lands verbatim in archive_dir before parsing.
- [ ] Byte-identical resend ⇒ no second staging row (also across restart).
- [ ] Wrong AUTH password ⇒ 535 to the client; correct ⇒ mail accepted.
- [ ] tls_mode=implicit serves TLS-wrapped SMTP usable by a 465-style
      client; tls_mode=off works for a plain client on the LAN.
- [ ] Parseable fixture ⇒ staging row with correct side/qty/symbol/price/
      account; unparseable fixture ⇒ archived + (if forward_unparsed)
      staging row with `parsed=false`.
- [ ] Parser/storage errors never produce an SMTP error reply; they log.
- [ ] Parser fixtures include at least the synthetic samples now, real
      captured MW samples once available.
