package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// MCPHandler builds the Model Context Protocol server exposed at /mcp. It is a
// thin tool surface over the same user-scoped logic the REST API uses, so AI
// clients (Claude Code / Claude app via an MCP connector, ChatGPT via an MCP
// connector) can read trades + journal and create/append entries without the
// key ever appearing in a prompt - it lives in the connector config and is
// verified by the same auth.APIKeyAuth.Middleware that guards /api/* (which is
// why MCPHandler is mounted behind it in main.go). The middleware injects the
// resolved auth.User into the request context; WithHTTPContextFunc forwards that
// context to every tool handler, so the handlers below reuse userID(ctx)-scoped
// loaders unchanged.
//
// Stateless mode keeps each request self-contained (no session bookkeeping),
// which suits a small request/response tool server behind key auth.
func (h *Handlers) MCPHandler() http.Handler {
	s := mcpserver.NewMCPServer("trading-journal", "v1",
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithToolHandlerMiddleware(h.mcpDebugMW),
	)
	h.registerMCPTools(s)
	return mcpserver.NewStreamableHTTPServer(s,
		mcpserver.WithStateLess(true),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return r.Context() // already carries auth.User from APIKeyAuth.Middleware
		}),
	)
}

// mcpDebugMW logs every tool call at debug level: tool name, the calling user,
// the raw arguments, whether the result was an error, and the duration. One hook
// for all tools (the tool name comes from the request). Near no-op when debug is
// off. Invaluable for tracing what an AI client actually sent and got back.
func (h *Handlers) mcpDebugMW(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if !slog.Default().Enabled(ctx, slog.LevelDebug) {
			return next(ctx, req)
		}
		start := time.Now()
		res, err := next(ctx, req)
		user := ""
		if u := auth.UserFrom(ctx); u != nil {
			user = u.Username
		}
		slog.Debug("mcp tool call",
			"tool", req.Params.Name,
			"user", user,
			"args", req.GetArguments(),
			"isError", err != nil || (res != nil && res.IsError),
			"dur", time.Since(start).String())
		return res, err
	}
}

