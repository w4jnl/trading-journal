package gex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Data quality and provenance (engine v0.11.0, journal update 009). A vendor
// host move on 2026-09-28 took every fetch down for a session, and the review
// found degraded rows (zeroed-quote windows, mismatched legs) feeding the
// percentile, the 1h/1d deltas and the flip times unflagged. The engine now
// says, per row, how much to trust it and where it came from, and fills
// outage gaps with flagged reconstructed rows. Both objects are additive and
// NULL on rows persisted before v0.11.0, which the contract says to read as
// measured-and-not-assessed: nil here never means "bad".

// DataQuality is the engine's per-row trust verdict. Degraded is true when at
// least one warning has severity "degraded" (EMPTY_CHAIN, PARTIAL_CHAIN,
// STALE_LEG, RATIO_DRIFT, FROZEN_SPOT, repeated STALE_PAYLOAD). The funnel,
// cross-check and rate-source details the engine also publishes are not
// decoded - the journal acts on the verdict and the warnings only.
type DataQuality struct {
	Degraded bool             `json:"degraded"`
	Warnings []QualityWarning `json:"warnings"`
	// SnapshotAgeSeconds is how old the underlying payload was when the row
	// was computed; QuoteEffectiveAt the moment the quotes describe (payload
	// timestamp minus the vendor's 15-minute delay). Both informational.
	SnapshotAgeSeconds *float64 `json:"snapshot_age_seconds"`
	QuoteEffectiveAt   ISOTime  `json:"quote_effective_at"`
}

// QualityWarning is one finding. Severity is "degraded" or "notice"; Symbol
// names the leg it concerns (may be empty for book-wide findings).
type QualityWarning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Symbol   string `json:"symbol"`
	Message  string `json:"message"`
}

// Provenance says how a row came to be. Kind is "measured" (a live cycle),
// "recomputed" (rebuilt exactly from that slot's archived payloads - as good
// as measured) or "reconstructed" (an older chain repriced at the slot's
// spot after a feed outage: ChainAsOf is the chain's vintage while the row's
// as_of is the grid moment it describes; walls are near-exact because they
// are OI-driven, but the flip and regime carry the older chain's IVs).
type Provenance struct {
	Kind       string  `json:"kind"`
	ChainAsOf  ISOTime `json:"chain_as_of"`
	SpotSource string  `json:"spot_source"`
	SpotAsOf   ISOTime `json:"spot_as_of"`
	Reason     string  `json:"reason"`
}

// ISOTime is a timestamp that tolerates null, absence and an unparseable
// value (all decode to the zero time). It exists so a malformed provenance
// or quality detail can never take the whole snapshot - and with it the GEX
// page - down: these fields are informational, the row's numbers are not.
type ISOTime struct{ time.Time }

// UnmarshalJSON accepts RFC 3339 (with or without fractional seconds and
// with a Z or numeric offset), the same shape every other engine timestamp
// uses; anything else is treated as absent rather than as an error.
func (t *ISOTime) UnmarshalJSON(b []byte) error {
	t.Time = time.Time{}
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00"} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v
			return nil
		}
	}
	return nil
}

// MarshalJSON keeps the zero value as null so a round trip is honest.
func (t ISOTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time)
}

// IsDegraded reports the engine's verdict; false for rows the engine never
// assessed (pre-v0.11.0), which the contract equates with clean.
func (s *Snapshot) IsDegraded() bool {
	return s != nil && s.DataQuality != nil && s.DataQuality.Degraded
}

// ProvenanceKind resolves the row's kind with the contract's default: a null
// provenance is a measured row.
func (s *Snapshot) ProvenanceKind() string {
	if s == nil || s.Provenance == nil || s.Provenance.Kind == "" {
		return "measured"
	}
	return s.Provenance.Kind
}

// DegradedWarnings returns the warnings that carry the degraded severity,
// in the engine's order; nil when the row is clean or unassessed.
func (d *DataQuality) DegradedWarnings() []QualityWarning {
	if d == nil {
		return nil
	}
	var out []QualityWarning
	for _, w := range d.Warnings {
		if w.Severity == "degraded" {
			out = append(out, w)
		}
	}
	return out
}

