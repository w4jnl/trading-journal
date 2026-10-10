package handlers

import (
	"context"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
)

// Data-quality presentation shared by the GEX overview, the Exposure tab and
// the scrub JSON (engine v0.11.0, update 009). The engine says per row how
// much to trust it (data_quality) and where it came from (provenance); the
// journal renders that as a strip of pills under the page banners so a
// degraded or reconstructed moment is never read as a clean measurement.
// Rows persisted before v0.11.0 carry neither object and render nothing.

// gexQuality is the strip's view model. Show is false when there is nothing
// to say (clean measured row, or unassessed) so the strip stays hidden.
type gexQuality struct {
	Show     bool `json:"show"`
	Degraded bool `json:"degraded"`
	// Kind names a provenance worth flagging: "reconstructed" (an older
	// chain repriced after an outage). Measured and recomputed rows are
	// equivalent for a reader and render nothing here.
	Kind     string              `json:"kind,omitempty"`
	KindNote string              `json:"kindNote,omitempty"`
	Warnings []gexQualityWarning `json:"warnings,omitempty"`
}

// gexQualityWarning is one engine finding with its pill class: "error" for
// severity degraded, "warn" for a notice.
type gexQualityWarning struct {
	Code     string `json:"code"`
	Symbol   string `json:"symbol,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message,omitempty"`
	Class    string `json:"class"`
}

// gexBuildQuality folds the engine's two objects into the strip model. Either
// may be nil (pre-v0.11.0 rows, or a profile payload without them).
func gexBuildQuality(dq *gex.DataQuality, prov *gex.Provenance) gexQuality {
	var q gexQuality
	if dq != nil {
		q.Degraded = dq.Degraded
		for _, w := range dq.Warnings {
			if w.Code == "RECOMPUTED" {
				// Update 010: every repaired row carries this notice. It is
				// provenance, not a trust finding - the footer names it
				// (gexProvLine); a pill on every row since 2026-09-29 would
				// bury the real warnings.
				continue
			}
			cls := "warn"
			if w.Severity == "degraded" {
				cls = "error"
			}
			q.Warnings = append(q.Warnings, gexQualityWarning{
				Code: w.Code, Symbol: w.Symbol, Severity: w.Severity, Message: w.Message, Class: cls,
			})
		}
	}
	if prov != nil && prov.Kind == "reconstructed" {
		q.Kind = "reconstructed"
		q.KindNote = gexReconstructedNote(prov)
	}
	q.Show = q.Degraded || q.Kind != "" || len(q.Warnings) > 0
	return q
}

// gexReconstructedNote explains a reconstructed row in one sentence: the
// walls are OI-driven and near-exact, but the flip and regime were computed
// from the older chain's implied vols.
func gexReconstructedNote(prov *gex.Provenance) string {
	vintage := "an older option chain"
	if prov != nil && !prov.ChainAsOf.IsZero() {
		vintage = "the " + prov.ChainAsOf.In(gexEastern()).Format("Jan 2 15:04 ET") + " chain"
	}
	return "reconstructed after a feed outage: " + vintage + " repriced at this moment's spot — walls near-exact, flip/regime carry that chain's IVs"
}

// gexProvLine is the footer suffix naming where a row's numbers came from
// when that is worth a word (update 010): a repaired row's reason
// ("recomputed: v0.12.0 numerics") and the straddle clock-health figure
// ("straddle +0.4%"). "" for a plain measured row without the check.
func gexProvLine(prov *gex.Provenance, dq *gex.DataQuality) string {
	var parts []string
	if prov != nil && prov.Kind == "recomputed" {
		p := "recomputed"
		if prov.Reason != "" {
			p += ": " + prov.Reason
		}
		parts = append(parts, p)
	}
	if dq != nil {
		if l := dq.Straddle.Label(); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " · ")
}

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
