package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

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

// loadEntryLabels fetches the labels of many entries in one query (the
// feed and the API list), keyed by entry id then kind, labels sorted.
func loadEntryLabels(ctx context.Context, db *sql.DB, entryIDs []int64) (map[int64]map[string][]string, error) {
	out := map[int64]map[string][]string{}
	if len(entryIDs) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(entryIDs))
	ph = ph[:len(ph)-1]
	args := make([]any, 0, len(entryIDs))
	for _, id := range entryIDs {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, `SELECT journal_entry_id, kind, label FROM journal_entry_labels WHERE journal_entry_id IN (`+ph+`) ORDER BY kind, label`, args...)
	if err != nil {
		return nil, fmt.Errorf("batch labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var kind, label string
		if err := rows.Scan(&id, &kind, &label); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string][]string{}
		}
		out[id][kind] = append(out[id][kind], label)
	}
	return out, rows.Err()
}

// journalLabelPattern is one label with its count over the listed entries
// and a link that adds it to the filter (or drops it when already active).
type journalLabelPattern struct {
	Kind   string
	Key    string
	Name   string
	N      int
	Active bool
	URL    string
}

// journalLabelPatterns counts each label over the entries on the page, most
// frequent first, so the feed shows what keeps happening and lets the
// trader drill into it with one click.
func journalLabelPatterns(f journalFilters, labels map[int64]map[string][]string) []journalLabelPattern {
	counts := map[[2]string]int{}
	for _, byKind := range labels {
		for kind, keys := range byKind {
			for _, k := range keys {
				counts[[2]string{kind, k}]++
			}
		}
	}
	if len(counts) == 0 {
		return nil
	}
	base := url.Values{}
	base.Set("from", f.fromStr)
	base.Set("to", f.toStr)
	for _, v := range f.scopes {
		base.Add("scope", v)
	}
	for _, v := range f.tags {
		base.Add("tag", v)
	}
	for _, id := range f.strategies {
		base.Add("strategy", strconv.FormatInt(id, 10))
	}
	if f.strategyNone {
		base.Add("strategy", "none")
	}
	f.labelValues(base)
	for _, k := range []string{"from", "to"} {
		if base.Get(k) == "" {
			base.Del(k)
		}
	}
	toggleURL := func(kind, key string, active bool) string {
		v := url.Values{}
		param := kind // "mistake" | "emotion"
		for k, vals := range base {
			for _, x := range vals {
				if active && k == param && x == key {
					continue
				}
				v.Add(k, x)
			}
		}
		if !active {
			v.Add(param, key)
		}
		if enc := v.Encode(); enc != "" {
			return "/journal?" + enc
		}
		return "/journal"
	}
	out := make([]journalLabelPattern, 0, len(counts))
	for ck, n := range counts {
		kind, key := ck[0], ck[1]
		active := (kind == journal.KindMistake && f.mistakeSet[key]) || (kind == journal.KindEmotion && f.emotionSet[key])
		out = append(out, journalLabelPattern{Kind: kind, Key: key, Name: journal.LabelName(kind, key), N: n, Active: active, URL: toggleURL(kind, key, active)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
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
