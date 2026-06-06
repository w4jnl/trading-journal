package handlers

import (
	"net/http"

	"github.com/w4jnl/trading-journal/internal/models"
)

// apiStrategyObj projects a strategy with its markdown body's image refs
// rewritten to API URLs and the referenced attachments enumerated.
func (h *Handlers) apiStrategyObj(s models.Strategy) apiStrategy {
	body, ids := apiRewriteAttachmentRefs(s.Body)
	return apiStrategy{
		ID:          s.ID,
		Name:        s.Name,
		Body:        body,
		Attachments: apiAttachmentsFrom(ids),
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

// APIStrategiesList: GET /api/v1/strategies
func (h *Handlers) APIStrategiesList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.Queries.ListStrategies(ctx, userID(ctx))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list strategies")
		return
	}
	items := make([]apiStrategy, 0, len(rows))
	for _, s := range rows {
		items = append(items, h.apiStrategyObj(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// APIStrategyDetail: GET /api/v1/strategies/{id}
func (h *Handlers) APIStrategyDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	s, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "strategy not found")
		return
	}
	writeJSON(w, http.StatusOK, h.apiStrategyObj(s))
}