// Tool annotations matter for the Claude app's connector page: it groups
// tools by readOnlyHint, and un-annotated tools all land under "Write/delete
// tools". Reads carry readOnlyHint so a user can always-allow them (the
// "how am I doing today" flow never prompts) while keeping the writes on
// ask; the writes carry destructiveHint=false because every one of them is
// additive or fill-a-blank - nothing here deletes or overwrites.
func (h *Handlers) registerMCPTools(s *mcpserver.MCPServer) {
	s.AddTool(mcpgo.NewTool("prepare_day",
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithDescription("Prepare a trading day for review: clusters copytrader fan-out into group entries and solos into trade entries, CREATES the missing journal entries (idempotent - reuses existing), and returns them. You then analyze each returned entry by its entryId (get_chart_image + append_note) and NEVER handle raw trade ids yourself. This is the entry point for the daily review. Defaults to the last trading day before today. The result also carries 'checks': the day's rule-based coach checks with trade ids and suggested labels - cite them as evidence in each entry's review and add the suggested labels you agree with."),
		mcpgo.WithString("date", mcpgo.Description("YYYY-MM-DD; default = the last trading day before today (skips weekends/holidays/no-trade days).")),
	), h.mcpPrepareDay)

	s.AddTool(mcpgo.NewTool("server_info",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Identify this server instance: service name, instance label (e.g. prod/dev), and the authenticated user. Call this first if multiple trading-journal connectors might be configured, and use only ONE instance for a whole run."),
	), h.mcpServerInfo)

	s.AddTool(mcpgo.NewTool("list_trades",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("List the caller's closed trades for a day or date range. Each row carries hasJournal and groupCount (use them to skip trades already journaled) and setId / setSize: copies of one decision across accounts (a copy set) share a setId, so group by it instead of guessing from timestamps. Page size 50. Dates are trading-timezone days; the keywords today, yesterday and last (last trading day) are resolved server-side - use day=\"today\" for today's trades, never compute the date yourself. Accounts hidden in Settings are left out unless accountId names one."),
		mcpgo.WithString("day", mcpgo.Description("Single trading day: YYYY-MM-DD or today|yesterday|last; sets from=to=day.")),
		mcpgo.WithString("from", mcpgo.Description("Range start YYYY-MM-DD or today|yesterday|last (inclusive).")),
		mcpgo.WithString("to", mcpgo.Description("Range end YYYY-MM-DD or today|yesterday|last (inclusive).")),
		mcpgo.WithBoolean("refresh", mcpgo.Description("Pull the latest fills from the brokers first and wait for them (up to ~15s) before listing. Use for intraday 'today' questions.")),
		mcpgo.WithString("symbolRoot", mcpgo.Description("Filter by root, e.g. MES.")),
		mcpgo.WithString("direction", mcpgo.Enum("long", "short")),
		mcpgo.WithString("outcome", mcpgo.Enum("win", "loss", "breakeven")),
		mcpgo.WithString("risk", mcpgo.Enum("planned", "past-stop"), mcpgo.Description("planned: trades with a stop or risk set; past-stop: planned trades whose measured heat (mae) exceeded the planned risk.")),
		mcpgo.WithNumber("accountId", mcpgo.Description("Filter to one account id.")),
		mcpgo.WithNumber("page", mcpgo.Description("1-based page; default 1."), mcpgo.DefaultNumber(1)),
		mcpgo.WithBoolean("includeRetired", mcpgo.Description("Include trades on retired accounts.")),
	), h.mcpListTrades)

	s.AddTool(mcpgo.NewTool("get_trade",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Get one trade in detail: fills (executions), linked trade-scope journal entry id, group count, and cached chart data."),
		mcpgo.WithNumber("id", mcpgo.Required(), mcpgo.Description("Trade id.")),
	), h.mcpGetTrade)

	s.AddTool(mcpgo.NewTool("list_journal",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("List the caller's journal entries (summary rows) for a day or range, optionally filtered by scope, tag, or a review label (mistake / emotion key; mistake=none means no mistake logged). Rows carry their labels. Newest first."),
		mcpgo.WithString("day", mcpgo.Description("Single day YYYY-MM-DD; sets from=to=day.")),
		mcpgo.WithString("from", mcpgo.Description("Range start YYYY-MM-DD (inclusive).")),
		mcpgo.WithString("to", mcpgo.Description("Range end YYYY-MM-DD (inclusive).")),
		mcpgo.WithString("scope", mcpgo.Enum("day", "week", "trade", "group")),
		mcpgo.WithString("tag", mcpgo.Enum("good", "neutral", "bad", "untagged")),
		mcpgo.WithString("mistake", mcpgo.Description("Only entries carrying this mistake key, or none.")),
		mcpgo.WithString("emotion", mcpgo.Description("Only entries carrying this emotion key.")),
		mcpgo.WithNumber("limit", mcpgo.Description("Max rows; default 100."), mcpgo.DefaultNumber(100)),
	), h.mcpListJournal)

	s.AddTool(mcpgo.NewTool("get_journal_entry",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Get one journal entry in detail: raw notes markdown (check it for the analysis marker before re-analyzing), trades overview + list, assigned strategy (id+name only - fetch get_strategy for the playbook), summary, and cached chart data."),
		mcpgo.WithNumber("id", mcpgo.Required(), mcpgo.Description("Journal entry id.")),
	), h.mcpGetJournalEntry)

	s.AddTool(mcpgo.NewTool("list_strategies",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("List the caller's strategies (id, short name, full markdown playbook)."),
	), h.mcpListStrategies)

	s.AddTool(mcpgo.NewTool("get_strategy",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Get one strategy: short name + full markdown playbook. Use to judge whether trades followed the assigned strategy."),
		mcpgo.WithNumber("id", mcpgo.Required(), mcpgo.Description("Strategy id.")),
	), h.mcpGetStrategy)

	s.AddTool(mcpgo.NewTool("get_chart_image",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Render a candlestick PNG (entry/exit markers + avg lines) for a trade or journal entry so you can read the price action. Cached bars only; returns an error if none are cached."),
		mcpgo.WithNumber("tradeId", mcpgo.Description("Render this trade's chart.")),
		mcpgo.WithNumber("entryId", mcpgo.Description("Render this journal entry's chart (alternative to tradeId).")),
		mcpgo.WithString("contract", mcpgo.Description("Pick a contract/root when several; defaults to the first with data.")),
	), h.mcpGetChartImage)

	s.AddTool(mcpgo.NewTool("create_journal_entry",
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithDescription("Create one journal entry. scope=group takes tradeIds[] (copytrader cluster); scope=trade takes tradeId; scope=day takes date (YYYY-MM-DD); scope=week takes week (YYYY-Www). Put your analysis in body (markdown). Day/week/trade are singletons: if one exists you get {alreadyExists:true, entryId} - then use append_note instead. Group always creates."),
		mcpgo.WithString("scope", mcpgo.Required(), mcpgo.Enum("day", "week", "trade", "group")),
		mcpgo.WithString("body", mcpgo.Description("Markdown body / analysis. Required for day/week/trade; optional for group.")),
		mcpgo.WithString("tag", mcpgo.Enum("good", "neutral", "bad")),
		mcpgo.WithNumber("mood", mcpgo.Description("1-5, day scope only.")),
		mcpgo.WithNumber("strategyId", mcpgo.Description("Assign a strategy (must be owned).")),
		mcpgo.WithString("date", mcpgo.Description("scope=day: YYYY-MM-DD (not future).")),
		mcpgo.WithString("week", mcpgo.Description("scope=week: YYYY-Www.")),
		mcpgo.WithNumber("tradeId", mcpgo.Description("scope=trade.")),
		mcpgo.WithArray("tradeIds", mcpgo.Description("scope=group: trade ids (>=1)."), mcpgo.WithNumberItems()),
		mcpgo.WithArray("mistakes", mcpgo.Description("Mistake labels, fixed keys: "+strings.Join(journal.LabelKeys(journal.KindMistake), ", ")+"."), mcpgo.WithStringItems()),
		mcpgo.WithArray("emotions", mcpgo.Description("Emotion labels, fixed keys: "+strings.Join(journal.LabelKeys(journal.KindEmotion), ", ")+"."), mcpgo.WithStringItems()),
	), h.mcpCreateJournalEntry)

	s.AddTool(mcpgo.NewTool("append_note",
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithDescription("Append a dated note to an existing entry's body. Append-only - never overwrites. Include your analysis marker in the note text so a re-run can detect it."),
		mcpgo.WithNumber("entryId", mcpgo.Required(), mcpgo.Description("Journal entry id.")),
		mcpgo.WithString("note", mcpgo.Required(), mcpgo.Description("Markdown to append.")),
	), h.mcpAppendNote)

	s.AddTool(mcpgo.NewTool("assign_strategy",
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithDescription("Assign a strategy to a journal entry, but ONLY if it has none yet - an existing assignment is never changed (assigned=false then). Call it for ANY entry (newly created or pre-existing) that has no strategy when one of the caller's strategies clearly fits the trade's setup; if nothing clearly fits, skip it - don't force one. Only trade and group entries take a strategy (a day/week spans mixed setups and is rejected)."),
		mcpgo.WithNumber("entryId", mcpgo.Required(), mcpgo.Description("Journal entry id.")),
		mcpgo.WithNumber("strategyId", mcpgo.Required(), mcpgo.Description("Strategy id to assign (must be owned).")),
	), h.mcpAssignStrategy)

	s.AddTool(mcpgo.NewTool("set_tag",
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithDescription("Set an entry's tag (good|neutral|bad), but ONLY if it has none yet - an existing tag is never changed (assigned=false then). Call it for ANY entry (newly created or pre-existing) that is untagged when your analysis warrants a clear verdict; if it's ambiguous, leave it untagged - don't force one."),
		mcpgo.WithNumber("entryId", mcpgo.Required(), mcpgo.Description("Journal entry id.")),
		mcpgo.WithString("tag", mcpgo.Required(), mcpgo.Enum("good", "neutral", "bad")),
	), h.mcpSetTag)

	s.AddTool(mcpgo.NewTool("add_labels",
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithDescription("Add mistake and/or emotion labels to a journal entry from a FIXED vocabulary (unknown keys are rejected). Additive: existing labels stay, duplicates are ignored, nothing is ever removed. Label only what the trades and the trader's own notes clearly show (e.g. an exit before the planned target = early-exit; adding to a loser past the stop = moved-stop); don't guess emotions the notes don't mention. Mistakes: "+strings.Join(journal.LabelKeys(journal.KindMistake), ", ")+". Emotions: "+strings.Join(journal.LabelKeys(journal.KindEmotion), ", ")+"."),
		mcpgo.WithNumber("entryId", mcpgo.Required(), mcpgo.Description("Journal entry id.")),
		mcpgo.WithArray("mistakes", mcpgo.Description("Mistake keys to add."), mcpgo.WithStringItems()),
		mcpgo.WithArray("emotions", mcpgo.Description("Emotion keys to add."), mcpgo.WithStringItems()),
	), h.mcpAddLabels)

	s.AddTool(mcpgo.NewTool("get_coach_checks",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Deterministic coach checks for a trading day: re-entry within a minute of a loss, size up after a loss, heat past the planned stop, a winner that kept under a quarter of its best move, a loser held several times longer than a usual winner, more decisions than the trader's usual day, entries past the usual hour. Each finding carries the numbers, the trade ids and, where one fits, the review label it suggests (add it with add_labels on the entry that covers those trades). Rule-based and objective: cite them in a review as evidence, not as verdicts. prepare_day returns the same checks under 'checks'."),
		mcpgo.WithString("day", mcpgo.Description("Trading day: YYYY-MM-DD or today|yesterday|last; default = the last trading day.")),
	), h.mcpGetCoachChecks)

	s.AddTool(mcpgo.NewTool("get_daily_stats",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("Daily P&L + trade stats per trading day (trades, wins, losses, net, gross, commission, winRate, largestWin/largestLoss) with totals over the range - the same numbers as the journal's Calendar. Use it for overview figures instead of summing trades yourself; for 'how am I doing today' call it with day=\"today\", refresh=true. A trade belongs to the day of its exit (trading tz); the keywords today, yesterday and last (last trading day) are resolved server-side - never compute the date yourself. Retired accounts never count and excluded-from-stats accounts are dropped unless accountId is given. Days without closed trades are omitted."),
		mcpgo.WithString("day", mcpgo.Description("Single trading day: YYYY-MM-DD or today|yesterday|last; sets from=to=day.")),
		mcpgo.WithString("from", mcpgo.Description("Range start YYYY-MM-DD or today|yesterday|last (inclusive, trading tz); default = to minus 29 days.")),
		mcpgo.WithString("to", mcpgo.Description("Range end YYYY-MM-DD or today|yesterday|last (inclusive); default = today. Span at most 400 days.")),
		mcpgo.WithNumber("accountId", mcpgo.Description("Restrict to one account id.")),
		mcpgo.WithBoolean("refresh", mcpgo.Description("Pull the latest fills from the brokers first and wait for them (up to ~15s) before computing. Use for intraday 'today' questions.")),
	), h.mcpGetDailyStats)
	s.AddTool(mcpgo.NewTool("get_analytics_summary",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("The /analytics page as one JSON object for a date range: totals (trades, win rate, net/gross/commission, avg win/loss, payoff ratio, profit factor, expectancy), R (over trades that carry a planned risk), excursion (MAE/MFE off cached bars: exit efficiency, heat on winners, profit losers gave back), drawdown (max peak-to-trough, dates, days to recover, current), streaks (trades and days), and breakdowns by symbol, direction, strategy, tag, mistake label, emotion label, weekday and entry hour. Use it for 'how is my edge / where do I lose' questions instead of summing trade lists. Same date keywords and account rules as get_daily_stats; default range = last 30 days."),
		mcpgo.WithString("day", mcpgo.Description("Single trading day: YYYY-MM-DD or today|yesterday|last; sets from=to=day.")),
		mcpgo.WithString("from", mcpgo.Description("Range start YYYY-MM-DD or today|yesterday|last (inclusive, trading tz); default = to minus 29 days.")),
		mcpgo.WithString("to", mcpgo.Description("Range end YYYY-MM-DD or today|yesterday|last (inclusive); default = today. Span at most 400 days.")),
		mcpgo.WithNumber("accountId", mcpgo.Description("Restrict to one account id (also lifts the excluded-from-stats rule for it).")),
		mcpgo.WithString("unit", mcpgo.Enum("trades", "sets"), mcpgo.Description("What one count means: trades (default), or sets, where a copy set (the same decision fanned across accounts) counts once with its money summed. Use sets for win rate, streaks and 'how many setups' questions on a copytrader book.")),
		mcpgo.WithBoolean("refresh", mcpgo.Description("Pull the latest fills from the brokers first and wait for them (up to ~15s).")),
	), h.mcpGetAnalyticsSummary)
}

