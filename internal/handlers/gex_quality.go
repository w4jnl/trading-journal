package handlers

import (
	"context"
	"time"
)

// Data-quality presentation shared by the GEX overview, the Exposure tab and
// the scrub JSON (engine v0.11.0, update 009). The engine says per row how
// much to trust it (data_quality) and where it came from (provenance); the
// journal renders that as a strip of pills under the page banners so a
// degraded or reconstructed moment is never read as a clean measurement.
// Rows persisted before v0.11.0 carry neither object and render nothing.

// gexHealthTimeout bounds the best-effort /health lookup the overview makes
// when the live row looks wrong (stale during session hours, or the fetch
// failed). A down engine already cost one client timeout; the explanation
// must not double it.
const gexHealthTimeout = 1500 * time.Millisecond

// gexHealthNotes returns the engine's own explanation of its state - the
// v0.11.0 /health reasons followed by notices - or nil when the engine says
// it is fine or cannot be asked. Rendered verbatim next to the "session
// closed" and "unreachable" banners so an outage reads as an outage.
func (h *Handlers) gexHealthNotes(ctx context.Context) []string {
	if h.GEX == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gexHealthTimeout)
	defer cancel()
	hl, err := h.GEX.Health(ctx)
	if err != nil {
		return nil
	}
	if hl.Status == "ok" && len(hl.Reasons) == 0 {
		return nil
	}
	notes := make([]string, 0, len(hl.Reasons)+len(hl.Notices)+1)
	if hl.Status != "" && hl.Status != "ok" {
		notes = append(notes, "status "+hl.Status)
	}
	notes = append(notes, hl.Reasons...)
	notes = append(notes, hl.Notices...)
	return notes
}
