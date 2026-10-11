package handlers

import (
	"context"
	"net/http"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/journal"
)

// Playbook checklists on the entry editors: one block per strategy with
// items (the picker shows the block of the selected strategy), the ticks
// of the entry being edited, and the saved entry's checklist for the read
// view.

type checklistBlock struct {
	StrategyID int64
	Items      []journal.ChecklistItem
}

// checklistData is the editor's input: every strategy's items (so switching
// the strategy shows its list without a round trip) and the entry's ticks.
func (h *Handlers) checklistData(ctx context.Context, entryID int64) ([]checklistBlock, map[int64]bool) {
	checked := map[int64]bool{}
	byStrategy, err := h.journalSvc().ChecklistsForUser(ctx, userID(ctx))
	if err != nil {
		h.logger().Warn("checklists", "err", err)
		return nil, checked
	}
	var blocks []checklistBlock
	for sid, items := range byStrategy {
		blocks = append(blocks, checklistBlock{StrategyID: sid, Items: items})
	}
	if entryID != 0 {
		if cl, err := h.journalSvc().EntryChecklist(ctx, userID(ctx), entryID); err == nil && cl != nil {
			for _, it := range cl.Items {
				if it.Checked {
					checked[it.ID] = true
				}
			}
		}
	}
	return blocks, checked
}

// entryChecklist is the saved entry's checklist for the read view (nil
// without a strategy or items).
func (h *Handlers) entryChecklist(ctx context.Context, entryID int64) *journal.Checklist {
	if entryID == 0 {
		return nil
	}
	cl, err := h.journalSvc().EntryChecklist(ctx, userID(ctx), entryID)
	if err != nil {
		h.logger().Warn("entry checklist", "entry", entryID, "err", err)
		return nil
	}
	return cl
}

// checkFields reads the ticked checklist items off a submitted editor form.
func checkFields(r *http.Request) []int64 {
	var out []int64
	for _, v := range r.Form["check"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}
