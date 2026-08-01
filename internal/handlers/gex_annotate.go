package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Gamma-context annotations (design §8 / P4): while prepare-day creates or
// reuses an entry, the annotator fetches the gex-engine snapshots that were
// live at the trade's entry and exit moments and appends one block - regime +
// percentile, side of / tick distance to the gamma flip, tick distances to
// the call/put walls - plus a machine-readable comment future backtesting can
// grep. The comment carries gexMarker, which doubles as the idempotency key:
// an entry that already contains it is never annotated (or fetched for)
// again. Annotation is strictly best-effort - unmapped symbols and
// pre-history moments skip silently, transport errors log once and disable
// the annotator for the rest of the pass, and nothing ever fails the review.
const gexMarker = "tj-gex:v1"

// gexStaleAfter is how far as_of may lag the trade moment before the context
// is labeled stale. Snapshots land on a 5-minute grid during 09:50-16:00 ET,
// so anything beyond ~15m means the trade sat outside the options session and
// got the prior close's levels (correct at-or-before behavior, but the block
// must be honest about it).
const gexStaleAfter = 15 * time.Minute

// gexMarket folds a journal symbol root onto an engine market: micros fold
// into the parent (same idea as copyClusterKey) and EP is CQG's name for ES.
// Journal fill prices for all of these are ES/NQ futures prices, so they
// compare directly against the snapshot's futures-denominated levels.
func gexMarket(root string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(root)) {
	case "ES", "MES", "EP":
		return "ES", true
	case "NQ", "MNQ":
		return "NQ", true
	}
	return "", false
}

// gexEastern lazily loads the options-session timezone. The gamma grid is an
// ET concept, so the block's time labels are ET regardless of the journal's
// trading timezone.
var (
	gexETOnce sync.Once
	gexETLoc  *time.Location
)

func gexEastern() *time.Location {
	gexETOnce.Do(func() {
		if l, err := time.LoadLocation("America/New_York"); err == nil {
			gexETLoc = l
		} else {
			gexETLoc = time.UTC
		}
	})
	return gexETLoc
}

// gexAnnotator carries one prepare-day pass's annotation state. broken trips
// on the first transport/auth error so a down engine costs a single timeout
// and a single warning, not one per entry.
type gexAnnotator struct {
	h      *Handlers
	broken bool
}

// annotate appends the gamma-context block to entryID, computed from the
// representative trade t (for group entries the cluster's first trade - same
// entry/exit window by construction). Never returns an error: the review
// flow must proceed identically whether or not gamma context is available.
func (a *gexAnnotator) annotate(ctx context.Context, entryID int64, t prepDayTrade) {
	h := a.h
	if h.GEX == nil || a.broken {
		return
	}
	market, ok := gexMarket(t.SymbolRoot)
	if !ok || t.ExitPrice == nil {
		return
	}
	e, err := h.Queries.GetJournalEntryByID(ctx, models.GetJournalEntryByIDParams{ID: entryID, UserID: userIDPtr(ctx)})
	if err != nil || strings.Contains(e.Body, gexMarker) {
		return
	}

	enSnap, err := h.GEX.Snapshot(ctx, market, t.EntryTime)
	if err != nil {
		a.fail(err, t.ID, "entry")
		return
	}
	exSnap, err := h.GEX.Snapshot(ctx, market, t.ExitTime)
	if err != nil {
		a.fail(err, t.ID, "exit")
		return
	}

	note := buildGexNote(t, market, enSnap, exSnap, h.gexTickSize(ctx, t.SymbolRoot))
	if err := h.appendOrSeedJournalNote(ctx, e, note); err != nil {
		h.logger().Warn("gex annotate: append failed", "entry", entryID, "err", err)
	}
}

// fail classifies a Snapshot error. ErrNoSnapshot is an expected skip (trade
// predates the engine's history); anything else is logged once and disables
// the annotator for the rest of the pass.
func (a *gexAnnotator) fail(err error, tradeID int64, moment string) {
	if errors.Is(err, gex.ErrNoSnapshot) {
		return
	}
	a.h.logger().Warn("gex annotate: fetch failed, skipping gamma context this pass",
		"trade", tradeID, "moment", moment, "err", err)
	a.broken = true
}

// gexTickSize resolves the tick size for distance math. The instruments row
// can be a needs_setup stub with a NULL tick spec; 0.25 is correct for every
// root gexMarket maps, so fall back rather than dropping the annotation.
func (h *Handlers) gexTickSize(ctx context.Context, root string) float64 {
	if inst, err := h.Queries.GetInstrument(ctx, root); err == nil && inst.TickSize != nil && *inst.TickSize > 0 {
		return *inst.TickSize
	}
	return 0.25
}

// gexMoment is the computed context for one of the trade's two moments.
type gexMoment struct {
	label string // "entry 14:32 ET"
	snap  *gex.Snapshot
	flip  *float64 // ticks from trade price to the flip (nil when flip is null)
	cw    *gex.Level
	pw    *gex.Level
	cwT   *float64 // ticks to the call wall (nil when absent / cold basis)
	pwT   *float64
	stale bool
}

func newGexMoment(kind string, at time.Time, price float64, s *gex.Snapshot, tick float64) gexMoment {
	m := gexMoment{
		label: kind + " " + at.In(gexEastern()).Format("15:04") + " ET",
		snap:  s,
		flip:  gexTicks(price, s.Flip.Futures, tick),
		cw:    gexWall(s, "call_wall"),
		pw:    gexWall(s, "put_wall"),
		stale: at.Sub(s.AsOf) > gexStaleAfter,
	}
	if m.cw != nil {
		m.cwT = gexTicks(price, m.cw.Futures, tick)
	}
	if m.pw != nil {
		m.pwT = gexTicks(price, m.pw.Futures, tick)
	}
	return m
}

