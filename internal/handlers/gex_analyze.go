package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/w4jnl/trading-journal/internal/ai"
	"html/template"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/gexview"
	"github.com/w4jnl/trading-journal/internal/models"
)

// AI reads of a gamma snapshot, for the Market > Gamma page.
//
// A snapshot is immutable once captured, so its read is generated once and
// then RETAINED, keyed by the snapshot's own as_of - scrubbing away and back,
// or arriving via a different at-or-before moment, shows the same text
// instead of spending another LLM call on an answer that cannot change.
//
// The model is not asked to eyeball the chart. gexAnalysisFacts computes the
// geometry that actually carries the read - how wide the current gamma pocket
// is against one expected move, how far each wall is in expected-move
// multiples, whether there is any positive shelf between the flip and the put
// wall - and hands those over as numbers. That is the difference between "positive
// gamma, expect chop" and "the whole pocket is one session wide".

// gexAnalysisSystem is the analyst voice: structural, quantitative, willing
// to say a level does not matter today.
const gexAnalysisSystem = `You are a derivatives-flow analyst reading one gamma-exposure snapshot for a futures day trader. You receive computed facts about the snapshot - never guess or invent a number that is not in them.

Write a SHORT read in Markdown, in this shape:

1. One opening paragraph (2-4 sentences): what the structure says at face value, then the more precise thing the geometry says. Lead with the naive read and correct it - the value you add is the correction.
2. Then 2-4 bullets, each led by a bold label, covering what follows for the session: what happens above the flip, what happens below it, and which levels are close enough to matter today.

Rules that make the read useful:
- Express distances BOTH in points and in expected-move multiples ("172 points, about 4x the expected move"). A level several expected moves away is not an intraday level - say so plainly and tell the trader to ignore it today.
- The expected move is the market-implied one-standard-deviation range: intraday it is the move left to this session's close (the facts also give the next full session's move when the engine publishes it). If a gamma pocket is narrower than one expected move, that is the headline: the regime is a pocket you are inside, not a state you can lean on.
- Positive gamma above the flip means dealer hedging dampens moves (fades, failed breakouts, mean reversion). Below the flip it amplifies them (momentum, expansion). Say which side price is on and what that implies for trade selection.
- Name the strongest node and treat it as the pin or the ceiling.
- Distinguish structure from prediction. "If it breaks, hedging stops braking" is structure. "It will go there" is not - never predict direction.
- No preamble, no restating the input as a list, no headings. Interpret; do not summarize.`

// gexAnalysisPromptFor assembles the system prompt, honoring the user's
// configured review language the same way the journal analyst does.
func gexAnalysisPromptFor(language string) string {
	if language == "" {
		return gexAnalysisSystem
	}
	return "WRITE THE ENTIRE READ IN " + language + " - every sentence and every bold label - even though these instructions are in English.\n\n" +
		gexAnalysisSystem +
		"\n\nReminder: write the read in " + language + ", not English."
}

