package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
)

// The Market > Gamma (GEX) page: current (or as-of a chosen moment) gamma
// state for ES/NQ from the gex-engine - regime, flip, walls, expected move,
// and the per-strike net-GEX profile chart. This is the same data the review
// annotations condense; the page is the browsable view of it. Feature-gated
// on Handlers.GEX like every optional service: nil hides the nav item
// (gexEnabled func) and 404s the route.

// gexPageMoment is the "as of" input format (a datetime-local control,
// interpreted in the journal's trading timezone).
const gexPageMoment = "2006-01-02T15:04"

// gexPageLevel is one row of the levels table, pre-formatted.
type gexPageLevel struct {
	Label   string
	Bucket  string
	Index   string
	Futures string
	Gex     string
	Dist    string // signed points from spot (futures terms)
}

// GEXPage renders /gex and /gex/{market}. Engine trouble never 5xxes the
// page: a missing snapshot renders an empty state, transport errors render a
// flash, and the shell (market toggle, as-of picker) stays usable.
func (h *Handlers) GEXPage(w http.ResponseWriter, r *http.Request) {
	if h.GEX == nil {
		http.NotFound(w, r)
		return
	}
	market := strings.ToUpper(strings.TrimSpace(r.PathValue("market")))
	if market == "" {
		market = "ES"
	}
	if market != "ES" && market != "NQ" {
		http.NotFound(w, r)
		return
	}

	tz := h.localTZ()
	at := time.Now()
	latest := true
	atParam := strings.TrimSpace(r.URL.Query().Get("at"))
	if atParam != "" {
		if t, err := time.ParseInLocation(gexPageMoment, atParam, tz); err == nil {
			at, latest = t, false
		} else {
			atParam = ""
		}
	}

	data := map[string]any{
		"NavActive": "gex",
		"BackHref":  validBackHref(r),
		"Market":    market,
		"AtParam":   atParam,
		"AtValue":   at.In(tz).Format(gexPageMoment), // picker prefill
		"Latest":    latest,
	}

	s, err := h.GEX.Snapshot(r.Context(), market, at)
	switch {
	case errors.Is(err, gex.ErrNoSnapshot):
		data["Empty"] = true
		data["EmptyWhen"] = at.In(gexEastern()).Format("Jan 2 2006 15:04 ET")
	case err != nil:
		h.logger().Warn("gex page: snapshot fetch failed", "market", market, "err", err)
		data["EngineError"] = err.Error()
	default:
		gexPageFill(data, s, latest)
	}
	h.renderPage(w, r, "gex.html", data)
}

// gexPageFill turns a snapshot into the template's pre-formatted view model.
// Every engine-nullable value renders "-" instead of erroring (client.go
// documents which ones those are).
func gexPageFill(data map[string]any, s *gex.Snapshot, latest bool) {
	et := gexEastern()
	data["AsOf"] = s.AsOf
	data["AsOfET"] = s.AsOf.In(et).Format("Mon Jan 2, 15:04:05 ET")
	data["Stale"] = latest && time.Since(s.AsOf) > gexStaleAfter

	state := s.Regime.State
	data["Regime"] = state
	switch state {
	case "positive":
		data["RegimeClass"] = "pnl-pos"
	case "negative":
		data["RegimeClass"] = "pnl-neg"
	}
	data["Pctl"] = "—"
	if v := s.Regime.Percentile20D; v != nil {
		data["Pctl"] = strconv.FormatFloat(*v, 'f', 0, 64)
	}
	data["NetGex"] = gexFmtB(s.Regime.NetGexAtSpot)

	data["SpotIndex"] = fmt.Sprintf("%.2f", s.SpotIndex)
	var spotFut *float64
	if fc := s.FuturesConversion; fc != nil {
		f := s.SpotIndex * fc.Ratio
		spotFut = &f
		data["SpotFutures"] = fmt.Sprintf("%.2f", f)
		data["ConvLine"] = fmt.Sprintf("futures conversion %.4f · %s · paired %s",
			fc.Ratio, fc.Source, fc.PairedAt.In(et).Format("15:04 ET"))
	} else {
		data["SpotFutures"] = "—"
		data["ConvLine"] = "futures conversion unavailable (cold basis) — futures-denominated values missing"
	}

	data["FlipFutures"] = "—"
	data["FlipSub"] = ""
	if f := s.Flip.Futures; f != nil {
		data["FlipFutures"] = fmt.Sprintf("%.2f", *f)
		if spotFut != nil {
			d := *spotFut - *f
			side := "above"
			if d < 0 {
				side = "below"
			}
			data["FlipSub"] = fmt.Sprintf("spot %+.2f pts (%s flip)", d, side)
		}
	}

	data["EM"] = "—"
	if v := s.ExpectedMove1D; v != nil {
		data["EM"] = fmt.Sprintf("%.1f pts", *v)
	}

	levels := make([]gexPageLevel, 0, len(s.Levels))
	for _, l := range s.Levels {
		row := gexPageLevel{
			Label:   gexLevelName(l.Type),
			Bucket:  l.Bucket,
			Index:   fmt.Sprintf("%.2f", l.Index),
			Futures: "—",
			Gex:     gexFmtB(l.Gex),
			Dist:    "—",
		}
		if l.Futures != nil {
			row.Futures = fmt.Sprintf("%.2f", *l.Futures)
			if spotFut != nil {
				row.Dist = fmt.Sprintf("%+.2f pts", *l.Futures-*spotFut)
			}
		}
		levels = append(levels, row)
	}
	data["Levels"] = levels

	chart := map[string]any{
		"strikes": s.Profile.StrikesIndex,
		"gex":     s.Profile.NetGex,
		"spot":    s.SpotIndex,
		"flip":    s.Flip.Index,
		"cw":      gexWallIndex(s, "call_wall"),
		"pw":      gexWallIndex(s, "put_wall"),
	}
	if b, err := json.Marshal(chart); err == nil {
		data["ChartJSON"] = template.JS(b)
	}
	data["HasProfile"] = len(s.Profile.StrikesIndex) > 1 &&
		len(s.Profile.StrikesIndex) == len(s.Profile.NetGex)
}

// gexFmtB renders a raw gamma-dollar value at billions scale ("+74.5B").
func gexFmtB(v float64) string {
	abs := v
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1e9, abs == 0:
		return fmt.Sprintf("%+.1fB", v/1e9)
	case abs >= 1e6:
		return fmt.Sprintf("%+.0fM", v/1e6)
	default:
		return fmt.Sprintf("%+.0f", v)
	}
}

func gexLevelName(typ string) string {
	switch typ {
	case "call_wall":
		return "Call wall"
	case "put_wall":
		return "Put wall"
	default:
		return strings.ReplaceAll(typ, "_", " ")
	}
}

// gexWallIndex is the chart-side wall position (index-denominated; walls
// always carry an index value even when the futures conversion is cold).
func gexWallIndex(s *gex.Snapshot, typ string) *float64 {
	if l := gexWall(s, typ); l != nil {
		return &l.Index
	}
	return nil
}