// Summary renders a warning as "CODE SYM - message" (the symbol and message
// are optional), the one-line form the annotation footnotes and the AI facts
// use.
func (w QualityWarning) Summary() string {
	var b strings.Builder
	b.WriteString(w.Code)
	if w.Symbol != "" {
		b.WriteString(" " + w.Symbol)
	}
	if w.Message != "" {
		b.WriteString(" - " + w.Message)
	}
	return b.String()
}

// CleanSnapshot is Snapshot with the engine's own history-statistics rule
// applied (update 009): when the at-or-before row is degraded, step to the
// previous row and take the first clean one - at most maxSteps rows back,
// and never to a row older than `within` before the requested moment, so a
// clean row is never bought at the price of being stale. When every
// candidate is degraded or the window runs out, the row the engine served
// first is returned (flagged, so the caller can say so). skipped counts the
// degraded rows passed over; 0 when the first row was clean.
//
// The step is one microsecond before the current row's as_of: as_of carries
// microseconds and the engine's at-or-before is inclusive, so that instant
// addresses exactly the preceding row.
func (c *Client) CleanSnapshot(ctx context.Context, market string, at time.Time, maxSteps int, within time.Duration) (*Snapshot, int, error) {
	first, err := c.Snapshot(ctx, market, at)
	if err != nil {
		return nil, 0, err
	}
	if !first.IsDegraded() {
		return first, 0, nil
	}
	cur := first
	for i := 1; i <= maxSteps; i++ {
		prevAt := cur.AsOf.Add(-time.Microsecond)
		if within > 0 && at.Sub(prevAt) > within {
			break
		}
		s, err := c.Snapshot(ctx, market, prevAt)
		if err != nil {
			// No older row, or the engine hiccuped mid-walk: the degraded
			// row in hand is still the honest answer.
			break
		}
		if within > 0 && at.Sub(s.AsOf) > within {
			// The preceding row sits beyond the window (a gap in the grid,
			// or the prior session): a clean-but-stale row would mislead.
			break
		}
		if !s.IsDegraded() {
			return s, i, nil
		}
		cur = s
	}
	return first, 0, nil
}

// Health is the engine's GET /health, reduced to what the journal shows:
// the status word plus the v0.11.0 reasons (why status is what it is) and
// notices. Reasons and notices are rendered as text; the engine publishes
// them as strings or small objects, and either is flattened here.
type Health struct {
	Status  string
	Reasons []string
	Notices []string
}

// Health fetches the engine's health summary. Callers use it to explain an
// odd state (a stale "latest" row during session hours, a failing fetch) -
// it is best-effort by nature, so a short context deadline is the norm.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return nil, fmt.Errorf("gex: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gex request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("gex: health status %d", resp.StatusCode)
	}
	var raw struct {
		Status  string            `json:"status"`
		Reasons []json.RawMessage `json:"reasons"`
		Notices []json.RawMessage `json:"notices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("gex: decode health: %w", err)
	}
	return &Health{Status: raw.Status, Reasons: flattenMessages(raw.Reasons), Notices: flattenMessages(raw.Notices)}, nil
}

// flattenMessages turns health reasons/notices into display strings: a JSON
// string as-is; an object by its message-like field (message, text, reason,
// detail - the first present, else "code"), falling back to compact JSON so
// nothing the engine says is silently dropped.
func flattenMessages(items []json.RawMessage) []string {
	var out []string
	for _, it := range items {
		var s string
		if err := json.Unmarshal(it, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(it, &obj); err == nil {
			picked := ""
			for _, k := range []string{"message", "text", "reason", "detail", "code"} {
				if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
					picked = strings.TrimSpace(v)
					break
				}
			}
			if picked == "" {
				keys := make([]string, 0, len(obj))
				for k := range obj {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, fmt.Sprintf("%s=%v", k, obj[k]))
				}
				picked = strings.Join(parts, " ")
			}
			out = append(out, picked)
			continue
		}
		if t := strings.TrimSpace(string(it)); t != "" && t != "null" {
			out = append(out, t)
		}
	}
	return out
}