// ---- tool handlers (reuse the same ctx-based logic as the REST API) ----

func (h *Handlers) mcpPrepareDay(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.aiJobRunning(ctx) {
		return mcpgo.NewToolResultError(aiBusyMessage), nil
	}
	resp, err := h.prepareDayEntries(ctx, req.GetString("date", ""))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpJSON(resp)
}

func (h *Handlers) mcpServerInfo(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	u := auth.MustUser(ctx)
	return mcpJSON(map[string]any{
		"service":         "trading-journal",
		"instanceLabel":   h.MCPInstanceLabel,
		"authenticatedAs": map[string]any{"id": u.ID, "username": u.Username},
	})
}

func (h *Handlers) mcpListTrades(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	h.syncForAPIRead(ctx, req.GetBool("refresh", false))
	f := tradeFilters{Page: req.GetInt("page", 1)}
	if f.Page < 1 {
		f.Page = 1
	}
	if v := mcpOptString(req, "symbolRoot"); v != nil {
		f.SymbolRoot = v
	}
	if v := req.GetString("direction", ""); v == "long" || v == "short" {
		f.Direction = &v
	}
	if v := req.GetString("outcome", ""); v == "win" || v == "loss" || v == "breakeven" {
		f.Outcome = &v
	}
	if v := req.GetString("risk", ""); v == "planned" || v == "past-stop" {
		f.Risk = &v
	}
	from, to := h.mcpDateRange(ctx, req)
	f.From, f.To = from, to
	if v := mcpOptInt(req, "accountId"); v != nil {
		f.Account = accountFilter{Active: true, AccountIDs: []int64{*v}}
	}
	f.IncludeRetired = req.GetBool("includeRetired", false)

	rows, err := listTradesFiltered(ctx, h.DB, f)
	if err != nil {
		return mcpgo.NewToolResultError("could not list trades"), nil
	}
	total, err := countTradesFiltered(ctx, h.DB, f)
	if err != nil {
		return mcpgo.NewToolResultError("could not count trades"), nil
	}
	items := make([]apiTradeRow, 0, len(rows))
	for _, t := range rows {
		items = append(items, apiTradeRowFrom(t))
	}
	pages := int((total + tradesPageSize - 1) / tradesPageSize)
	if pages < 1 {
		pages = 1
	}
	return mcpJSON(map[string]any{
		"items": items,
		"page":  map[string]any{"page": f.Page, "pageSize": tradesPageSize, "pages": pages, "total": total},
	})
}

