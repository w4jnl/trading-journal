package handlers

import (
	"context"
	"net/http"
)

// aiBusyMessage is the one 409 text every adapter uses while the user's AI
// day job holds the journal.
const aiBusyMessage = "AI analysis is running - cancel it or wait before editing the journal."

// journalChanged bumps the user's live-data version after a journal write
// has committed, so open dashboard, calendar, feed and journal-day pages
// re-fetch their live regions (CLAUDE.md: every mutation path bumps the
// notifier POST-COMMIT). Every journal mutation - HTML, API, MCP, the AI
// reviewer, the GEX annotator - funnels through the shared helpers in
// api_journal.go or calls this directly after its own write.
func (h *Handlers) journalChanged(ctx context.Context) {
	if h.LiveData != nil {
		h.LiveData.Bump(userID(ctx))
	}
}

// guardJournalingDuringAIJSON is guardJournalingDuringAI for the JSON API:
// a 409 with the same message, as JSON.
func (h *Handlers) guardJournalingDuringAIJSON(w http.ResponseWriter, r *http.Request) bool {
	if h.aiJobRunning(r.Context()) {
		writeJSONError(w, http.StatusConflict, aiBusyMessage)
		return true
	}
	return false
}