// gexAnalysisFacts renders the computed geometry the model reasons over. One
// fact per line, denominations named, so a local model cannot mix them up.
func gexAnalysisFacts(s *gex.Snapshot, market string) string {
	et := gexview.Eastern()
	spotFut := gexview.SpotFutures(s)
	denom, spot := "index", s.SpotIndex
	if spotFut != nil {
		denom, spot = "futures", *spotFut
	}
	// Levels in the SAME denomination as spot, so every distance below is
	// computed against a like-for-like price.
	pick := func(idx float64, fut *float64) *float64 {
		if spotFut != nil {
			return fut
		}
		v := idx
		return &v
	}
	var flip *float64
	if spotFut != nil {
		flip = s.Flip.Futures
	} else {
		flip = s.Flip.Index
	}
	cwL, pwL := gexview.Wall(s, "call_wall"), gexview.Wall(s, "put_wall")
	var cw, pw *float64
	if cwL != nil {
		cw = pick(cwL.Index, cwL.Futures)
	}
	if pwL != nil {
		pw = pick(pwL.Index, pwL.Futures)
	}

	// The expected move is published in INDEX points; scale the distance when
	// everything else is futures-denominated.
	var em float64
	if e := s.ExpectedMove1D; e != nil {
		em = *e
		if spotFut != nil && s.FuturesConversion != nil {
			em *= s.FuturesConversion.Ratio
		}
	}

	var b strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	line("market: %s futures", market)
	line("snapshot time: %s", s.AsOf.In(et).Format("Mon Jan 2 2006, 15:04 ET"))
	// Trust verdict and origin (engine v0.11.0, update 009): emitted only
	// when the row carries them, so retained reads of older snapshots stay
	// byte-identical. Degraded rows keep their walls (OI-driven) but their
	// percentile, deltas and flip times are suspect; a reconstructed row
	// was rebuilt from an older chain, so its flip and regime inherit that
	// chain's implied vols.
	if s.IsDegraded() {
		line("DATA QUALITY: DEGRADED - %s. Treat the regime percentile, the 1h/1d changes and any flip timing as unreliable for this row; the walls (open-interest driven) are the most trustworthy part", gexWarningList(s.DataQuality))
	} else if s.DataQuality != nil {
		if ws := s.DataQuality.Warnings; len(ws) > 0 {
			parts := make([]string, 0, len(ws))
			for _, w := range ws {
				parts = append(parts, w.Summary())
			}
			line("data quality notices (not degrading): %s", strings.Join(parts, "; "))
		}
	}
	switch s.ProvenanceKind() {
	case "reconstructed":
		vintage := "an older option chain"
		if p := s.Provenance; p != nil && !p.ChainAsOf.IsZero() {
			vintage = "the option chain as of " + p.ChainAsOf.In(et).Format("15:04 ET")
		}
		line("PROVENANCE: RECONSTRUCTED - this row was rebuilt after a feed outage from %s repriced at this moment's spot; the walls are near-exact, but the flip and the regime carry that chain's implied volatilities, so lean on the walls and treat the flip as approximate", vintage)
	case "recomputed":
		// A repaired row (v0.12.0 numerics) is recomputed in place from the
		// slot's archived payloads: as trustworthy as a live measurement,
		// and - when a reason is given - carrying corrected numbers.
		reason := ""
		if p := s.Provenance; p != nil && p.Reason != "" {
			reason = " (" + p.Reason + ")"
		}
		line("provenance: recomputed from this slot's archived payloads%s - as trustworthy as a live measurement", reason)
	}
	line("all prices below are %s-denominated", denom)
	line("regime: %s", s.Regime.State)
	if p := s.Regime.Percentile20D; p != nil {
		line("regime percentile (20d): %.0f", *p)
	} else {
		line("regime percentile (20d): not available yet")
	}
	if total, ok := s.Regime.Total(); ok {
		line("total net gamma across the whole book: %s", gexview.FmtB(total))
	}
	// Front-week regime (engine v0.6.1): emitted only when the snapshot has
	// it, so retained reads of older snapshots stay byte-identical.
	if is := s.Regime.IntradayState; is != "" {
		if is != s.Regime.State {
			line("front-week (<=5 DTE) regime: %s - DISAGREES with the whole-book regime above; spot sits between the intraday and whole-book flips, and for the session the front-week reading usually dominates behavior", is)
		} else {
			line("front-week (<=5 DTE) regime: %s (agrees with the whole book)", is)
		}
	}
	line("spot: %.2f", spot)
	// Two expected moves since engine v0.12.0 (update 010): the front
	// expiry's straddle is the move left to THIS session's close - the
	// intraday yardstick every distance below is measured against - and the
	// next-session figure is the true one-day move. Rows without the new
	// field keep the old single sentence, so retained reads of older
	// snapshots are unaffected.
	if em > 0 {
		if n := s.ExpectedMoveNextSession; n != nil {
			next := *n
			if spotFut != nil && s.FuturesConversion != nil {
				next *= s.FuturesConversion.Ratio
			}
			line("expected move to this session's close (1 standard deviation; the front expiry's straddle, which shrinks through the day): %.1f points", em)
			line("expected-move band to the close: %.2f to %.2f", spot-em, spot+em)
			line("expected move for the next full session (1 standard deviation): %.1f points", next)
			line("every 'x the expected move' multiple below uses the move to the close")
		} else {
			line("expected move (1 session, 1 standard deviation): %.1f points", em)
			line("expected-move band: %.2f to %.2f", spot-em, spot+em)
		}
	} else {
		line("expected move: not available")
	}

	dist := func(name string, v *float64, extra string) {
		if v == nil {
			line("%s: not present in this snapshot", name)
			return
		}
		d := *v - spot
		if em > 0 {
			line("%s: %.2f (%+.1f points from spot, %.1fx the expected move)%s", name, *v, d, math.Abs(d)/em, extra)
		} else {
			line("%s: %.2f (%+.1f points from spot)%s", name, *v, d, extra)
		}
	}
	flipExtra := ""
	if flip != nil {
		if spot > *flip {
			flipExtra = " - spot is ABOVE the flip (long-gamma side)"
		} else {
			flipExtra = " - spot is BELOW the flip (short-gamma side)"
		}
	}
	dist("gamma flip", flip, flipExtra)
	// The front-week (<=5 DTE) flip, engine v0.6.0. Emitted only when the
	// snapshot has it: retained reads of pre-v0.6.0 snapshots should not
	// gain a "not present" line they never had.
	var flipI *float64
	if spotFut != nil {
		flipI = s.FlipIntraday.Futures
	} else {
		flipI = s.FlipIntraday.Index
	}
	if flipI != nil {
		side := "ABOVE"
		if spot <= *flipI {
			side = "BELOW"
		}
		dist("intraday gamma flip (front week, <=5 DTE)", flipI,
			fmt.Sprintf(" - spot is %s it; for the session this hedging flip usually matters more than the whole-book flip", side))
	}
	cwExtra, pwExtra := "", ""
	if cwL != nil {
		cwExtra = ", gamma " + gexview.FmtB(cwL.Gex)
	}
	if pwL != nil {
		pwExtra = ", gamma " + gexview.FmtB(pwL.Gex)
	}
	dist("call wall", cw, cwExtra)
	dist("put wall", pw, pwExtra)

	// The pocket: how much room the current gamma side actually has before
	// the structure changes. This is the fact the naive read misses.
	if flip != nil && em > 0 {
		if spot > *flip && cw != nil {
			w := *cw - *flip
			line("long-gamma pocket (flip to call wall): %.1f points wide, %.2fx the expected move", w, w/em)
			line("floor of that pocket (the flip) sits %.1f points below spot", spot-*flip)
		} else if spot <= *flip && pw != nil {
			w := *flip - *pw
			line("short-gamma zone (put wall to flip): %.1f points wide, %.2fx the expected move", w, w/em)
		}
	}

	// Profile shape: where the structure actually is, and where there is
	// nothing at all. "No positive shelf between the flip and the put wall"
	// is what turns a wall distance into an air pocket.
	if n := len(s.Profile.StrikesIndex); n > 1 && n == len(s.Profile.NetGex) {
		// Profile strikes are index-denominated at source. Convert them into
		// the working denomination so every price in this block is directly
		// comparable - quoting a node at its index strike beside a futures
		// wall reads as two levels when it is one (a live model did exactly
		// that: "node at 7550" next to "call wall 7578.89", the same level).
		conv := func(k float64) float64 { return k }
		if spotFut != nil && s.FuturesConversion != nil {
			r := s.FuturesConversion.Ratio
			conv = func(k float64) float64 { return k * r }
		}
		type node struct {
			idx, gex float64
		}
		var maxPos, maxNeg node
		for i, k := range s.Profile.StrikesIndex {
			g := s.Profile.NetGex[i]
			if g > maxPos.gex {
				maxPos = node{k, g}
			}
			if g < maxNeg.gex {
				maxNeg = node{k, g}
			}
		}
		// Naming the level when a node IS a wall saves the model from
		// presenting one price as two: the biggest positive node usually IS
		// the call wall, by construction.
		named := func(idx float64) string {
			switch {
			case cwL != nil && idx == cwL.Index:
				return " - this IS the call wall, not a separate level"
			case pwL != nil && idx == pwL.Index:
				return " - this IS the put wall, not a separate level"
			}
			return ""
		}
		line("profile: %d strikes from %.0f to %.0f", n, conv(s.Profile.StrikesIndex[0]), conv(s.Profile.StrikesIndex[n-1]))
		if maxPos.gex > 0 {
			line("largest positive node: %.2f at %s%s", conv(maxPos.idx), gexview.FmtB(maxPos.gex), named(maxPos.idx))
		}
		if maxNeg.gex < 0 {
			line("largest negative node: %.2f at %s%s", conv(maxNeg.idx), gexview.FmtB(maxNeg.gex), named(maxNeg.idx))
		}
		if s.Flip.Index != nil && pwL != nil {
			lo, hi := pwL.Index, *s.Flip.Index
			pos, tot := 0, 0
			for i, k := range s.Profile.StrikesIndex {
				if k < lo || k > hi {
					continue
				}
				tot++
				if s.Profile.NetGex[i] > 0 {
					pos++
				}
			}
			line("between the put wall and the flip: %d strikes, %d of them positive-gamma", tot, pos)
			if pos == 0 && tot > 0 {
				line("=> there is NO positive-gamma shelf anywhere between the flip and the put wall")
			}
		}
	}
	return b.String()
}

