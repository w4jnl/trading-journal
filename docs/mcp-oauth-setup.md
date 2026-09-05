# MCP OAuth: connect ChatGPT, claude.ai and Claude Code without static keys

**Date:** 2026-09-05 · **From:** trading-journal · **Concerns:** the operator (Traefik/Authelia host) and anyone connecting an AI client · **Applies to:** journal ≥ the `mcp.oauth` feature (rolled out in three PRs, see *Rollout* below)

The journal's MCP server at `/mcp` has always been protected by a per-user static API key (`Authorization: Bearer tj_…`). That works for scripts and for Claude Code when you paste the key into its config, but **ChatGPT's MCP connector cannot use a static key at all** — it supports only OAuth or "no auth", and it registers itself as an OAuth client automatically. Claude's connectors (claude.ai custom connectors, Claude Code's `claude mcp add --transport http`) speak the same OAuth flow, so the same mechanism lets you retire the copied-around key for them too.

The app therefore acts as its own small **OAuth 2.1 authorization server** for `/mcp`. The moving parts, in one picture:

```
  AI client (ChatGPT / claude.ai / Claude Code)
     │
     │ 1. POST /mcp (no token)
     ▼
  trading-journal ── 401 + WWW-Authenticate: Bearer resource_metadata="…/.well-known/oauth-protected-resource"
     │
     │ 2. GET /.well-known/oauth-protected-resource      ─┐ public (bypass Authelia)
     │ 3. GET /.well-known/oauth-authorization-server    ─┤
     │ 4. POST /oauth/register   (client registers itself, gets a client_id)  ─┘
     │
     │ 5. opens YOUR browser at /oauth/authorize?client_id=…&code_challenge=…
     ▼
  Authelia login (the normal one)  →  journal shows "Allow <ChatGPT|Claude|Claude Code> to access your journal?"  →  Allow
     │
     │ 6. 302 back to the client with a one-time code
     │ 7. POST /oauth/token (code + PKCE verifier)  → access token (1 h) + refresh token (30 d)   ─ public
     │ 8. POST /mcp with Authorization: Bearer tjo_…  → tools work, exactly as with a tj_ key
     ▼
```

What does **not** change: `tj_` API keys keep working for `/api/v1` and remain accepted on `/mcp`, so nothing you have today breaks. OAuth tokens are valid **only** on `/mcp` (they are audience-bound to it); the REST API stays key-only.

## 1. App configuration (`config.yaml`)

```yaml
mcp:
  enabled: true
  oauth:
    enabled: true
    # The public origin the connectors reach the app on. It is written into the
    # discovery documents and MUST equal the URL they fetch them from, so it is
    # configured explicitly, never guessed from request headers.
    issuer: "https://trading.example.com"
    # Show a one-click approval page naming the client before issuing a code.
    # Keep this on: anyone can register a client, so with auto-approve a crafted
    # link could mint a token for a signed-in user without them noticing.
    consent: true
    access_token_ttl_sec: 3600        # 1 hour; clients refresh silently
    refresh_token_ttl_sec: 2592000    # 30 days of inactivity before re-login
    code_ttl_sec: 60                  # raise to 120 if a client is slow to exchange
    # Optional: restrict which https hosts clients may register as redirect
    # targets. Loopback http (Claude Code's local callback) is always allowed.
    # allowed_redirect_hosts: ["chatgpt.com", "claude.ai"]
```

Restart the app. On boot it logs `MCP OAuth discovery enabled issuer=… resource=…`. With `oauth.enabled` absent or `false` nothing changes: no OAuth routes are registered and `/mcp` stays key-only.

## 2. Traefik: which paths bypass Authelia

Today `/api/v1` and `/mcp` have priority-100 routers that skip the Authelia middleware (the app enforces its own auth there). The OAuth endpoints split the same way:

| Path | Who calls it | Authelia? |
|---|---|---|
| `/.well-known/oauth-protected-resource` (and `…/mcp`) | the AI client, no session | **bypass** |
| `/.well-known/oauth-authorization-server` | the AI client, no session | **bypass** |
| `/oauth/register` | the AI client, no session | **bypass** |
| `/oauth/token` | the AI client, no session | **bypass** |
| `/oauth/authorize` | **you, in a browser** | **must go THROUGH Authelia** — that login is how the app learns who is approving (`Remote-User`) |

Add one router next to your existing `trading-mcp` one. Copy its values; only the name and rule differ:

```yaml
      # OAuth discovery + machine endpoints: same Authelia bypass as /api/v1 and
      # /mcp. The client has no browser session when it calls these. The app
      # enforces its own checks (PKCE, single-use codes, registered redirects).
      # NOTE: /oauth/authorize is deliberately NOT listed here.
      - "traefik.http.routers.trading-oauth.rule=Host(`trading.example.com`) && (Path(`/oauth/token`) || Path(`/oauth/register`) || PathPrefix(`/.well-known/oauth-authorization-server`) || PathPrefix(`/.well-known/oauth-protected-resource`))"
      - "traefik.http.routers.trading-oauth.entrypoints=websecure"
      - "traefik.http.routers.trading-oauth.tls.certresolver=myresolver"
      - "traefik.http.routers.trading-oauth.service=trading"
      - "traefik.http.routers.trading-oauth.priority=100"
```

