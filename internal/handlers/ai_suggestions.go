package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- AI suggestions on a note, and who set what ------------------------------
//
// An in-app AI review proposes a tag, a strategy and labels instead of writing
// them (journal.Suggest); the note shows each as a dashed pill to accept or
// dismiss. Values written through the API or MCP carry source 'api' and show
// marked until the trader saves the note in its editor.

// suggestionView is one open suggestion as a pill: Label is what it reads.
type suggestionView struct {
	Kind, Value, Label string
}

// entrySuggestions loads the open suggestions of the caller's entry as pills.
func (h *Handlers) entrySuggestions(ctx context.Context, entryID int64) []suggestionView {
	list, err := h.journalSvc().Suggestions(ctx, userID(ctx), entryID)
	if err != nil {
		h.logger().Warn("load suggestions", "entry", entryID, "err", err)
		return nil
	}
	out := make([]suggestionView, 0, len(list))
	for _, sg := range list {
		v := suggestionView{Kind: sg.Kind, Value: sg.Value}
		switch sg.Kind {
		case journal.SuggestTag:
			v.Label = "Tag: " + sg.Value
		case journal.SuggestStrategy:
			id, _ := strconv.ParseInt(sg.Value, 10, 64)
			st, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID(ctx)})
			if err != nil {
				continue
			}
			v.Label = "Strategy: " + st.Name
		default:
			v.Label = journal.LabelName(sg.Kind, sg.Value)
		}
		out = append(out, v)
	}
	return out
}

// labelSources maps the caller's entry's labels ("kind:key") to who set them.
func (h *Handlers) labelSources(ctx context.Context, entryID int64) map[string]string {
	return h.journalSvc().EntryLabelSources(ctx, userID(ctx), entryID)
}

// sourceOf reads a nullable source column ("" = the trader, or legacy).
func sourceOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// JournalSuggestion: POST /journal/suggestions/{id} (htmx) - accept or dismiss
// one open suggestion on entry {id}. Form: kind, value, do=accept|dismiss.
// then=trade:N or then=set:N re-renders that sheet in place; anything else
// reloads the page.
func (h *Handlers) JournalSuggestion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	kind, value := r.PostFormValue("kind"), r.PostFormValue("value")
	svc := h.journalSvc()
	switch r.PostFormValue("do") {
	case "accept":
		err = svc.AcceptSuggestion(ctx, userID(ctx), id, kind, value)
	case "dismiss":
		err = svc.DismissSuggestion(ctx, userID(ctx), id, kind, value)
	default:
		http.Error(w, "do must be accept or dismiss", http.StatusBadRequest)
		return
	}
	var ve apiValidationError
	switch {
	case errors.Is(err, journal.ErrNotFound):
		http.Error(w, "suggestion not found", http.StatusNotFound)
		return
	case errors.Is(err, journal.ErrReviewPending):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case errors.As(err, &ve):
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	case err != nil:
		h.serverError(w, err)
		return
	}
	then, ref, _ := strings.Cut(r.URL.Query().Get("then"), ":")
	switch then {
	case "trade":
		r.SetPathValue("id", ref)
		h.renderTradeDetail(w, r, "accepted")
	case "set":
		r.SetPathValue("id", ref)
		h.renderSetDetail(w, r, "")
	default:
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	}
}
