package handlers

import "context"

// aiDayRunMessage refuses a second full-day review (prepare_day) while the
// in-app day run is going.
const aiDayRunMessage = "An AI day review is running - wait for it to finish or cancel it before preparing another day."

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