func (h *Handlers) mcpGetDailyStats(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	tz := h.localTZ()
	fromStr, toStr := req.GetString("from", ""), req.GetString("to", "")
	if day := req.GetString("day", ""); day != "" {
		fromStr, toStr = day, day
	}
	rng, err := parseAPIDateRange(h.resolveDayKeyword(ctx, fromStr, tz), h.resolveDayKeyword(ctx, toStr, tz), tz, todayLocal(tz))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	var af accountFilter
	if v := mcpOptInt(req, "accountId"); v != nil {
		af = accountFilter{Active: true, AccountIDs: []int64{*v}}
	}
	h.syncForAPIRead(ctx, req.GetBool("refresh", false)) // after validation: a bad date must not nudge
	out, err := h.dailyStats(ctx, af, rng)
	if err != nil {
		return mcpgo.NewToolResultError("could not load daily stats"), nil
	}
	return mcpJSON(out)
}

func (h *Handlers) mcpGetAnalyticsSummary(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	tz := h.localTZ()
	fromStr, toStr := req.GetString("from", ""), req.GetString("to", "")
	if day := req.GetString("day", ""); day != "" {
		fromStr, toStr = day, day
	}
	rng, err := parseAPIDateRange(h.resolveDayKeyword(ctx, fromStr, tz), h.resolveDayKeyword(ctx, toStr, tz), tz, todayLocal(tz))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	var af accountFilter
	if v := mcpOptInt(req, "accountId"); v != nil {
		af = accountFilter{Active: true, AccountIDs: []int64{*v}}
	}
	h.syncForAPIRead(ctx, req.GetBool("refresh", false))
	out, err := h.analyticsSummaryFor(ctx, af, rng, req.GetString("unit", "trades"))
	if err != nil {
		return mcpgo.NewToolResultError("could not load analytics"), nil
	}
	return mcpJSON(out)
}

