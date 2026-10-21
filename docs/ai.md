# AI: in-app reviews and MCP

[‹ README](../README.md)

Two directions, usable together. The app can call a model itself (**in-app AI**: reviews,
plan drafts, stop and target, strategy drafts, grid explanations), and an outside assistant can
read and write the journal through **MCP**. Neither is required; without a provider the AI
buttons are hidden.

## In-app AI

The Analyze buttons call a chat-completions endpoint from the server. Two sources:

- **Each user's own key** (Settings > AI). Pick OpenAI, Anthropic (Claude), Google Gemini,
  Mistral or OpenRouter, paste an API key and a model (**Load models** lists the ones the key
  can use). Saving makes a one-word test call first and stores the key AES-256-GCM encrypted
  under `secrets.encryption_key`; a user's own provider wins over the server default. Admins can
  also enter any OpenAI-compatible URL (a local model server, Groq, …); regular users get the
  fixed hosted endpoints only, so nobody can point the server at an internal host. On by default
  once an encryption key is set; `ai.user_keys: false` turns it off.
- **A server-wide default** (`ai.enabled` with an OpenAI-compatible `ai.base_url`, such as a
  local MLX or llama.cpp server) for every user without a key.

A ChatGPT or Claude subscription does not include API access: the providers bill API keys per
use, separately (a review is a few thousand tokens). Those subscriptions reach the journal the
other way round, through MCP.

What the model gets is the app's own numbers, never a bare prompt: a trade review is briefed
with the plan, the fills, the price path and the session as it stood at the entry (optionally
the chart image, for vision models); a day review with the plan and the coach's findings; a week
review with the digest. The model writes; the app measures. A review never writes your tag,
strategy or labels: it records suggestions you accept or dismiss, and each review names one
lesson for the ledger. Reviews run in the background and lock only the note they are writing.
See [features.md](features.md#ai-reviews) for the behaviour on the page.

**The method is data.** `internal/aimethod` owns what the models work by: the generic skill (role,
grounding rules, the glossary of the evidence the app hands over, the format), one skill per entry
scope (trade, group, day, week), the lesson and verdict contracts and the label vocabulary. The
in-app prompts render from it; the MCP server serves it as instructions at `initialize`, as the
resource `trading-journal://method`, as the tool `get_review_method` and as one prompt per mode
(`review_day`, `review_entry`, `today`, `review_week`, `plan_day`), and the client skills only
point at it, so it exists once and carries a version (`aimethod.Version`, recorded on every review marker as
`method=` and on every stored output). The contracts and the markers are not user-editable: the
parsers and the review loop depend on them.

**The trader profile.** Settings > AI holds how the method applies to one trader: their style
(scalper, day trader, swing), the evidence they weigh most in order (short-term levels, session
structure, volume profile, gamma map, coach checks, playbooks), their markets and free-text standing
instructions, next to the review language. The profile is one cached block in every review and plan
(after the label vocabulary, before the playbooks), it sizes the evidence the app sends (a scalper
gets more one-minute bars and fewer fifteen-minute ones, a swing trader the reverse and no one-minute
window; a trader whose focus leaves out gamma gets the walls, not the whole map), and it reaches
every MCP client through `server_info.profile`, the resource `trading-journal://profile`, the
prompts and `get_review_method`. The method itself, the contracts and the markers stay the
journal's. A review can be asked for in another language once: `?lang=Czech` on the in-app review
routes, the `language` argument on the MCP prompts and on `get_review_method`.

**Caching.** A call is blocks (`ai.Request`): the method, the label vocabulary and the playbooks
lead and are flagged cacheable, the item follows. Anthropic receives `cache_control` on the last
cached system block and the last cached user block and bills the prefix at the cached rate from the
second call within five minutes (prefixes under 1024 tokens, or 4096 on some models, are not cached;
the method plus the vocabulary clears that). OpenAI, xAI and the other OpenAI-compatible providers
cache prefixes on their own, which the stable-first order serves. The `-d` log shows per call
`ai usage model=… input=… output=… cacheRead=… cacheWrite=…`.

**What a review costs.** The evidence is JSON; bars are one string per row ("time open high low
close") so the session windows and the chart stay a few thousand tokens, the trader's own words
exclude earlier AI blocks, and a day review builds on the day's recorded lessons. A trade review
runs about five to seven thousand input tokens with a typical playbook list, more with a chart image.

**The day map in the model's evidence.** The plan draft, the per-note reviews of a trade, a group
or a day, and the levels suggestion on an open position all carry the day map (`/levels`) for the
markets involved: the strongest zones with what meets there, the nearest zones above and below the
entry (or the last price), the expected range from the daily ATR, and the day type with its lean.
A review is asked to judge the location, whether the entry, the exit and the stop sat at a zone or
in the open; the plan draft takes its levels from the zones and the gamma map. The map's scores
rank, they do not forecast, and the prompts say so.

## MCP

The journal is an MCP server at `POST /mcp` (`mcp.enabled`, on by default). Claude Code, the
Claude apps, ChatGPT and any other MCP client can list and read trades, positions, journal
entries, strategies, daily stats, the day plan, the coach's checks, the digest and a trade's
chart image, and can create entries, append notes, assign strategies and tags, set the day plan
and a position's plan, and prepare a day for review. Values written this way are marked `api`
on the note until the trader saves it.

Two ways to authenticate:

- A per-user `tj_` API key in the client's connector configuration (the same key as the REST
  API).
- **OAuth 2.1**: the journal is its own authorization server (`mcp.oauth`). Point the client at
  `/mcp`, log in, click Allow, and revoke it any time under Settings > Connected apps. No key is
  copied into any connector. Clients register themselves (RFC 7591); the consent page names the
  client before a code is issued. Setup and the reverse-proxy routing it needs are in
  [mcp-oauth-setup.md](mcp-oauth-setup.md).

A ready-made analysis workflow ships as the `trading-journal-analyst` skill
(`.claude/skills/trading-journal-analyst/`): it reviews a trading day, groups copytrader
fan-out, writes the per-trade and per-group analysis once and synthesises the daily journal; the
same folder has instructions for a ChatGPT project. The tool list and the REST surface are in
[API.md](../API.md).
