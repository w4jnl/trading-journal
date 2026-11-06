// Data quality: the provenance line and the quality strip.
package gexview

import (
	"fmt"
	"github.com/w4jnl/trading-journal/internal/gex"
	"strings"
	"time"
)

// StaleBasis is how old the futures conversion's pairing may be before the
// strip says so. The ratio comes from the MotiveWave marks heartbeat; when
// the chart study stops posting, the engine keeps serving the last
// persisted ratio and the futures-denominated levels silently carry the
// basis drift since. Four hours matches the engine's own limit for reusing
// a nearby ratio (reconstruct_ratio_max_age_s).
const StaleBasis = 4 * time.Hour

// Quality is the strip's view model. Show is false when there is nothing
// to say (clean measured row, or unassessed) so the strip stays hidden.
type Quality struct {
	Show     bool `json:"show"`
	Degraded bool `json:"degraded"`
	// Kind names a provenance worth flagging: "reconstructed" (an older
	// chain repriced after an outage). Measured and recomputed rows are
	// equivalent for a reader and render nothing here.
	Kind     string           `json:"kind,omitempty"`
	KindNote string           `json:"kindNote,omitempty"`
	Warnings []QualityWarning `json:"warnings,omitempty"`
}

// QualityWarning is one engine finding with its pill class: "error" for
// severity degraded, "warn" for a notice.
type QualityWarning struct {
	Code     string `json:"code"`
	Symbol   string `json:"symbol,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message,omitempty"`
	Class    string `json:"class"`
}

// BuildQuality folds the engine's two objects into the strip model. Either
// may be nil (pre-v0.11.0 rows, or a profile payload without them).
func BuildQuality(dq *gex.DataQuality, prov *gex.Provenance) Quality {
	var q Quality
	if dq != nil {
		q.Degraded = dq.Degraded
		for _, w := range dq.Warnings {
			if w.Code == "RECOMPUTED" {
				// Update 010: every repaired row carries this notice. It is
				// provenance, not a trust finding - the footer names it
				// (ProvLine); a pill on every row since 2026-09-29 would
				// bury the real warnings.
				continue
			}
			cls := "warn"
			if w.Severity == "degraded" {
				cls = "error"
			}
			q.Warnings = append(q.Warnings, QualityWarning{
				Code: w.Code, Symbol: w.Symbol, Severity: w.Severity, Message: w.Message, Class: cls,
			})
		}
	}
	if prov != nil && prov.Kind == "reconstructed" {
		q.Kind = "reconstructed"
		q.KindNote = ReconstructedNote(prov)
	}
	q.Show = q.Degraded || q.Kind != "" || len(q.Warnings) > 0
	return q
}

// Add appends a warning and lights the strip.
func (q *Quality) Add(w QualityWarning) {
	q.Warnings = append(q.Warnings, w)
	q.Show = true
}

// StaleBasisWarning is the strip's pill for a futures conversion paired
// more than StaleBasis before the row's as-of (so a historical row is
// judged as of then, not against the clock). ok is false when the basis is
// fresh, cold (nil: the page already says so) or the pairing time unknown.
func StaleBasisWarning(fc *gex.FuturesConversion, asOf time.Time) (w QualityWarning, ok bool) {
	if fc == nil || fc.PairedAt.IsZero() {
		return w, false
	}
	age := asOf.Sub(fc.PairedAt)
	if age <= StaleBasis {
		return w, false
	}
	return QualityWarning{Code: "STALE_BASIS", Severity: "notice", Class: "warn",
		Message: fmt.Sprintf("futures conversion paired %s, %s before this snapshot: the MotiveWave marks heartbeat stopped, so the futures-denominated levels carry the basis drift since",
			fc.PairedAt.In(Eastern()).Format("Mon Jan 2 15:04 ET"), ageText(age))}, true
}

// ageText says a duration the way the strip reads it: "47 h" or "3 days".
func ageText(d time.Duration) string {
	if h := int(d.Hours()); h < 48 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// ReconstructedNote explains a reconstructed row in one sentence: the
// walls are OI-driven and near-exact, but the flip and regime were computed
// from the older chain's implied vols.
func ReconstructedNote(prov *gex.Provenance) string {
	vintage := "an older option chain"
	if prov != nil && !prov.ChainAsOf.IsZero() {
		vintage = "the " + prov.ChainAsOf.In(Eastern()).Format("Jan 2 15:04 ET") + " chain"
	}
	return "reconstructed after a feed outage: " + vintage + " repriced at this moment's spot — walls near-exact, flip/regime carry that chain's IVs"
}

// ProvLine is the footer suffix naming where a row's numbers came from
// when that is worth a word (update 010): a repaired row's reason
// ("recomputed: v0.12.0 numerics") and the straddle clock-health figure
// ("straddle +0.4%"). "" for a plain measured row without the check.
func ProvLine(prov *gex.Provenance, dq *gex.DataQuality) string {
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
