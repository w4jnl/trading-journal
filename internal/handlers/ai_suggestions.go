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

// suggestionView is one open suggestion as a pill: Label is what it reads on
// the note ("Tag: bad"), Name the bare value ("bad") where the pill's colour
// already says the kind (the feed row).
type suggestionView struct {
	Kind, Value, Label, Name string
}

// suggestedIn reports whether the open suggestions hold kind/value: the
// editors mark that choice in their pickers.
func suggestedIn(list []suggestionView, kind, value string) bool {
	for _, sg := range list {
		if sg.Kind == kind && sg.Value == value {
			return true
		}
	}
	return false
}

// entrySuggestions loads the open suggestions of the caller's entry as pills.
func (h *Handlers) entrySuggestions(ctx context.Context, entryID int64) []suggestionView {
	list, err := h.journalSvc().Suggestions(ctx, userID(ctx), entryID)
	if err != nil {
		h.logger().Warn("load suggestions", "entry", entryID, "err", err)
		return nil
	}
	return suggestionViews(list, func(id int64) (string, bool) {
		st, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: id, UserID: userID(ctx)})
		return st.Name, err == nil
	})
}

// suggestionViews turns open suggestions into pills; strategyName resolves a
// strategy id to its name, and a strategy it does not know is left out.
func suggestionViews(list []journal.Suggestion, strategyName func(id int64) (string, bool)) []suggestionView {
	out := make([]suggestionView, 0, len(list))
	for _, sg := range list {
		v := suggestionView{Kind: sg.Kind, Value: sg.Value}
		switch sg.Kind {
		case journal.SuggestTag:
			v.Name = sg.Value
			v.Label = "Tag: " + v.Name
		case journal.SuggestStrategy:
			id, _ := strconv.ParseInt(sg.Value, 10, 64)
			name, ok := strategyName(id)
			if !ok {
				continue
			}
			v.Name = name
			v.Label = "Strategy: " + name
		default:
			v.Name = journal.LabelName(sg.Kind, sg.Value)
			v.Label = v.Name
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
