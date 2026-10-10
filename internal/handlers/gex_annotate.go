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

// gexCleanSteps caps how many degraded rows the annotator steps past before
// settling for the degraded one (engine update 009: the engine's own history
// statistics skip degraded rows the same way). Three rows is 15 minutes of
// grid - the same distance gexStaleAfter allows, so a clean row is never
// bought at the price of a stale one.
const gexCleanSteps = 3

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
func (a *gexAnnotator) Annotate(ctx context.Context, entryID int64, t prepDayTrade) {
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

	enSnap, exSnap, err := h.gexMomentSnapshots(ctx, market, t)
	if err != nil {
		a.fail(err, t.ID, "entry/exit")
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

// gexMomentSnapshots fetches the rows live at the trade's entry and exit,
// stepping past degraded rows (CleanSnapshot) so a zeroed-quote window or a
// mismatched leg does not become the trade's recorded context when a clean
// row sits one grid step earlier. Skips are logged at debug level (-d).
func (h *Handlers) gexMomentSnapshots(ctx context.Context, market string, t prepDayTrade) (en, ex *gex.Snapshot, err error) {
	en, enSkipped, err := h.GEX.CleanSnapshot(ctx, market, t.EntryTime, gexCleanSteps, gexStaleAfter)
	if err != nil {
		return nil, nil, err
	}
	ex, exSkipped, err := h.GEX.CleanSnapshot(ctx, market, t.ExitTime, gexCleanSteps, gexStaleAfter)
	if err != nil {
		return nil, nil, err
	}
	if enSkipped > 0 || exSkipped > 0 {
		h.logger().Debug("gex annotate: stepped past degraded rows",
			"trade", t.ID, "entrySkipped", enSkipped, "exitSkipped", exSkipped)
	}
	return en, ex, nil
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
	flipI *float64 // ticks to the intraday (<=5 DTE) flip; nil pre-v0.6.0
	cw    *gex.Level
	pw    *gex.Level
	cwT   *float64 // ticks to the call wall (nil when absent / cold basis)
	pwT   *float64
	stale bool
	// degraded / recon carry the engine's per-row verdict (update 009):
	// the header cell names them and a footnote explains each.
	degraded bool
	recon    bool
}

func newGexMoment(kind string, at time.Time, price float64, s *gex.Snapshot, tick float64) gexMoment {
	m := gexMoment{
		label: kind + " " + at.In(gexEastern()).Format("15:04") + " ET",
		snap:  s,
		flip:  gexTicks(price, s.Flip.Futures, tick),
		flipI: gexTicks(price, s.FlipIntraday.Futures, tick),
		cw:    gexWall(s, "call_wall"),
		pw:    gexWall(s, "put_wall"),
		stale: at.Sub(s.AsOf) > gexStaleAfter,
		// Degraded means the engine found at least one degraded-severity
		// warning on this row; recon means the row was rebuilt from an
		// older chain after an outage (walls near-exact, flip/regime
		// carrying that chain's IVs). Pre-v0.11.0 rows are neither.
		degraded: s.IsDegraded(),
		recon:    s.ProvenanceKind() == "reconstructed",
	}
	if m.cw != nil {
		m.cwT = gexTicks(price, m.cw.Futures, tick)
	}
	if m.pw != nil {
		m.pwT = gexTicks(price, m.pw.Futures, tick)
	}
	return m
}

// gexWall picks the snapshot's all-book wall of the given type. Since engine
// v0.5.0 the wall types also appear with bucket "0dte" (today's expiring
// book), so matching on type alone would misread a 0DTE wall as the all-book
// level; the bucket guard is contract-mandated (engine design doc, update
// 002). Nil when the snapshot has no such level.
func gexWall(s *gex.Snapshot, typ string) *gex.Level {
	return gexLevelIn(s, typ, "all")
}

// gexLevelIn picks the snapshot's level of the given type and bucket; nil
// when absent.
func gexLevelIn(s *gex.Snapshot, typ, bucket string) *gex.Level {
	for i := range s.Levels {
		l := &s.Levels[i]
		if l.Type == typ && l.Bucket == bucket {
			return l
		}
	}
	return nil
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
		c := m.snap.Regime.State + " (P" + p + ")"
		// The front-week reading is quoted when it DISAGREES - that is the
		// session context that explains behavior (engine update 004);
		// agreement would just repeat the state. The machine comment always
		// carries it either way.
		if is := m.snap.Regime.IntradayState; is != "" && is != m.snap.Regime.State {
			c += " · intraday " + is
		}
		return c
	}
	flipCell := func(ticks, cur, ref *float64) string {
		if ticks == nil {
			return "—"
		}
		c := gexSide(ticks) + " (" + fmt.Sprintf("%+.0f t", *ticks) + ")"
		return c + gexDrift(cur, ref)
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
	// Name the denomination: every level and tick distance in this block is
	// futures-priced (matching the trade's own fill prices), while the
	// engine also publishes index values for the same levels.
	b.WriteString("_Gamma context (GEX) — futures prices_\n\n")
	// The header names each moment's verdict inline ("exit 14:40 ET
	// (degraded)") so a reader scanning the table sees it before the
	// numbers; the footnotes below carry the engine's reasons.
	b.WriteString("|  | " + en.label + gexMomentFlags(en) + " | " + ex.label + gexMomentFlags(ex) + " |\n|---|---|---|\n")
	b.WriteString("| regime | " + regimeCell(en) + " | " + regimeCell(ex) + " |\n")
	b.WriteString("| flip " + gexLevelLabel(enSnap.Flip.Futures) + " | " + flipCell(en.flip, en.snap.Flip.Futures, nil) + " | " + flipCell(ex.flip, ex.snap.Flip.Futures, enSnap.Flip.Futures) + " |\n")
	// The intraday (<=5 DTE) flip is the front-week hedging line - for day
	// trades usually the more relevant regime boundary, sitting far closer to
	// spot than the whole-book flip (engine update 003). The row appears only
	// when a moment has it: pre-v0.6.0 snapshots serve null, and rendering a
	// dash row into every historical annotation would be noise, not absence.
	if enSnap.FlipIntraday.Futures != nil || exSnap.FlipIntraday.Futures != nil {
		b.WriteString("| flip intraday " + gexLevelLabel(enSnap.FlipIntraday.Futures) + " | " + flipCell(en.flipI, en.snap.FlipIntraday.Futures, nil) + " | " + flipCell(ex.flipI, ex.snap.FlipIntraday.Futures, enSnap.FlipIntraday.Futures) + " |\n")
	}
	b.WriteString("| call wall " + gexWallLabel(en.cw) + " | " + wallCell(en.cwT, en.cw, nil) + " | " + wallCell(ex.cwT, ex.cw, gexWallFutures(en.cw)) + " |\n")
	b.WriteString("| put wall " + gexWallLabel(en.pw) + " | " + wallCell(en.pwT, en.pw, nil) + " | " + wallCell(ex.pwT, ex.pw, gexWallFutures(en.pw)) + " |\n")

	for _, m := range []gexMoment{en, ex} {
		word := strings.Fields(m.label)[0]
		if m.stale {
			b.WriteString("\n_" + word + " context stale: levels as of " +
				m.snap.AsOf.In(gexEastern()).Format("Jan 2 15:04 ET") + " (prior session)_\n")
		}
		if m.degraded {
			b.WriteString("\n_" + word + " context degraded: " + gexWarningList(m.snap.DataQuality) + "_\n")
		}
		if m.recon {
			b.WriteString("\n_" + word + " context " + gexReconstructedNote(m.snap.Provenance) + "_\n")
		}
	}

	b.WriteString("\n" + gexComment(market, en))
	return b.String()
}

// gexMomentFlags is the header-cell suffix naming a moment's verdict:
// " (degraded)", " (reconstructed)" or both; "" for a clean measured row.
func gexMomentFlags(m gexMoment) string {
	var flags []string
	if m.degraded {
		flags = append(flags, "degraded")
	}
	if m.recon {
		flags = append(flags, "reconstructed")
	}
	if len(flags) == 0 {
		return ""
	}
	return " (" + strings.Join(flags, ", ") + ")"
}

// gexWarningList joins the degraded-severity findings ("STALE_LEG SPY - SPY
// leg 18 min older than SPX; FROZEN_SPOT SPX - ..."); when the engine flagged
// the row without naming a degraded finding, says so.
func gexWarningList(dq *gex.DataQuality) string {
	ws := dq.DegradedWarnings()
	if len(ws) == 0 {
		return "engine flagged this row as degraded"
	}
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		parts = append(parts, w.Summary())
	}
	return strings.Join(parts, "; ")
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
	intra := m.snap.Regime.IntradayState
	if intra == "" {
		intra = "null"
	}
	sideI := gexSide(m.flipI)
	if sideI == "" {
		sideI = "null"
	}
	return "<!-- " + gexMarker +
		" market=" + market +
		" regime=" + m.snap.Regime.State +
		" intraday_regime=" + intra +
		" pctl=" + num(m.snap.Regime.Percentile20D) +
		" flip_side=" + side +
		" dist_flip_ticks=" + ticks(m.flip) +
		" flip_intraday_side=" + sideI +
		" dist_flip_intraday_ticks=" + ticks(m.flipI) +
		" dist_cw_ticks=" + ticks(m.cwT) +
		" dist_pw_ticks=" + ticks(m.pwT) +
		" em=" + num(m.snap.ExpectedMove1D) +
		" stale=" + strconv.FormatBool(m.stale) +
		// Update 009: the engine's verdict on the row (null = persisted
		// before v0.11.0, never assessed) and how the row came to be
		// (measured | recomputed | reconstructed; null provenance reads as
		// measured per the contract). Appended, so the v1 marker and every
		// earlier key keep their positions for existing greps.
		" quality=" + gexQualityToken(m.snap) +
		" provenance=" + m.snap.ProvenanceKind() +
		// Update 010: em= above is expected_move_1d - intraday the move left
		// to that session's close. em_next= is the true next-session move
		// (null before engine v0.12.0 unless the row was repaired).
		" em_next=" + num(m.snap.ExpectedMoveNextSession) +
		" -->"
}

// gexQualityToken is the machine-comment value for data_quality: "null"
// when the engine never assessed the row, else "degraded" or "ok".
func gexQualityToken(s *gex.Snapshot) string {
	switch {
	case s.DataQuality == nil:
		return "null"
	case s.DataQuality.Degraded:
		return "degraded"
	}
	return "ok"
}