// gexWall picks the snapshot's wall of the given type, preferring the "all"
// bucket. Nil when the snapshot has no such level.
func gexWall(s *gex.Snapshot, typ string) *gex.Level {
	var fallback *gex.Level
	for i := range s.Levels {
		l := &s.Levels[i]
		if l.Type != typ {
			continue
		}
		if l.Bucket == "all" {
			return l
		}
		if fallback == nil {
			fallback = l
		}
	}
	return fallback
}

// gexTicks is the signed tick distance from the trade price to a level.
// Positive = price above the level.
func gexTicks(price float64, level *float64, tick float64) *float64 {
	if level == nil || tick <= 0 {
		return nil
	}
	d := (price - *level) / tick
	return &d
}

func gexSide(ticks *float64) string {
	switch {
	case ticks == nil:
		return ""
	case *ticks > 0:
		return "above"
	case *ticks < 0:
		return "below"
	default:
		return "at"
	}
}

// buildGexNote renders the visible markdown block + the machine comment. The
// caller frames it (appendOrSeedJournalNote adds the divider / dated header),
// so the note starts directly with its own heading.
func buildGexNote(t prepDayTrade, market string, enSnap, exSnap *gex.Snapshot, tick float64) string {
	en := newGexMoment("entry", t.EntryTime, t.EntryPrice, enSnap, tick)
	ex := newGexMoment("exit", t.ExitTime, *t.ExitPrice, exSnap, tick)

	regimeCell := func(m gexMoment) string {
		p := "—"
		if v := m.snap.Regime.Percentile20D; v != nil {
			p = strconv.FormatFloat(*v, 'f', 0, 64)
		}
		return m.snap.Regime.State + " (P" + p + ")"
	}
	flipCell := func(m gexMoment, ref *float64) string {
		if m.flip == nil {
			return "—"
		}
		c := gexSide(m.flip) + " (" + fmt.Sprintf("%+.0f t", *m.flip) + ")"
		return c + gexDrift(m.snap.Flip.Futures, ref)
	}
	wallCell := func(ticks *float64, l *gex.Level, ref *float64) string {
		if ticks == nil {
			return "—"
		}
		var lv *float64
		if l != nil {
			lv = l.Futures
		}
		return fmt.Sprintf("%+.0f t", *ticks) + gexDrift(lv, ref)
	}

	var b strings.Builder
	b.WriteString("_Gamma context (GEX)_\n\n")
	b.WriteString("|  | " + en.label + " | " + ex.label + " |\n|---|---|---|\n")
	b.WriteString("| regime | " + regimeCell(en) + " | " + regimeCell(ex) + " |\n")
	b.WriteString("| flip " + gexLevelLabel(enSnap.Flip.Futures) + " | " + flipCell(en, nil) + " | " + flipCell(ex, enSnap.Flip.Futures) + " |\n")
	b.WriteString("| call wall " + gexWallLabel(en.cw) + " | " + wallCell(en.cwT, en.cw, nil) + " | " + wallCell(ex.cwT, ex.cw, gexWallFutures(en.cw)) + " |\n")
	b.WriteString("| put wall " + gexWallLabel(en.pw) + " | " + wallCell(en.pwT, en.pw, nil) + " | " + wallCell(ex.pwT, ex.pw, gexWallFutures(en.pw)) + " |\n")

	for _, m := range []gexMoment{en, ex} {
		if m.stale {
			b.WriteString("\n_" + strings.Fields(m.label)[0] + " context stale: levels as of " +
				m.snap.AsOf.In(gexEastern()).Format("Jan 2 15:04 ET") + " (prior session)_\n")
		}
	}

	b.WriteString("\n" + gexComment(market, en))
	return b.String()
}

// gexDrift annotates an exit cell whose level moved between the two
// snapshots: the row label carries the entry-moment value, so a shifted wall
// or flip shows its own value inline ("-129 t @ 7580.00").
func gexDrift(cur, ref *float64) string {
	if cur == nil || ref == nil {
		return ""
	}
	if d := *cur - *ref; d > 0.005 || d < -0.005 {
		return " @ " + strconv.FormatFloat(*cur, 'f', 2, 64)
	}
	return ""
}

func gexLevelLabel(v *float64) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func gexWallLabel(l *gex.Level) string {
	if l == nil {
		return "—"
	}
	return gexLevelLabel(l.Futures)
}

func gexWallFutures(l *gex.Level) *float64 {
	if l == nil {
		return nil
	}
	return l.Futures
}

// gexComment is the machine-readable line backtests grep for, computed for
// the ENTRY moment (the decision point). ASCII only, order-stable k=v pairs,
// "null" for values the engine legitimately omits.
func gexComment(market string, m gexMoment) string {
	num := func(v *float64) string {
		if v == nil {
			return "null"
		}
		return strconv.FormatFloat(*v, 'g', -1, 64)
	}
	ticks := func(v *float64) string {
		if v == nil {
			return "null"
		}
		return fmt.Sprintf("%+.1f", *v)
	}
	side := gexSide(m.flip)
	if side == "" {
		side = "null"
	}
	return "<!-- " + gexMarker +
		" market=" + market +
		" regime=" + m.snap.Regime.State +
		" pctl=" + num(m.snap.Regime.Percentile20D) +
		" flip_side=" + side +
		" dist_flip_ticks=" + ticks(m.flip) +
		" dist_cw_ticks=" + ticks(m.cwT) +
		" dist_pw_ticks=" + ticks(m.pwT) +
		" em=" + num(m.snap.ExpectedMove1D) +
		" stale=" + strconv.FormatBool(m.stale) +
		" -->"
}
