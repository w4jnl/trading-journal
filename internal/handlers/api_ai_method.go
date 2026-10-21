package handlers

import (
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/aimethod"
	"github.com/w4jnl/trading-journal/internal/models"
)

// APIAIMethod: GET /api/v1/ai/method - the review method this journal works
// by, for agents and scripts that are not MCP clients: the version, the
// markers, the user's review language and profile, the document as markdown
// (with the profile), the label vocabulary and the modes with their steps.
func (h *Handlers) APIAIMethod(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lang, profile := h.userAILanguage(ctx), h.userAIProfile(ctx)
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
		"profile":        profile,
		"methodMarkdown": aimethod.Document(aimethod.DocOptions{Tools: mcpToolNames, Language: lang, Profile: profile.Block()}),
		"labels":         aimethod.LabelVocabulary(),
		"modes":          modes,
	})
}

// APIAIPreview: GET /api/v1/ai/preview/{id} - the request a review of the
// entry would send right now, block by block and without calling a model:
// the system turn and the user turn, which blocks are cached, how big each
// is (characters and an estimate of tokens at four characters each), and
// whether a chart image would ride along. ?lang= previews another language.
// For checking what the model is told; nothing is written.
func (h *Handlers) APIAIPreview(w http.ResponseWriter, r *http.Request) {
	ctx := withAILanguage(r.Context(), r.URL.Query().Get("lang"))
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: id, UserID: userIDPtr(ctx)})
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "entry not found")
		return
	}
	req, err := h.aiReviewRequest(ctx, e)
	if err != nil {
		h.logger().Warn("ai preview", "entry", id, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not build the request")
		return
	}
	system, user := previewBlocks(req.System), previewBlocks(req.User)
	var chars, cached int
	for _, b := range append(append([]ai.Block{}, req.System...), req.User...) {
		chars += len(b.Text)
		if b.Cache {
			cached += len(b.Text)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entryId": id, "scope": e.Scope, "method": aimethod.Version, "language": h.userAILanguage(ctx),
		"images": len(req.Images), "system": system, "user": user,
		"totals": map[string]any{"chars": chars, "tokensEstimate": tokensEstimate(chars), "cachedTokensEstimate": tokensEstimate(cached)},
	})
}

// tokensEstimate is the usual four characters per token; providers count
// their own way, this is for comparing blocks.
func tokensEstimate(chars int) int { return (chars + 3) / 4 }

func previewBlocks(bs []ai.Block) []map[string]any {
	out := make([]map[string]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, map[string]any{"cached": b.Cache, "chars": len(b.Text), "tokensEstimate": tokensEstimate(len(b.Text)), "text": b.Text})
	}
	return out
}