// gexAnalysisKey is the retention key: the snapshot's own capture time, in a
// canonical form (see the migration).
func gexAnalysisKey(asOf time.Time) string { return asOf.UTC().Format(time.RFC3339Nano) }

// gexAnalysisFor returns the retained read for a snapshot and the model that
// wrote it, or "" when none is stored yet.
func (h *Handlers) gexAnalysisFor(ctx context.Context, market string, asOf time.Time) (body, model string) {
	if h.Queries == nil {
		return "", ""
	}
	row, err := h.Queries.GetGexAnalysis(ctx, models.GetGexAnalysisParams{
		UserID: userID(ctx), Market: market, AsOf: gexAnalysisKey(asOf),
	})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			h.logger().Warn("gex analysis lookup", "market", market, "err", err)
		}
		return "", ""
	}
	return row.Body, row.Model
}

// gexReadHTML is the read as the scrub path swaps it in: the rendered
// markdown with the model line the partial shows under it.
func gexReadHTML(body, model string) template.HTML {
	out := renderMarkdown(body)
	if model != "" {
		out += `<p class="ai-model-line">` + template.HTMLEscapeString(model) + `</p>`
	}
	return template.HTML(out)
}

// gexAttachAnalysis fills the view's snapshot identity and any retained read.
// Kept off gexview.BuildView (which stays a pure function of the snapshot) because
// this part needs the request's user.
func (h *Handlers) gexAttachAnalysis(ctx context.Context, v *gexview.View, market string, asOf time.Time) {
	v.AsOfParam = asOf.UTC().Format(time.RFC3339Nano)
	if body, model := h.gexAnalysisFor(ctx, market, asOf); body != "" {
		v.Analysis, v.AnalysisModel = body, model
		v.AnalysisHTML = gexReadHTML(body, model)
	}
}

