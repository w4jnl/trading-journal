package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/w4jnl/trading-journal/internal/aimethod"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- The method over MCP ---------------------------------------------------
//
// The server carries the review method (internal/aimethod) to every client:
// the instructions at initialize, the document and the trader's evidence as
// resources, the modes as prompts, and two tools for clients that read
// neither instructions nor resources (ChatGPT): get_review_method and
// list_lessons. The client skill only points here.

const (
	mcpResMethod    = "trading-journal://method"
	mcpResLabels    = "trading-journal://method/labels"
	mcpResLessons   = "trading-journal://lessons"
	mcpResPlaybooks = "trading-journal://playbooks"
)

// mcpToolNames are the tools registered on the last server built, in order.
var mcpToolNames []string

func addTool(s *mcpserver.MCPServer, t mcpgo.Tool, fn mcpserver.ToolHandlerFunc) {
	mcpToolNames = append(mcpToolNames, t.Name)
	s.AddTool(t, fn)
}

// registerMCPMethod adds the resources, the prompts and the method tools.
func (h *Handlers) registerMCPMethod(s *mcpserver.MCPServer) {
	s.AddResource(mcpgo.NewResource(mcpResMethod, "The review method",
		mcpgo.WithResourceDescription("How to review a trade, a group, a day or a week and how to plan a session: the briefs, the lines every review ends with, the write rules, the modes and the evidence, in the trader's language."),
		mcpgo.WithMIMEType("text/markdown")), h.mcpMethodResource)
	s.AddResource(mcpgo.NewResource(mcpResLabels, "The label vocabulary",
		mcpgo.WithResourceDescription("The mistakes and emotions a review may name, each with the evidence its description asks for."),
		mcpgo.WithMIMEType("application/json")), h.mcpLabelsResource)
	s.AddResource(mcpgo.NewResource(mcpResLessons, "The trader's lessons",
		mcpgo.WithResourceDescription("The open lessons of the last thirty days grouped by mistake, most repeated first, and every lesson of the last ninety days."),
		mcpgo.WithMIMEType("application/json")), h.mcpLessonsResource)
	s.AddResource(mcpgo.NewResource(mcpResPlaybooks, "The trader's playbooks",
		mcpgo.WithResourceDescription("Every active strategy with its full playbook."),
		mcpgo.WithMIMEType("application/json")), h.mcpPlaybooksResource)
	for _, m := range aimethod.Modes {
		opts := []mcpgo.PromptOption{mcpgo.WithPromptDescription(m.Purpose)}
		for _, a := range m.Args {
			ao := []mcpgo.ArgumentOption{mcpgo.ArgumentDescription(a.Description)}
			if a.Required {
				ao = append(ao, mcpgo.RequiredArgument())
			}
			opts = append(opts, mcpgo.WithArgument(a.Name, ao...))
		}
		mode := m
		s.AddPrompt(mcpgo.NewPrompt(m.Name, opts...), func(ctx context.Context, req mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
			text := aimethod.PromptText(mode, req.Params.Arguments, h.mcpLanguage(ctx))
			return mcpgo.NewGetPromptResult(mode.Purpose, []mcpgo.PromptMessage{mcpgo.NewPromptMessage(mcpgo.RoleUser, mcpgo.NewTextContent(text))}), nil
		})
	}
	addTool(s, mcpgo.NewTool("get_review_method",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("The review method this journal works by, as markdown: how to review a trade, a group, a day or a week, how to plan a session, the lines every review must end with (the lesson line and the marker), the write rules and the modes with their tool order. Call it at the start of a session and follow it; it carries a version, and the trader's review language."),
	), h.mcpGetReviewMethod)
	addTool(s, mcpgo.NewTool("list_lessons",
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDescription("The trader's lessons ledger: what their reviews concluded, newest first. open=true keeps the open lessons of the last thirty days grouped by mistake (most repeated first); from/to bound the days otherwise (default the last ninety days). Read it before a review so a lesson that recurs is named as recurring, not reworded."),
		mcpgo.WithString("from", mcpgo.Description("First day YYYY-MM-DD (inclusive).")),
		mcpgo.WithString("to", mcpgo.Description("Last day YYYY-MM-DD (inclusive).")),
		mcpgo.WithBoolean("open", mcpgo.Description("Only the open lessons of the last thirty days, grouped by mistake.")),
	), h.mcpListLessons)
}

// mcpLanguage is the calling user's review language ("" by default).
func (h *Handlers) mcpLanguage(ctx context.Context) string {
	if h.Queries == nil {
		return ""
	}
	return h.userAILanguage(ctx)
}

