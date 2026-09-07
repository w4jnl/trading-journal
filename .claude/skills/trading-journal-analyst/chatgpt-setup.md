# Using the trading-journal analyst in ChatGPT

Same workflow as [SKILL.md](SKILL.md), same 14 MCP tools, same names. ChatGPT reaches the
journal through an **MCP connector (ChatGPT calls it a plugin) that signs in with OAuth** —
there is no key to generate, copy or store anywhere. The server-side prerequisite is
`mcp.oauth` enabled on the journal (one-time, see `docs/mcp-oauth-setup.md`).

## Connect (once)

1. ChatGPT → **Settings → Connectors (Plugins) → Create**.
2. **Server URL:** `https://YOUR_HOST/mcp` — the `/mcp` path, not the site root.
   **Authentication:** **OAuth**.
3. Open **Advanced OAuth settings** only to confirm what ChatGPT discovered; it should read
   *Registration method: Dynamic Client Registration (DCR)* with the default scopes `mcp` and
   `offline_access` checked. Leave it exactly like that — there is no client ID or secret to
   fill in. (The "CIMD is unavailable" notice is expected; the journal advertises DCR only.)
4. **Create.** ChatGPT sends you to the journal: log in as usual (your SSO), then press
   **Allow** on the page that reads *"ChatGPT wants to access your trading journal as …"*.
   You land back in ChatGPT with the tools listed.

To disconnect: journal **Settings → Connected apps → Revoke** (immediate), then remove the
connector in ChatGPT.

## Instructions for the analyst

Put the text from [chatgpt-instructions.md](chatgpt-instructions.md) (everything below the
divider) where your ChatGPT setup keeps standing instructions — a **Project** with the
connector enabled is the natural home; a custom GPT that has the connector enabled works
the same way. It is the ChatGPT twin of SKILL.md: same workflow, same tool names, trimmed
to fit the instructions-box size limit.

Then: *"Review yesterday's trading"* runs the daily flow; *"How is today going?"* the
read-only P&L check; *"Analyze trade 4412"* the on-demand flow.

## Notes
- Several journals (e.g. prod and a test instance)? Connect each as its own plugin; the
  instructions tell the model to call `server_info` first when more than one is present.
- Writes are additive only (create entry / append note / fill a blank tag or strategy). The
  tools never overwrite or delete, so an agent can't damage existing journal content.
- Re-runs are safe: the `<!-- tj-ai-mcp:v1 -->` marker stops a day from being analyzed twice.
- The REST API (`/api/v1`, per-user `tj_…` keys, OpenAPI at `/api/v1/openapi.json`) still
  exists for scripts and other apps; it is no longer needed for ChatGPT. The earlier
  "Custom GPT with an Action" route is retired in favour of the connector.
