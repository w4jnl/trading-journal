package handlers

import (
	"context"
	"time"

	"github.com/w4jnl/trading-journal/internal/journal"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The journal domain lives in internal/journal; the HTML handlers, the JSON
// API, the MCP tools, the AI reviewer and the GEX annotator reach it through
// these adapters, which add the two things only a request knows: the caller
// (userID from the context) and the live-data notifier.

type (
	apiCreateInput     = journal.CreateInput
	createConflict     = journal.Conflict
	apiValidationError = journal.ValidationError
	prepDayTrade       = journal.PrepTrade
	preparedEntry      = journal.PreparedEntry
)

const (
	aiMarkerLocal     = journal.MarkerLocal
	aiMarkerMCP       = journal.MarkerMCP
	aiPlaceholderBody = journal.PlaceholderBody
)

func badInput(format string, a ...any) error { return journal.BadInput(format, a...) }

func clusterCopytrader(trs []prepDayTrade) ([][]prepDayTrade, []prepDayTrade) {
	return journal.ClusterCopytrader(trs)
}

func copyClusterKey(symbolRoot string) string { return journal.CopyClusterKey(symbolRoot) }

func parseISOWeek(s string, tz *time.Location) (time.Time, int, int, error) {
	return journal.ParseISOWeek(s, tz)
}

// journalSvc is the journal service wired to this Handlers: the writer pool,
// the trading timezone, the live-data bump, and the GEX annotator.
func (h *Handlers) journalSvc() *journal.Service {
	h.journalOnce.Do(func() {
		h.journal = &journal.Service{
			DB: h.DB, Q: h.Queries, TZ: h.localTZ,
			Changed: func(_ context.Context, userID int64) {
				if h.LiveData != nil {
					h.LiveData.Bump(userID)
				}
			},
			NewAnnotator: func() journal.Annotator { return &gexAnnotator{h: h} },
		}
	})
	return h.journal
}

func (h *Handlers) createJournalEntry(ctx context.Context, in apiCreateInput) (models.JournalEntry, *createConflict, error) {
	return h.journalSvc().Create(ctx, userID(ctx), in)
}

func (h *Handlers) appendOrSeedJournalNote(ctx context.Context, entry models.JournalEntry, note string) error {
	return h.journalSvc().AppendNote(ctx, userID(ctx), entry, note)
}

func (h *Handlers) assignTagIfUnset(ctx context.Context, entryID int64, tag string) (bool, error) {
	return h.journalSvc().AssignTagIfUnset(ctx, userID(ctx), entryID, tag)
}

func (h *Handlers) assignStrategyIfUnset(ctx context.Context, entryID, strategyID int64) (bool, error) {
	return h.journalSvc().AssignStrategyIfUnset(ctx, userID(ctx), entryID, strategyID)
}

func (h *Handlers) entryHasMarker(ctx context.Context, entryID int64, marker string) bool {
	return h.journalSvc().EntryHasMarker(ctx, userID(ctx), entryID, marker)
}

func (h *Handlers) prepareDayEntries(ctx context.Context, dateStr string) (*prepDayResponse, error) {
	res, err := h.journalSvc().PrepareDay(ctx, userID(ctx), dateStr)
	if err != nil {
		return nil, err
	}
	out := &prepDayResponse{PrepResult: res}
	if day, err := time.ParseInLocation("2006-01-02", res.Date, h.localTZ()); err == nil {
		out.Checks = h.coachReport(ctx, day)
	}
	return out, nil
}

// lastTradingDay: the trading-tz date of the caller's most recent closed
// trade before today (zero when none). tz is the service's own; the
// parameter stays for the call sites' sake.
func (h *Handlers) lastTradingDay(ctx context.Context, _ *time.Location) (time.Time, error) {
	return h.journalSvc().LastTradingDay(ctx, userID(ctx))
}
