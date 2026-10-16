package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Journal editing from the phone (device keys) -------------------------
//
// Agent keys append and never overwrite; the trader's own phone edits like
// the page editors do. These routes need a device key and go through the
// same journal.Service saves as the HTML forms, so what they write is the
// trader's (MarkEntrySourcesTrader), the AI review lock applies
// (ErrReviewPending -> 409 review-pending), and an entry that changed
// since the phone loaded it is refused (409 stale) rather than overwritten.

// apiAttachmentRefRe matches the API image URLs the phone writes into a body.
var apiAttachmentRefRe = regexp.MustCompile(`/api/v1/attachments/(\d+)`)

// apiUnrewriteAttachmentRefs turns the API's image URLs back into the
// stored form (/journal/attachments/{id}), the inverse of
// apiRewriteAttachmentRefs.
func apiUnrewriteAttachmentRefs(md string) string {
	return apiAttachmentRefRe.ReplaceAllString(md, "/journal/attachments/$1")
}

// apiJournalUpdate is the PUT body: the whole entry, as the page editors
// save it.
type apiJournalUpdate struct {
	Body       string   `json:"body"`
	Tag        *string  `json:"tag"`
	Mood       *int64   `json:"mood"`
	StrategyID *int64   `json:"strategyId"`
	Mistakes   []string `json:"mistakes"`
	Emotions   []string `json:"emotions"`
	Checks     []int64  `json:"checks"`
	UpdatedAt  *string  `json:"updatedAt"`
}

// apiSaveError maps a journal save failure onto the API's status codes.
func (h *Handlers) apiSaveError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, journal.ErrReviewPending) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "reason": "review-pending"})
		return
	}
	var ve journal.ValidationError
	if errors.As(err, &ve) {
		writeJSONError(w, http.StatusBadRequest, ve.Msg)
		return
	}
	if errors.Is(err, journal.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "entry not found")
		return
	}
	h.logger().Error(what, "err", err)
	writeJSONError(w, http.StatusInternalServerError, "could not save the entry")
}

// APIJournalUpdate: PUT /api/v1/journal/{id} (device key) - replace the
// entry's body, tag, mood (day entries), strategy, labels and checklist
// ticks in one go. Pass updatedAt as loaded to refuse a stale edit.
func (h *Handlers) APIJournalUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	ctx := r.Context()
	tz := h.localTZ()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	e, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	var in apiJournalUpdate
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.UpdatedAt != nil {
		at, err := time.Parse(time.RFC3339Nano, *in.UpdatedAt)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "updatedAt must be RFC 3339")
			return
		}
		if d := at.Sub(e.UpdatedAt); d > time.Second || d < -time.Second {
			current, _ := h.journalDetailJSON(ctx, e, tz)
			writeJSON(w, http.StatusConflict, map[string]any{"error": "the entry changed since you loaded it", "reason": "stale", "entry": current})
			return
		}
	}
	body := strings.TrimSpace(apiUnrewriteAttachmentRefs(in.Body))
	if body == "" {
		writeJSONError(w, http.StatusBadRequest, "body must not be empty; DELETE the entry to remove it")
		return
	}
	var tag *string
	if in.Tag != nil && strings.TrimSpace(*in.Tag) != "" {
		if tag = parseTag(*in.Tag); tag == nil {
			writeJSONError(w, http.StatusBadRequest, "tag must be good, neutral or bad")
			return
		}
	}
	var mood *int64
	if in.Mood != nil {
		if e.Scope != "day" {
			writeJSONError(w, http.StatusBadRequest, "mood is recorded on day entries only")
			return
		}
		if *in.Mood < 1 || *in.Mood > 5 {
			writeJSONError(w, http.StatusBadRequest, "mood must be 1 to 5")
			return
		}
		mood = in.Mood
	}
	var strategyID *int64
	if in.StrategyID != nil && *in.StrategyID > 0 {
		if _, err := h.Queries.GetStrategy(ctx, models.GetStrategyParams{ID: *in.StrategyID, UserID: userID(ctx)}); err != nil {
			writeJSONError(w, http.StatusBadRequest, "unknown strategy")
			return
		}
		strategyID = in.StrategyID
	}
	// Labels are checked here, before the save: the service validates them
	// after it wrote the body, and a bad key must leave the entry as it was.
	for kind, keys := range map[string][]string{journal.KindMistake: in.Mistakes, journal.KindEmotion: in.Emotions} {
		for _, k := range keys {
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !journal.ValidLabel(kind, k) {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown %s label %q; one of: %s", kind, k, strings.Join(journal.LabelKeys(kind), ", ")))
				return
			}
		}
	}
	f := journal.Fields{Body: body, Mood: mood, Tag: tag, StrategyID: strategyID, Mistakes: in.Mistakes, Emotions: in.Emotions, Checks: in.Checks}
	var err error
	switch e.Scope {
	case "day":
		_, err = h.journalSvc().SaveDay(ctx, userID(ctx), e.EntryDate, f)
	case "week":
		_, err = h.journalSvc().SaveWeek(ctx, userID(ctx), e.EntryDate, f)
	case "trade":
		if e.TradeID == nil {
			writeJSONError(w, http.StatusConflict, "the entry's trade is gone")
			return
		}
		_, err = h.journalSvc().SaveTrade(ctx, userID(ctx), *e.TradeID, f)
	default:
		_, err = h.journalSvc().SaveGroup(ctx, userID(ctx), e.ID, f)
	}
	if err != nil {
		h.apiSaveError(w, "api journal update", err)
		return
	}
	h.LiveData.Bump(userID(ctx))
	saved, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	resp, err := h.journalDetailJSON(ctx, saved, tz)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "saved, but could not render the entry")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// APIJournalDelete: DELETE /api/v1/journal/{id} (device key).
func (h *Handlers) APIJournalDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	ctx := r.Context()
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad id")
		return
	}
	e, ok := h.apiLoadEntry(w, r, id)
	if !ok {
		return
	}
	var err error
	switch e.Scope {
	case "day":
		_, err = h.journalSvc().DeleteDay(ctx, userID(ctx), e.EntryDate)
	case "week":
		_, err = h.journalSvc().DeleteWeek(ctx, userID(ctx), e.EntryDate)
	default:
		err = h.journalSvc().Delete(ctx, userID(ctx), e.ID)
	}
	if err != nil {
		h.apiSaveError(w, "api journal delete", err)
		return
	}
	h.LiveData.Bump(userID(ctx))
	w.WriteHeader(http.StatusNoContent)
}

// APIJournalAttachmentUpload: POST /api/v1/journal/attachments (device
// key, multipart "file") - store an image and return the markdown to put
// in a body.
func (h *Handlers) APIJournalAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	id, name, size, ok := h.storeAttachmentUpload(w, r)
	if !ok {
		return
	}
	url := fmt.Sprintf("/api/v1/attachments/%d", id)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "url": url, "name": name, "size": size,
		"markdown": "![" + strings.ReplaceAll(name, "]", "") + "](" + url + ")",
	})
}

// APIJournalLabels: GET /api/v1/journal/labels - the fixed vocabulary with
// display names and a description of each label's evidence, for pickers.
func (h *Handlers) APIJournalLabels(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for _, kind := range []string{journal.KindMistake, journal.KindEmotion} {
		list := make([]map[string]string, 0)
		for _, l := range journal.LabelOptions(kind) {
			list = append(list, map[string]string{"key": l.Key, "name": l.Name, "description": l.Desc})
		}
		out[kind+"s"] = list
	}
	writeJSON(w, http.StatusOK, out)
}
