package handlers

import (
	"net/http"

	"github.com/w4jnl/trading-journal/internal/aimethod"
)

// APIAIMethod: GET /api/v1/ai/method - the review method this journal works
// by, for agents and scripts that are not MCP clients: the version, the
// markers, the user's review language, the document as markdown, the label
// vocabulary and the modes with their steps.
func (h *Handlers) APIAIMethod(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lang := h.userAILanguage(ctx)
	modes := make([]map[string]any, 0, len(aimethod.Modes))
	for _, m := range aimethod.Modes {
		args := make([]map[string]any, 0, len(m.Args))
		for _, a := range m.Args {
			args = append(args, map[string]any{"name": a.Name, "description": a.Description, "required": a.Required})
		}
		modes = append(modes, map[string]any{"name": m.Name, "title": m.Title, "purpose": m.Purpose, "arguments": args, "steps": m.Steps})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        aimethod.Version,
		"marker":         aimethod.MarkerLine(),
		"markers":        map[string]string{"app": aimethod.Markers.Local, "agent": aimethod.Markers.MCP},
		"language":       lang,
		"methodMarkdown": aimethod.Document(aimethod.DocOptions{Tools: mcpToolNames, Language: lang}),
		"labels":         aimethod.LabelVocabulary(),
		"modes":          modes,
	})
}
