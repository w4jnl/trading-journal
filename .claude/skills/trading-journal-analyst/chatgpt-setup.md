# Using the trading-journal analyst in ChatGPT

Same workflow as [SKILL.md](SKILL.md); only the transport differs. ChatGPT reaches the
trading-journal over its own tool mechanism, with the API key stored in the tool config
(never pasted into a chat). The recommended path is a **Custom GPT with an Action** against
the REST API.

## Option A — Custom GPT with an Action (works on most plans)

1. **Get a key.** In the web UI at `https://YOUR_HOST/settings`, generate a per-user API
   key (`tj_…`). It's full-access to your own data; revoke it there if exposed.

2. **Create the GPT.** ChatGPT → **Create a GPT** → **Configure** → **Actions** →
   **Create new action**.

3. **Add the schema.** The OpenAPI lives at `https://YOUR_HOST/api/v1/openapi.json`, but
   that endpoint requires the key, so ChatGPT's "Import from URL" (which fetches
   unauthenticated) will 401. Instead **fetch it yourself and paste it**:
   ```bash
   curl -H "Authorization: Bearer tj_…" https://YOUR_HOST/api/v1/openapi.json
   ```
   Paste the JSON into the schema box. The server URL comes back **absolute** (derived
   from the host you fetched it from, e.g. `"servers": [{ "url":
   "https://YOUR_HOST/api/v1" }]`), so there's nothing to hand-edit. (If you ever see a
   relative `"/api/v1"`, set it to your absolute base — ChatGPT requires an absolute URL.)

4. **Authentication** → **API Key** → **Auth Type: Bearer** → paste your `tj_…` key. It's
   stored in the GPT config, not in prompts.

5. **Privacy policy.** A **public** GPT requires a Privacy Policy URL; a **private** or
   "only people with the link" GPT (just you) does not — leave it unpublished.

6. **Instructions.** Open [chatgpt-instructions.md](chatgpt-instructions.md), copy
   **everything below the divider line**, and paste it into the GPT's **Instructions** box
   **as-is** — it's already adapted to the Action operations (no edits needed).

Then: *"Review yesterday's trading"* runs the daily flow; *"Analyze trade 4412"* runs the
on-demand flow.

### Operation names (for reference)
The instructions call the operations by their `operationId`; these map 1:1 to the MCP tools
Claude uses:

| MCP tool (SKILL.md) | Action operationId | REST |
|---|---|---|
| `prepare_day` | `prepareDay` | `POST /journal/prepare-day` |
| `list_trades` | `listTrades` | `GET /trades` |
| `get_trade` | `getTrade` | `GET /trades/{id}` |
| `list_journal` / `get_journal_entry` | `listJournal` / `getJournalEntry` | `GET /journal` / `GET /journal/{id}` |
| `list_strategies` / `get_strategy` | `listStrategies` / `getStrategy` | `GET /strategies` / `GET /strategies/{id}` |
| `create_journal_entry` | `createJournalEntry` | `POST /journal` |
| `append_note` | `appendNote` | `POST /journal/{id}/notes` |
| `assign_strategy` | `assignStrategy` | `POST /journal/{id}/strategy` |
| `set_tag` | `setTag` | `POST /journal/{id}/tag` |

**Charts:** there is no `get_chart_image` equivalent — Actions return JSON, so the GPT
reasons from the numeric `chart` field on `getTrade` / `getJournalEntry` (OHLC bars +
entry/exit markers). The `getTradeChart` / `getJournalChart` operations return a PNG the
GPT can't see; the instructions tell it to ignore them.

## Option B — MCP connector (ChatGPT plans that support remote MCP)
Add `https://YOUR_HOST/mcp` as a connector with header `Authorization: Bearer tj_…`. This
gives ChatGPT the same MCP tools (including the `get_chart_image` image tool) that Claude
uses; use [SKILL.md](SKILL.md) as the instructions unchanged. (Availability and the exact
auth flow depend on your ChatGPT plan/build.)

## Notes
- The key is full-access to your own data; revoke it in `/settings` if exposed.
- Writes are additive only (create entry / append note). The API never overwrites or
  deletes, so an agent can't damage existing journal content.
- Re-runs are safe: the `<!-- tj-ai:v1 -->` marker stops a day from being analyzed twice.