// GEXAnalyze: POST /gex/analyze/{market}?at=<unix> - generate (or return the
// retained) AI read for the snapshot at that moment and render it. Responds
// with the analysis fragment for an in-place HTMX swap, so the page keeps its
// scrub position. ?force=1 regenerates and replaces the stored text.
func (h *Handlers) GEXAnalyze(w http.ResponseWriter, r *http.Request) {
	clearWriteDeadline(w) // waits on the AI model, possibly longer than the server WriteTimeout
	if h.GEX == nil || !h.aiPossible() {
		http.NotFound(w, r)
		return
	}
	market, ok := gexMarketParam(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// at is the snapshot's own as_of, RFC3339 with its fractional second
	// intact. Whole-second precision is NOT enough: as_of sits a fraction of
	// a second past its grid mark, so a truncated value is earlier than the
	// snapshot and at-or-before hands back the previous one - the read would
	// then be pinned to a moment the user never looked at.
	atParam := strings.TrimSpace(r.URL.Query().Get("at"))
	at, err := time.Parse(time.RFC3339, atParam)
	if err != nil {
		http.Error(w, "at must be an RFC3339 timestamp", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	s, err := h.GEX.Snapshot(ctx, market, at)
	if err != nil {
		if errors.Is(err, gex.ErrNoSnapshot) {
			http.Error(w, "no snapshot at that moment", http.StatusNotFound)
			return
		}
		h.logger().Warn("gex analyze: snapshot fetch failed", "market", market, "err", err)
		http.Error(w, "gex-engine unreachable", http.StatusBadGateway)
		return
	}

	body, model := "", ""
	if r.URL.Query().Get("force") != "1" {
		body, model = h.gexAnalysisFor(ctx, market, s.AsOf)
	}
	if body == "" {
		client, ref, err := h.aiFor(ctx)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, errNoAIProvider) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		lang := h.userAILanguage(ctx)
		label := ref.Label(aiWordsFor(lang))
		user := gexAnalysisFacts(s, market)
		if lang != "" {
			user = "Reply in " + lang + ".\n\n" + user
		}
		model = label
		resp, err := ai.Do(ctx, client, ai.Cached(gexAnalysisPromptFor(lang), user))
		out := resp.Text
		if err != nil {
			h.logger().Warn("gex analyze: generate failed", "market", market, "err", err)
			http.Error(w, "AI analysis failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		body = strings.TrimSpace(out)
		if body == "" {
			http.Error(w, "AI returned an empty read", http.StatusBadGateway)
			return
		}
		if err := h.Queries.UpsertGexAnalysis(ctx, models.UpsertGexAnalysisParams{
			UserID: userID(ctx), Market: market, AsOf: gexAnalysisKey(s.AsOf),
			Model: model, Body: body,
		}); err != nil {
			// The read is still worth showing; only its retention failed.
			h.logger().Error("gex analysis not retained", "market", market, "err", err)
		}
	}

	h.renderPartial(w, "_gex_analysis.html", "gex_analysis_body", map[string]any{
		"Analysis": body, "Model": model,
	})
}
