# Using the trading-journal analyst in ChatGPT

The workflow is identical to [SKILL.md](SKILL.md); only the transport differs.
ChatGPT reaches the trading-journal over its own tool mechanism, with the API key
stored in the tool config (never pasted into a chat). Two options:

## Option A — Custom GPT with an Action (works on most plans)

1. Generate a per-user API key in the web UI at `/settings`.
2. Create a **Custom GPT** → **Configure** → **Actions** → **Create new action**.
3. **Import** the schema from `https://YOUR_HOST/api/v1/openapi.json` (or paste
   it). This exposes the REST operations: list/get trades, list/get journal,
   list/get strategies, create entry, append note, chart PNG.
4. **Authentication** → **API Key** → **Bearer**, value `tj_...` (your key). It is
   stored in the GPT config, not in prompts.
5. Paste the workflow into the GPT's **Instructions**: copy the body of
   [SKILL.md](SKILL.md) (the modes, the `<!-- tj-ai:v1 -->` marker rule, and the
   analysis rubric). Replace the MCP tool names with the matching operations —
   they map 1:1:

   | SKILL.md tool | Action operation |
   |---|---|
   | `prepare_day` | `POST /journal/prepare-day` |
   | `list_trades` | `GET /trades` |
   | `get_trade` | `GET /trades/{id}` |
   | `list_journal` / `get_journal_entry` | `GET /journal` / `GET /journal/{id}` |
   | `list_strategies` / `get_strategy` | `GET /strategies` / `GET /strategies/{id}` |
   | `create_journal_entry` | `POST /journal` |
   | `append_note` | `POST /journal/{id}/notes` |
   | `assign_strategy` | `POST /journal/{id}/strategy` |
   | `set_tag` | `POST /journal/{id}/tag` |

6. **Charts**: Actions return JSON, so ChatGPT reasons from the numeric `chart`
   field on the journal/trade detail (OHLC bars + entry/exit markers) rather than
   the rendered PNG. The analysis is the same; it just doesn't "see" the image.

Then: *"Review yesterday's trading"* runs the daily flow; *"Analyze trade 4412"*
runs the on-demand flow.

## Option B — MCP connector (ChatGPT plans that support remote MCP)

Add `https://YOUR_HOST/mcp` as a connector with header
`Authorization: Bearer tj_...`. This gives ChatGPT the same MCP tools (and the
`get_chart_image` image tool) that Claude uses. Use SKILL.md as the GPT
instructions unchanged.

## Notes
- The key is full-access to your own data; revoke it in `/settings` if exposed.
- Writes are additive only (create entry / append note). The API never overwrites
  or deletes, so an agent can't damage existing journal content.
- Re-runs are safe: the `<!-- tj-ai:v1 -->` marker stops a day from being
  analyzed twice.