func (h *Handlers) mcpGetTrade(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id, err := req.RequireInt("id")
	if err != nil {
		return mcpgo.NewToolResultError("id is required"), nil
	}
	d, err := h.tradeDetail(ctx, int64(id))
	if errors.Is(err, sql.ErrNoRows) {
		return mcpgo.NewToolResultError("trade not found"), nil
	}
	if err != nil {
		return mcpgo.NewToolResultError("could not load trade"), nil
	}
	return mcpJSON(d)
}

func (h *Handlers) mcpListJournal(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	tz := h.localTZ()
	f := newJournalFilters()
	if from, to := mcpDateRangeTZ(req, tz); from != nil {
		f.from, f.to = from, to
	}
	if v := req.GetString("mistake", ""); v != "" {
		f.addLabelFilter(journal.KindMistake, v)
	}
	if v := req.GetString("emotion", ""); v != "" {
		f.addLabelFilter(journal.KindEmotion, v)
	}
	if v := req.GetString("scope", ""); v != "" {
		f.scopes = []string{v}
		f.scopeSet[v] = true
	}
	if v := req.GetString("tag", ""); v != "" {
		f.tags = []string{v}
		f.tagSet[v] = true
	}
	limit := req.GetInt("limit", 100)
	if limit < 1 || limit > 500 {
		limit = 100
	}
	items, err := h.journalListItems(ctx, f, 0, limit, tz)
	if err != nil {
		return mcpgo.NewToolResultError("could not list journal"), nil
	}
	return mcpJSON(map[string]any{"items": items})
}

