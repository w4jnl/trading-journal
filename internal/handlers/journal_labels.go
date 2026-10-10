package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// entryLabels is the editors' read of an entry's review labels; errors and a
// zero id both read as "none" (the pickers simply start empty).
func (h *Handlers) entryLabels(ctx context.Context, entryID int64) (mistakes, emotions []string) {
	if entryID == 0 {
		return nil, nil
	}
	labels, err := h.journalSvc().EntryLabels(ctx, userID(ctx), entryID)
	if err != nil {
		h.logger().Warn("entry labels", "entry", entryID, "err", err)
		return nil, nil
	}
	return labels[journal.KindMistake], labels[journal.KindEmotion]
}

// labelsOut shapes an entry's labels for JSON: both kinds always present,
// never null, so clients can index without nil checks.
func labelsOut(m map[string][]string) map[string][]string {
	out := map[string][]string{}
	for _, k := range journal.LabelKinds {
		v := m[k]
		if v == nil {
			v = []string{}
		}
		out[k+"s"] = v
	}
	return out
}

// saveFailed writes a service error from an editor save: bad input (an
// unknown label key, a malformed field) is the browser's fault and gets a
// 400 with the message; anything else is a server error.
func (h *Handlers) saveFailed(w http.ResponseWriter, what string, err error) {
	var ve apiValidationError
	if errors.As(err, &ve) {
		http.Error(w, ve.Msg, http.StatusBadRequest)
		return
	}
	h.serverError(w, fmt.Errorf("%s: %w", what, err))
}

// labelFields reads the two pickers off a submitted editor form.
func labelFields(r *http.Request) (mistakes, emotions []string) {
	return r.Form["mistake"], r.Form["emotion"]
}

// APIJournalAddLabels: POST /api/v1/journal/{id}/labels {mistakes:[], emotions:[]}
// Adds labels; never removes what the trader set (the page editors do
// that). Unknown keys are a 400 naming the vocabulary.
func (h *Handlers) APIJournalAddLabels(w http.ResponseWriter, r *http.Request) {
	if h.guardJournalingDuringAIJSON(w, r) {
		return
	}
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Mistakes []string `json:"mistakes"`
		Emotions []string `json:"emotions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body (expected {\"mistakes\": [...], \"emotions\": [...]})")
		return
	}
	if len(body.Mistakes) == 0 && len(body.Emotions) == 0 {
		writeJSONError(w, http.StatusBadRequest, "nothing to add: pass mistakes and/or emotions")
		return
	}
	labels, err := h.addLabels(r.Context(), id, body.Mistakes, body.Emotions)
	switch {
	case errors.Is(err, journal.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "entry not found")
		return
	case err != nil:
		var ve apiValidationError
		if errors.As(err, &ve) {
			writeJSONError(w, http.StatusBadRequest, ve.Msg)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "could not add labels")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "labels": labelsOut(labels)})
}

// addLabels adds both kinds (either may be empty) and returns the entry's
// labels afterwards. Shared by the API and the MCP tool.
func (h *Handlers) addLabels(ctx context.Context, entryID int64, mistakes, emotions []string) (map[string][]string, error) {
	svc := h.journalSvc()
	uid := userID(ctx)
	if len(mistakes) > 0 {
		if err := svc.SetLabels(ctx, uid, entryID, journal.KindMistake, mistakes, false); err != nil {
			return nil, err
		}
	}
	if len(emotions) > 0 {
		if err := svc.SetLabels(ctx, uid, entryID, journal.KindEmotion, emotions, false); err != nil {
			return nil, err
		}
	}
	return svc.EntryLabels(ctx, uid, entryID)
}
