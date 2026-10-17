# Users and sign-in

[‹ README](../README.md)

The app is multi-user. Every request resolves to a row in the `users` table, and data isolation
is enforced at the query layer: two users on the same database never see each other's accounts,
trades, journal entries, balances or sync log. A single trader is simply the only user.

## Auth modes

`auth.mode` in `config.yaml` picks one:

- `local` (default): the built-in login page at `/login`, bcrypt passwords, SQLite-backed
  sessions, HMAC-bound CSRF tokens on every mutating request. The cookie is `HttpOnly`,
  `SameSite=Lax` and `Secure` by default; set `auth.cookie_secure: false` only for plain HTTP on
  a trusted private network.
- `proxy`: trust identity headers (`Remote-User`, `Remote-Email`, `Remote-Groups`) from a
  reverse proxy such as Authelia, oauth2-proxy or Caddy `forward_auth`. See Proxy mode below.

`auth.session_secret` is the HMAC key for CSRF tokens, session-id binding and the act-as cookie:
any string of 32 bytes or more (or `base64:<random>`), so tokens survive restarts. It can be
supplied as the `TJ_SESSION_SECRET` environment variable instead, which wins over the file.
Likewise `secrets.encryption_key` (`TJ_ENCRYPTION_KEY`) is the key users' AI provider keys and
TopstepX credentials are encrypted under: generate it with `openssl rand -hex 32` and back it up
with the config, since losing it means every user re-enters their keys.

## Managing users

Two surfaces, one code path.

**Web:** `/admin/users` (admins only): create, reset password, promote and demote, deactivate and
activate, delete, act as. Linked from the sidebar's Admin entry whenever the signed-in user is
an admin.

**CLI:** the binary doubles as a user-management tool, which is how the first admin gets a
password before any UI is reachable:

```sh
./trading-journal users -h                   # full help

./trading-journal users add bob              # prompts for a password, twice
./trading-journal users add jane --admin --email=jane@example.com
./trading-journal users reset bob            # also destroys bob's sessions

./trading-journal users list                 # id, username, admin, active, email

./trading-journal users promote bob
./trading-journal users demote bob
./trading-journal users deactivate bob       # disables login and kills sessions
./trading-journal users activate bob

./trading-journal users delete bob           # interactive: retype the username
./trading-journal users delete bob --yes
```

`users reset` and `users add` read the password from the terminal and hang in a script; for
non-interactive setups use proxy mode or mint API keys directly (see `docs/development.md`).

`delete` removes everything that belongs to the user in one transaction with a foreign-key
check at the end: accounts, trades, executions, journal entries of every scope and their
attachments, balance anchors, daily stats, templates, sessions, API keys and devices; `sync_log`
rows keep the audit history with the user set to NULL. On disk it removes the user's attachment
files and `imports/{inbox,processed,failed}/<name>/` and `imports/uploads/<id>/`. Both surfaces
refuse to delete the last active admin and refuse to let an admin delete the account they are
signed in as.

## Act as

Admins can browse the app as another user without learning their password: **Act as** on
`/admin/users`. A signed cookie carries the target id, every page shows a red "Acting as ALICE,
signed in as ADMIN" banner with a Return button, and the target's `is_active` is re-checked on
every request. Every act-as request uses the target's user id end to end (queries, sync log,
import attribution); the admin's identity stays in the cookie for the banner only.

## Proxy mode

```yaml
auth:
  mode: "proxy"
  bootstrap_user: "admin"             # still required: owns flat inbox drops
  proxy:
    user_header: "Remote-User"
    email_header: "Remote-Email"
    display_name_header: "Remote-Name"
    groups_header: "Remote-Groups"
    admin_group: "admin"              # a comma-list match grants is_admin
    trusted_proxies:                  # CIDR allowlist for r.RemoteAddr
      - "172.20.0.0/16"
      - "127.0.0.1"
    auto_provision: false             # create users on first sight?
    logout_url: ""                    # where /logout sends the browser
```

On every request the proxy authenticator checks that `r.RemoteAddr` falls inside
`trusted_proxies` (otherwise the headers are ignored and the request is unauthenticated), reads
`user_header`, looks the user up (creating the row when `auto_provision` is on; proxy users
never log in locally), and refreshes e-mail, display name and the admin flag from the headers so
changes in the SSO propagate. `/login` redirects to `/`; `/logout` redirects to `logout_url`.

The CIDR allowlist is the only thing between the internet and impersonation, so set it, and
make sure the proxy strips client-supplied `Remote-*` headers before forwarding. A minimal
Authelia setup uses `forwarded_headers` with `Remote-User`, `Remote-Email` and `Remote-Groups`
and an access rule that grants the `admin` group to whoever administers the journal; oauth2-proxy
and Caddy `forward_auth` work the same way.

## API keys and devices

Per-user API keys (`tj_…`) are made under Settings and sent as `Authorization: Bearer`. An
**agent** key reads everything of its user and appends notes; a **device** key, made by pairing a
phone (Settings > Phone app shows a code and a QR for five minutes), may also edit and delete
journal entries, keep balance anchors, change settings and register for push. Keys are stored
hashed and revoked from the same page. MCP clients can instead sign in through the built-in
OAuth 2.1 server; see [ai.md](ai.md) and [mcp-oauth-setup.md](mcp-oauth-setup.md).
