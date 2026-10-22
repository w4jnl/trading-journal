package handlers

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// TestMCPToolsList stands the MCP server up over httptest and drives it with the
// real mcp-go client (initialize -> tools/list), asserting the full tool set is
// registered. Listing tools doesn't invoke handlers, so no DB is needed; this
// guards registration + the streamable-HTTP handshake. Tool execution against
// the real schema is covered by the DB-clone MCP smoke.
func TestMCPToolsList(t *testing.T) {
	h := &Handlers{}
	srv := httptest.NewServer(h.MCPHandler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := mcpclient.NewStreamableHttpClient(srv.URL, mcptransport.WithHTTPHeaders(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	init, err := c.Initialize(ctx, mcpgo.InitializeRequest{})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// The method reaches a client that brings no skill: instructions at
	// initialize, the modes as prompts, the method and the evidence as
	// resources.
	if !strings.Contains(init.Instructions, "tj-ai-mcp:v1 method=") || !strings.Contains(init.Instructions, "get_review_method") {
		t.Errorf("instructions = %.200q", init.Instructions)
	}
	prompts, err := c.ListPrompts(ctx, mcpgo.ListPromptsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	gotPrompts := map[string]bool{}
	for _, p := range prompts.Prompts {
		gotPrompts[p.Name] = true
	}
	for _, name := range []string{"review_day", "review_entry", "today", "review_week", "plan_day"} {
		if !gotPrompts[name] {
			t.Errorf("prompt %s missing (got %v)", name, gotPrompts)
		}
	}
	pr, err := c.GetPrompt(ctx, mcpgo.GetPromptRequest{Params: mcpgo.GetPromptParams{Name: "review_day", Arguments: map[string]string{"date": "2026-10-09"}}})
	if err != nil || len(pr.Messages) != 1 {
		t.Fatalf("review_day prompt = %+v %v", pr, err)
	}
	if text := pr.Messages[0].Content.(mcpgo.TextContent).Text; !strings.Contains(text, "date = 2026-10-09") || !strings.Contains(text, "prepare_day") || !strings.Contains(text, "tj-ai-mcp:v1 method=") {
		t.Errorf("review_day text = %.300q", text)
	}
	// The language argument asks for one answer in another language.
	pr, err = c.GetPrompt(ctx, mcpgo.GetPromptRequest{Params: mcpgo.GetPromptParams{Name: "today", Arguments: map[string]string{"language": "Czech"}}})
	if err != nil || len(pr.Messages) != 1 {
		t.Fatalf("today prompt = %+v %v", pr, err)
	}
	if text := pr.Messages[0].Content.(mcpgo.TextContent).Text; !strings.Contains(text, "Write the prose in Czech") || strings.Contains(text, "language = ") {
		t.Errorf("today text = %.300q", text)
	}
	resources, err := c.ListResources(ctx, mcpgo.ListResourcesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	gotRes := map[string]bool{}
	for _, r := range resources.Resources {
		gotRes[r.URI] = true
	}
	for _, uri := range []string{"trading-journal://method", "trading-journal://method/labels", "trading-journal://lessons", "trading-journal://playbooks", "trading-journal://profile"} {
		if !gotRes[uri] {
			t.Errorf("resource %s missing", uri)
		}
	}
	rr, err := c.ReadResource(ctx, mcpgo.ReadResourceRequest{Params: mcpgo.ReadResourceParams{URI: "trading-journal://method"}})
	if err != nil || len(rr.Contents) != 1 {
		t.Fatalf("method resource = %+v %v", rr, err)
	}
	doc := rr.Contents[0].(mcpgo.TextResourceContents).Text
	for _, tool := range mcpToolNames {
		if !strings.Contains(doc, "`"+tool+"`") {
			t.Errorf("the method document does not name the tool %s", tool)
		}
	}
	if !strings.Contains(doc, "This list is live") {
		t.Error("the method document does not say how to notice a stale tool list")
	}
	// server_info is live: it names every tool and every image type, so an
	// agent holding yesterday's schema can tell (the facts need a user and a
	// database this server has not; the helpers behind them are checked here).
	if names := sortedToolNames(); len(names) != len(mcpToolNames) || !sort.StringsAreSorted(names) || !strings.Contains(mcpSchemaNote, "reconnect") {
		t.Errorf("toolNames = %v", names)
	}
	if types := attachmentImageTypes(); strings.Join(types, " ") != "image/gif image/jpeg image/png image/svg+xml image/webp" {
		t.Errorf("imageTypes = %v", types)
	}
	if !strings.Contains(doc, "## Modes") || !strings.Contains(doc, "THIS ITEM IS ONE TRADE") {
		t.Error("the method document lacks the modes or the briefs")
	}
	res, err := c.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	want := []string{
		"prepare_day", "server_info",
		"list_trades", "get_trade", "list_journal", "get_journal_entry",
		"list_strategies", "get_strategy", "create_strategy", "update_strategy", "get_chart_image",
		"create_journal_entry", "append_note", "assign_strategy", "set_tag", "add_labels",
		"get_daily_stats", "get_analytics_summary", "get_coach_checks", "get_day_plan", "set_day_plan", "get_weekly_digest",
		"list_positions", "set_position_plan", "get_levels", "get_review_method", "list_lessons",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("tools/list missing %q (got %v)", w, got)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("tool count = %d, want %d", len(res.Tools), len(want))
	}

	// Annotations drive the Claude app's connector grouping: reads must be
	// readOnly (so they can be always-allowed), writes must declare they are
	// non-destructive (additive / fill-a-blank only), except the one that
	// replaces the trader's own text, which must say so. An un-annotated
	// tool is filed under "Write/delete tools" by the client.
	readOnly := map[string]bool{
		"server_info": true, "list_trades": true, "get_trade": true, "list_journal": true,
		"get_journal_entry": true, "list_strategies": true, "get_strategy": true,
		"get_chart_image": true, "get_daily_stats": true, "get_analytics_summary": true, "get_coach_checks": true, "get_day_plan": true, "get_weekly_digest": true,
		"list_positions": true, "get_levels": true, "get_review_method": true, "list_lessons": true,
	}
	overwrites := map[string]bool{"update_strategy": true}
	for _, tl := range res.Tools {
		ro := tl.Annotations.ReadOnlyHint != nil && *tl.Annotations.ReadOnlyHint
		if ro != readOnly[tl.Name] {
			t.Errorf("%s readOnlyHint = %v, want %v", tl.Name, ro, readOnly[tl.Name])
		}
		if readOnly[tl.Name] {
			continue
		}
		if tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint != overwrites[tl.Name] {
			t.Errorf("%s must declare destructiveHint=%v", tl.Name, overwrites[tl.Name])
		}
	}
}
