package handlers

import (
	"context"
	"net/http/httptest"
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
	if _, err := c.Initialize(ctx, mcpgo.InitializeRequest{}); err != nil {
		t.Fatalf("initialize: %v", err)
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
		"list_strategies", "get_strategy", "get_chart_image",
		"create_journal_entry", "append_note", "assign_strategy", "set_tag", "add_labels",
		"get_daily_stats", "get_analytics_summary", "get_coach_checks", "get_day_plan", "set_day_plan", "get_weekly_digest",
		"list_positions", "set_position_plan", "get_levels",
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
	// non-destructive (additive / fill-a-blank only). An un-annotated tool
	// is filed under "Write/delete tools" by the client.
	readOnly := map[string]bool{
		"server_info": true, "list_trades": true, "get_trade": true, "list_journal": true,
		"get_journal_entry": true, "list_strategies": true, "get_strategy": true,
		"get_chart_image": true, "get_daily_stats": true, "get_analytics_summary": true, "get_coach_checks": true, "get_day_plan": true, "get_weekly_digest": true,
		"list_positions": true, "get_levels": true,
	}
	for _, tl := range res.Tools {
		ro := tl.Annotations.ReadOnlyHint != nil && *tl.Annotations.ReadOnlyHint
		if ro != readOnly[tl.Name] {
			t.Errorf("%s readOnlyHint = %v, want %v", tl.Name, ro, readOnly[tl.Name])
		}
		if !readOnly[tl.Name] && (tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint) {
			t.Errorf("%s must declare destructiveHint=false", tl.Name)
		}
	}
}
