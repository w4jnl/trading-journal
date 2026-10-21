---
name: trading-journal-analyst
description: Review and write the trading journal via the trading-journal MCP/API. Use when asked to analyze yesterday's trading, review my trades, write/synthesize my journal, do a daily trade review, plan the session, or analyze a specific trade or journal entry. The method lives on the journal server; this skill connects and follows it.
---

# Trading journal analyst

The journal server carries the review method: how to review a trade, a group, a
day or a week, how to plan a session, which lines every review ends with, the
write rules and the modes with their tool order. It is versioned and it changes
with the server, so **do not keep a copy here**: read it at the start of every
session and follow it.

## Every session

1. Call `server_info` on the trading-journal connector (with several journals
   configured, pick one instance and use it for the whole run). It names the
   server, the trader's review language, the marker line and the method version.
2. Read the method: the resource `trading-journal://method`, or the tool
   `get_review_method` when your client does not read resources. Follow it.
3. When your client lists the server's prompts, use them: `review_day`,
   `review_entry`, `today`, `review_week`, `plan_day`. They carry the same steps
   with the arguments filled in.

The method's rules that matter most, in one line each: `prepare_day` clusters and
creates the entries, you only ever pass the `entryId` values it returns; every
review ends with the lesson line and then the marker line from `server_info`,
and a note that already carries the marker is done; writes are additive and
only-if-unset; write the prose in the trader's language from `server_info`;
dates like `today`, `yesterday` and `last` are resolved on the server.

## Setup (one-time)

Every client connects to `https://YOUR_HOST/mcp` and **signs in with OAuth**
through the browser (the journal is its own authorization server, `mcp.oauth`
enabled; see `docs/mcp-oauth-setup.md`). Each connection shows up under the
journal's **Settings → Connected apps**, where Revoke cuts it off.

- **Claude Code**:
  ```bash
  claude mcp add --transport http trading-journal https://YOUR_HOST/mcp
  claude mcp login trading-journal      # browser → your login → Allow
  ```
- **Claude app** (claude.ai / desktop): Settings → Connectors → *Add custom
  connector* → URL `https://YOUR_HOST/mcp`, leave client ID/secret empty →
  *Connect* → log in → **Allow**.
- **ChatGPT**: Settings → Connectors → *Create* → server URL
  `https://YOUR_HOST/mcp`, authentication **OAuth** → *Create* → log in →
  **Allow**. Then put [chatgpt-instructions.md](chatgpt-instructions.md) into the
  Project or GPT that has the connector; steps in [chatgpt-setup.md](chatgpt-setup.md).
- **Scripts** keep using a per-user API key against the REST API; the method is
  at `GET /api/v1/ai/method`.

## Scheduling (daily, automatic)

- **Claude Code routine**: run `use trading-journal-analyst to review yesterday`
  each morning, for example on a `0 7 * * *` cron with the `schedule` skill.
- **Claude app**: a scheduled task with the same prompt and the connector enabled.
- **Plain cron** fallback: `claude -p "use trading-journal-analyst to review yesterday"`.

Each run is idempotent (the marker), so a missed or repeated day is safe.
