# Security policy

The trading journal is a network service meant for one trader or a few, on a private network or
behind a reverse proxy. It serves a web UI, a JSON API and an MCP endpoint, accepts CSV uploads
and e-mails, polls brokers and calls model providers with the keys you give it. What an attacker
can reach, and what stands in the way:

- **Sign-in.** Local mode stores bcrypt password hashes, binds sessions and CSRF tokens to an
  HMAC key (`auth.session_secret`) and marks the cookie `HttpOnly`, `SameSite=Lax` and
  `Secure`. Proxy mode trusts `Remote-User` and friends only from `auth.proxy.trusted_proxies`;
  that CIDR list is the whole boundary, and the proxy must strip client-supplied headers.
- **API keys** (`tj_…`) are per user, stored hashed and shown once. An agent key reads its
  user's data and appends notes; only a device key made by pairing may edit, delete or change
  settings. The MCP OAuth server issues short-lived tokens after a consent page that names the
  client; anyone can register a client, which is why consent stays on.
- **Stored secrets.** Users' AI provider keys and TopstepX credentials are AES-256-GCM encrypted
  under `secrets.encryption_key`; the API never returns them. Backups contain `config.yaml` and
  are written mode 0600.
- **Outbound calls.** The server calls the model provider, gex-engine, Yahoo, TopstepX and the
  IBKR gateways you configure. Regular users may only pick hosted AI presets; a free-form URL and
  an IBKR gateway outside `ibkr.allowed_gateway_hosts` are admin-only, which is the control
  against pointing the server at an internal host.
- **Uploads and e-mail.** CSV uploads are parsed, never executed, and capped in size; journal
  attachments are stored under the data directory and served by id. The MotiveWave SMTP listener
  authenticates with the configured credentials but is meant for a LAN or VPN, never a public
  address.
- **Pages** carry a Content Security Policy with no inline scripts (a per-request nonce) and
  render user content through `html/template`.
- **Data isolation.** Every query is scoped to the signed-in user; admins can act as another
  user through a signed cookie that is re-checked on every request.

## Supported versions

Only the latest release gets fixes. Update from the
[releases page](https://github.com/w4jnl/trading-journal/releases) or pull the next image.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting for this repository:
<https://github.com/w4jnl/trading-journal/security/advisories/new>. Describe what the attacker
controls (a request to the UI or the API, an MCP call, a CSV, an e-mail, a proxy header, a
broker response) and what they gain. You will get an answer within a week; fixes ship as a patch
release with a note in `CHANGELOG.md`. Please do not open a public issue for something
exploitable before it is fixed.

## Deployment advice

- Terminate TLS at your proxy, keep `auth.cookie_secure` on and set `server.external_url`.
- Set `auth.session_secret` and `secrets.encryption_key` from the environment
  (`TJ_SESSION_SECRET`, `TJ_ENCRYPTION_KEY`) and back the encryption key up with the database.
- Keep the SMTP listener, the IBKR gateways and the gex-engine on the private network, and set
  `ibkr.allowed_gateway_hosts` before letting non-admin users add a gateway.