func (h *Handlers) mcpMethodResource(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	doc := aimethod.Document(aimethod.DocOptions{Tools: mcpToolNames, Language: h.mcpLanguage(ctx)})
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "text/markdown", Text: doc}}, nil
}

func (h *Handlers) mcpLabelsResource(_ context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	b, _ := json.MarshalIndent(aimethod.LabelVocabulary(), "", "  ")
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}}, nil
}

func (h *Handlers) mcpLessonsResource(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	b, _ := json.MarshalIndent(h.lessonsLedger(ctx, "", "", false), "", "  ")
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}}, nil
}

func (h *Handlers) mcpPlaybooksResource(ctx context.Context, req mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	var list []map[string]any
	if h.Queries != nil {
		if strats, err := h.Queries.ListActiveStrategies(ctx, userID(ctx)); err == nil {
			for _, s := range strats {
				list = append(list, map[string]any{"id": s.ID, "name": s.Name, "playbook": s.Body})
			}
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"strategies": list}, "", "  ")
	return []mcpgo.ResourceContents{mcpgo.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}}, nil
}

// lessonsLedger is the lessons as the resource and the tool answer them.
func (h *Handlers) lessonsLedger(ctx context.Context, from, to string, openOnly bool) map[string]any {
	out := map[string]any{"asOf": time.Now().UTC().Format(time.RFC3339)}
	if h.Queries == nil {
		return out
	}
	now := time.Now().In(h.localTZ())
	since := now.AddDate(0, 0, -openLessonDays).Format("2006-01-02")
	out["open"] = h.openLessons(ctx, since)
	if openOnly {
		return out
	}
	if from == "" {
		from = now.AddDate(0, 0, -90).Format("2006-01-02")
	}
	if to == "" {
		to = now.Format("2006-01-02")
	}
	rows, err := h.Queries.ListLessonsBetween(ctx, models.ListLessonsBetweenParams{UserID: userID(ctx), Day: from, Day_2: to})
	if err != nil {
		return out
	}
	list := make([]map[string]any, 0, len(rows))
	for _, l := range rows {
		m := map[string]any{"id": l.ID, "day": l.Day, "scope": l.Scope, "lesson": l.Text}
		if l.EntryID != nil {
			m["entryId"] = *l.EntryID
		}
		if l.MistakeKey != nil {
			m["mistake"] = *l.MistakeKey
		}
		if l.LearnedAt != nil {
			m["learned"] = true
		}
		list = append(list, m)
	}
	out["from"], out["to"], out["lessons"] = from, to, list
	return out
}

func (h *Handlers) mcpGetReviewMethod(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpJSON(map[string]any{
		"version":  aimethod.Version,
		"marker":   aimethod.MarkerLine(),
		"language": h.mcpLanguage(ctx),
		"method":   aimethod.Document(aimethod.DocOptions{Tools: mcpToolNames, Language: h.mcpLanguage(ctx)}),
	})
}

func (h *Handlers) mcpListLessons(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	from, to := req.GetString("from", ""), req.GetString("to", "")
	for _, d := range []string{from, to} {
		if d != "" {
			if _, err := time.Parse("2006-01-02", d); err != nil {
				return mcpgo.NewToolResultError("from and to must be YYYY-MM-DD"), nil
			}
		}
	}
	return mcpJSON(h.lessonsLedger(ctx, from, to, req.GetBool("open", false)))
}

// mcpServerFacts is what server_info says beyond the identity: the server,
// the trader's language, the method and where to read it.
func (h *Handlers) mcpServerFacts(ctx context.Context) map[string]any {
	facts := map[string]any{
		"version":        h.Version,
		"timezone":       h.localTZ().String(),
		"language":       h.mcpLanguage(ctx),
		"marker":         aimethod.MarkerLine(),
		"methodVersion":  aimethod.Version,
		"methodResource": mcpResMethod,
		"methodTool":     "get_review_method",
		"tools":          len(mcpToolNames),
		"prompts":        promptNames(),
	}
	if h.Queries != nil {
		facts["features"] = map[string]bool{"ai": h.aiPossible(), "charts": h.MarketData != nil, "guardrail": h.Guardrail != nil, "push": h.Push.Enabled()}
	}
	return facts
}

func promptNames() []string {
	names := make([]string, 0, len(aimethod.Modes))
	for _, m := range aimethod.Modes {
		names = append(names, m.Name)
	}
	return names
}

// mcpNoteLesson takes the lesson line off a note an agent appends and
// records it, so external reviews feed the ledger like the in-app ones.
func (h *Handlers) mcpNoteLesson(ctx context.Context, e models.JournalEntry, note string) string {
	l := parseAILesson(note)
	if l.Text == "" {
		return note
	}
	h.recordLesson(ctx, e, l)
	return strings.TrimSpace(stripAILesson(note))
}
