package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The AI review's suggestions on a note, for the phone: the open ones ride
// on GET /journal/{id} as "suggestions", and a device key accepts or
// dismisses one on POST /journal/{id}/suggestions, the way the page does.

type apiSuggestion struct {
	Kind  string `json:"kind"`  // tag | strategy | mistake | emotion
	Value string `json:"value"` // the tag word, the strategy id, the label key
	Name  string `json:"name"`  // what to show: the strategy's or the label's name
}

// apiSuggestions lists the entry's open suggestions with display names.
func (h *Handlers) apiSuggestions(ctx context.Context, entryID int64) []apiSuggestion {
	open, err := h.journalSvc().Suggestions(ctx, userID(ctx), entryID)
	if err != nil {
		return []apiSuggestion{}
	}
	out := make([]apiSuggestion, 0, len(open))
	for _, s := range open {
		name := s.Value
		switch s.Kind {
		case journal.SuggestStrategy:
			if id, err := strconv.ParseInt(s.Value, 10, 64); err == nil {
				if st, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID(ctx)}); err == nil {
					name = st.Name
				}
			}
		case journal.SuggestTag:
		default:
			name = journal.LabelName(s.Kind, s.Value)
		}
		out = append(out, apiSuggestion{Kind: s.Kind, Value: s.Value, Name: name})
	}
	return out
}

// APIJournalSuggestion: POST /api/v1/journal/{id}/suggestions (device key)
// {"kind","value","do":"accept"|"dismiss"} - accept makes the suggestion the
// field, as the trader's own; dismiss keeps it dismissed. Answers with the
// entry as GET /journal/{id} returns it.
func (h *Handlers) APIJournalSuggestion(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	ctx := r.Context()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	var in struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Do    string `json:"do"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	svc := h.journalSvc()
	var err error
	switch in.Do {
	case "accept":
		err = svc.AcceptSuggestion(ctx, userID(ctx), id, in.Kind, in.Value)
	case "dismiss":
		err = svc.DismissSuggestion(ctx, userID(ctx), id, in.Kind, in.Value)
	default:
		writeJSONError(w, http.StatusBadRequest, "do must be accept or dismiss")
		return
	}
	var ve apiValidationError
	switch {
	case errors.Is(err, journal.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "no such open suggestion")
		return
	case errors.Is(err, journal.ErrReviewPending):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "reason": "review-pending"})
		return
	case errors.As(err, &ve):
		writeJSONError(w, http.StatusBadRequest, ve.Msg)
		return
	case err != nil:
		h.logger().Error("api suggestion", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not apply the suggestion")
		return
	}
	h.LiveData.Bump(userID(ctx))
	e, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	resp, err := h.journalDetailJSON(ctx, e, h.localTZ())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "applied, but could not render the entry")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