func (h *Handlers) mcpGetJournalEntry(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id, err := req.RequireInt("id")
	if err != nil {
		return mcpgo.NewToolResultError("id is required"), nil
	}
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: int64(id), UserID: userIDPtr(ctx)})
	if errors.Is(err, sql.ErrNoRows) {
		return mcpgo.NewToolResultError("entry not found"), nil
	}
	if err != nil {
		return mcpgo.NewToolResultError("could not load entry"), nil
	}
	resp, err := h.journalDetailJSON(ctx, e, h.localTZ())
	if err != nil {
		return mcpgo.NewToolResultError("could not load entry trades"), nil
	}
	return mcpJSON(resp)
}

func (h *Handlers) mcpListStrategies(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	rows, err := h.Queries.ListActiveStrategies(ctx, userID(ctx))
	if err != nil {
		return mcpgo.NewToolResultError("could not list strategies"), nil
	}
	items := make([]apiStrategy, 0, len(rows))
	for _, s := range rows {
		items = append(items, h.apiStrategyObj(s))
	}
	return mcpJSON(map[string]any{"items": items})
}

func (h *Handlers) mcpGetStrategy(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id, err := req.RequireInt("id")
	if err != nil {
		return mcpgo.NewToolResultError("id is required"), nil
	}
	s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: int64(id), UserID: userID(ctx)})
	if err != nil {
		return mcpgo.NewToolResultError("strategy not found"), nil
	}
	return mcpJSON(h.apiStrategyObj(s))
}

func (h *Handlers) mcpGetChartImage(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.MarketData == nil {
		return mcpgo.NewToolResultError("charting is disabled on the server"), nil
	}
	tz := h.localTZ()
	var trades []linkedTrade
	var label string
	switch {
	case mcpOptInt(req, "tradeId") != nil:
		id := *mcpOptInt(req, "tradeId")
		if _, err := h.Queries.GetTradeByID(ctx, models.GetTradeByIDParams{ID: id, UserID: userID(ctx)}); err != nil {
			return mcpgo.NewToolResultError("trade not found"), nil
		}
		t, err := loadOneTrade(ctx, h.DB, id)
		if err != nil {
			return mcpgo.NewToolResultError("could not load trade"), nil
		}
		trades, label = t, fmt.Sprintf("trade %d", id)
	case mcpOptInt(req, "entryId") != nil:
		id := *mcpOptInt(req, "entryId")
		e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(ctx)})
		if err != nil {
			return mcpgo.NewToolResultError("entry not found"), nil
		}
		t, err := h.apiTradesForEntry(ctx, e, tz)
		if err != nil {
			return mcpgo.NewToolResultError("could not load entry trades"), nil
		}
		trades, label = t, fmt.Sprintf("entry %d", id)
	default:
		return mcpgo.NewToolResultError("pass tradeId or entryId"), nil
	}
	env := h.buildChartEnvelope(ctx, trades, false)
	png, err := h.chartEnvelopePNG(env, req.GetString("contract", ""), tz)
	if errors.Is(err, errNoChartData) {
		return mcpgo.NewToolResultError("no cached chart data for " + label + "; warm bars by opening the chart in the UI first"), nil
	}
	if err != nil {
		return mcpgo.NewToolResultError("could not render chart"), nil
	}
	return mcpgo.NewToolResultImage("Chart for "+label, base64.StdEncoding.EncodeToString(png), "image/png"), nil
}