`/oauth/authorize` needs no label: it falls through to your catch-all host router, which applies Authelia. If you ever see the consent page without having logged in, or the app answers `/oauth/authorize` with a 401/redirect loop, the path has ended up on a bypass router by mistake.

Firewalling the bypass routers by source IP? Anthropic's connector traffic originates from `160.79.104.0/21`; ChatGPT's egress ranges are published by OpenAI and change, so an allow-list there is not recommended.

## 3. Connecting each client

All three clients discover everything from the single URL `https://trading.example.com/mcp`. You never type a client ID or secret.

### ChatGPT (MCP connector)
1. ChatGPT → Settings → Connectors → *Create* (developer mode / custom connector).
2. MCP server URL: `https://trading.example.com/mcp`. Authentication: **OAuth**.
3. ChatGPT registers itself, then opens the journal's authorize page → Authelia login → **Allow** for "ChatGPT".
4. The tool list appears. Import nothing else; the OpenAPI/Actions setup in `.claude/skills/trading-journal-analyst/chatgpt-setup.md` becomes optional.

### claude.ai (custom connector)
1. Settings → Connectors → *Add custom connector*.
2. Remote MCP server URL: `https://trading.example.com/mcp`. Leave OAuth client ID/secret **empty** (the app supports dynamic registration).
3. Click *Connect* → Authelia login → **Allow** for "Claude". Tools appear with the same read/write grouping as before (annotations are unaffected by auth).

### Claude Code (CLI)
```bash
claude mcp add --transport http trading-journal https://trading.example.com/mcp
claude mcp login trading-journal          # opens the browser; --no-browser prints a URL to paste
```
Claude Code registers itself with a loopback callback (`http://localhost:<random port>/callback`), the app accepts that per RFC 8252, and after **Allow** the token is stored by Claude Code and refreshed automatically. Remove the old `--header "Authorization: Bearer tj_…"` entry from your MCP config; the key itself can stay valid for `/api/v1` scripts.

### Disconnecting
Settings → **Connected apps** lists every approved client (name, when, last used) with a **Revoke** button. Revoking kills that client's access and refresh tokens immediately — no config edits, no restart. Revoking in the journal is the authoritative action; also remove the connector on the client side so it stops retrying.

## 4. Rollout

| Stage | What is live | What to do |
|---|---|---|
| **PR1** (this doc) | Schema, config, the two discovery documents | Add the `mcp.oauth` config + the Traefik router. `curl https://trading.example.com/.well-known/oauth-authorization-server` must return JSON with `"issuer": "https://trading.example.com"`. `/mcp` still accepts only `tj_` keys. |
| **PR2** | Registration, authorize + consent, token endpoint; `/mcp` accepts `tjo_` tokens and answers unauthenticated calls with the `WWW-Authenticate` challenge | Connect the clients as in §3. |
| **PR3** | Settings → Connected apps (revoke), housekeeping of expired tokens | Nothing; use Revoke to drop a client. |

## 5. Troubleshooting

- **Client says "could not discover authorization server"** → `curl -i https://trading.example.com/mcp` must return `401` with a `WWW-Authenticate: Bearer resource_metadata="…"` header (PR2+), and both well-known URLs must return JSON with `Cache-Control: no-store`. A `302` to Authelia instead means the path is not on a bypass router.
- **`issuer` mismatch** → the `issuer` in `/.well-known/oauth-authorization-server` must be byte-identical to the origin the client used (`https://trading.example.com`, no trailing slash, no path). Fix `mcp.oauth.issuer`.
- **Authorize page shows an error instead of the consent form** → "unknown client" or "redirect_uri not registered": the client registered with a different redirect than it is now using. Remove and re-add the connector on the client side. The app never redirects to an unregistered URI, by design.
- **Approved, but the client immediately fails** → the code is single-use and lives `code_ttl_sec` (60 s). If the client is consistently slow, raise it to 120.
- **Consent page appears without an Authelia login** → `/oauth/authorize` has landed on a bypass router. Remove it from that rule.
- **Clicking Allow does nothing** (console: "Refused to load https://chatgpt.com/… because it does not appear in the form-action directive") → you are on v2.11.0, where the page's Content-Security-Policy blocked the redirect back to the client. Fixed in v2.11.1: the consent page allows exactly the client's verified redirect origin. No configuration involved.
- **Access token rejected after ~1 h** → expected; clients refresh with the refresh token. If refresh also fails with `invalid_grant`, the grant was revoked (Settings → Connected apps) or the refresh token was reused — the whole family is revoked on reuse as a theft guard. Reconnect.