func (h *Handlers) mcpCreateJournalEntry(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.aiJobRunning(ctx) {
		return mcpgo.NewToolResultError(aiBusyMessage), nil
	}
	in := apiCreateInput{
		Scope:      req.GetString("scope", ""),
		Body:       req.GetString("body", ""),
		Tag:        mcpOptString(req, "tag"),
		Mood:       mcpOptInt(req, "mood"),
		StrategyID: mcpOptInt(req, "strategyId"),
		Date:       req.GetString("date", ""),
		Week:       req.GetString("week", ""),
		TradeID:    mcpOptInt(req, "tradeId"),
		TradeIDs:   mcpInt64Slice(req, "tradeIds"),
		Mistakes:   mcpStringSlice(req, "mistakes"),
		Emotions:   mcpStringSlice(req, "emotions"),
	}
	e, conflict, err := h.createJournalEntry(ctx, in)
	if conflict != nil {
		return mcpJSON(map[string]any{"alreadyExists": true, "entryId": conflict.EntryID, "message": conflict.Message})
	}
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	resp, err := h.journalDetailJSON(ctx, e, h.localTZ())
	if err != nil {
		return mcpJSON(map[string]any{"id": e.ID, "scope": e.Scope})
	}
	return mcpJSON(resp)
}

func (h *Handlers) mcpAssignStrategy(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.aiJobRunning(ctx) {
		return mcpgo.NewToolResultError(aiBusyMessage), nil
	}
	eid, err := req.RequireInt("entryId")
	if err != nil {
		return mcpgo.NewToolResultError("entryId is required"), nil
	}
	sid, err := req.RequireInt("strategyId")
	if err != nil {
		return mcpgo.NewToolResultError("strategyId is required"), nil
	}
	assigned, err := h.assignStrategyIfUnset(ctx, int64(eid), int64(sid))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpJSON(map[string]any{"entryId": eid, "strategyId": sid, "assigned": assigned})
}

func (h *Handlers) mcpSetTag(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.aiJobRunning(ctx) {
		return mcpgo.NewToolResultError(aiBusyMessage), nil
	}
	eid, err := req.RequireInt("entryId")
	if err != nil {
		return mcpgo.NewToolResultError("entryId is required"), nil
	}
	tag, err := req.RequireString("tag")
	if err != nil {
		return mcpgo.NewToolResultError("tag is required"), nil
	}
	assigned, err := h.assignTagIfUnset(ctx, int64(eid), tag)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpJSON(map[string]any{"entryId": eid, "tag": tag, "assigned": assigned})
}

func (h *Handlers) mcpAddLabels(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.aiJobRunning(ctx) {
		return mcpgo.NewToolResultError(aiBusyMessage), nil
	}
	eid, err := req.RequireInt("entryId")
	if err != nil {
		return mcpgo.NewToolResultError("entryId is required"), nil
	}
	mistakes, emotions := mcpStringSlice(req, "mistakes"), mcpStringSlice(req, "emotions")
	if len(mistakes) == 0 && len(emotions) == 0 {
		return mcpgo.NewToolResultError("nothing to add: pass mistakes and/or emotions"), nil
	}
	labels, err := h.addLabels(ctx, int64(eid), mistakes, emotions)
	if errors.Is(err, journal.ErrNotFound) {
		return mcpgo.NewToolResultError("entry not found"), nil
	}
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return mcpJSON(map[string]any{"entryId": eid, "labels": labelsOut(labels)})
}

func (h *Handlers) mcpAppendNote(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if h.aiJobRunning(ctx) {
		return mcpgo.NewToolResultError(aiBusyMessage), nil
	}
	id, err := req.RequireInt("entryId")
	if err != nil {
		return mcpgo.NewToolResultError("entryId is required"), nil
	}
	note := strings.TrimSpace(req.GetString("note", ""))
	if note == "" {
		return mcpgo.NewToolResultError("note is empty"), nil
	}
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: int64(id), UserID: userIDPtr(ctx)})
	if errors.Is(err, sql.ErrNoRows) {
		return mcpgo.NewToolResultError("entry not found"), nil
	}
	if err != nil {
		return mcpgo.NewToolResultError("could not load entry"), nil
	}
	if err := h.appendOrSeedJournalNote(ctx, e, note); err != nil {
		return mcpgo.NewToolResultError("could not append note"), nil
	}
	return mcpJSON(map[string]any{"id": e.ID, "appended": true})
}

// ---- small arg helpers (MCP args arrive as JSON: numbers are float64) ----

func mcpJSON(v any) (*mcpgo.CallToolResult, error) {
	res, err := mcpgo.NewToolResultJSON(v)
	if err != nil {
		return mcpgo.NewToolResultError("could not encode result"), nil
	}
	return res, nil
}

func mcpOptString(req mcpgo.CallToolRequest, key string) *string {
	v, ok := req.GetArguments()[key]
	if !ok {
		return nil
	}
	if s, ok := v.(string); ok {
		if s = strings.TrimSpace(s); s != "" {
			return &s
		}
	}
	return nil
}

func mcpOptInt(req mcpgo.CallToolRequest, key string) *int64 {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return nil
	}
	if n, ok := toInt64(v); ok {
		return &n
	}
	return nil
}

func mcpInt64Slice(req mcpgo.CallToolRequest, key string) []int64 {
	raw, ok := req.GetArguments()[key].([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, x := range raw {
		if n, ok := toInt64(x); ok {
			out = append(out, n)
		}
	}
	return out
}

// mcpStringSlice reads an array-of-strings argument; non-string items and
// blanks are dropped (the service validates the keys that remain).
func mcpStringSlice(req mcpgo.CallToolRequest, key string) []string {
	raw, ok := req.GetArguments()[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		if str, ok := x.(string); ok {
			if str = strings.TrimSpace(str); str != "" {
				out = append(out, str)
			}
		}
	}
	return out
}

// toInt64 coerces a JSON-decoded tool argument to int64. MCP args usually arrive
// as float64, but be tolerant of clients that encode numbers as json.Number, a
// plain int, or a numeric string, so e.g. an array of trade ids can't silently
// come back empty (which previously read as "no owned trades").
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// mcpDateRange parses day/from/to (YYYY-MM-DD, UTC) into a trade-filter window
// (to is exclusive +24h), mirroring parseTradeFilters.
// mcpDateRange is the trades-filter variant: day/from/to (keywords resolved)
// are trading-tz calendar days, converted to UTC instants because
// trades.exit_time is stored in UTC and compared as text - so "today" means
// the trading day, not the UTC day, and matches get_daily_stats.
func (h *Handlers) mcpDateRange(ctx context.Context, req mcpgo.CallToolRequest) (from, to *time.Time) {
	tz := h.localTZ()
	fromStr := req.GetString("from", "")
	toStr := req.GetString("to", "")
	if day := req.GetString("day", ""); day != "" {
		fromStr, toStr = day, day
	}
	if t, err := time.ParseInLocation("2006-01-02", h.resolveDayKeyword(ctx, fromStr, tz), tz); err == nil {
		u := t.UTC()
		from = &u
	}
	if t, err := time.ParseInLocation("2006-01-02", h.resolveDayKeyword(ctx, toStr, tz), tz); err == nil {
		u := t.AddDate(0, 0, 1).UTC() // inclusive upper bound
		to = &u
	}
	return from, to
}

// mcpDateRangeTZ is the journal-filter variant: dates are parsed in the trading
// tz and `to` is the inclusive day +24h, mirroring parseJournalFilters.
func mcpDateRangeTZ(req mcpgo.CallToolRequest, tz *time.Location) (from, to *time.Time) {
	fromStr := req.GetString("from", "")
	toStr := req.GetString("to", "")
	if day := req.GetString("day", ""); day != "" {
		fromStr, toStr = day, day
	}
	if t, err := time.ParseInLocation("2006-01-02", fromStr, tz); err == nil {
		from = &t
	}
	if t, err := time.ParseInLocation("2006-01-02", toStr, tz); err == nil {
		t = t.AddDate(0, 0, 1)
		to = &t
	}
	return from, to
}
