package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/dayplan"
	"github.com/w4jnl/trading-journal/internal/levelmap"
	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- The day map (/levels) -----------------------------------------------
//
// One level set per market and trading day: the journal's own bar math
// (internal/levelmap), the session map a MotiveWave chart posted (the feed,
// api_levels.go) and the gamma engine's walls, merged with a source on
// every level. Built on demand, held a minute in memory, and written to
// level_maps so a past day opens as it stood: the premarket phase is
// frozen once the RTH open passed, the session phase is the latest.

const (
	levelsRootsDays    = 30  // the markets traded this recently get a map
	levelsDailyDays    = 160 // daily history read for pivots, periods, swings and gaps
	levelsIntradayDays = 10  // recent fine bars for the session, swings and touches
	levelsCacheTTL     = time.Minute
	levelsFeedKeep     = 30 * 24 * time.Hour
	levelsGammaMaxAge  = 24 * time.Hour
)

type levelsCacheEntry struct {
	m  *levelmap.Map
	at time.Time
}

// levelsPrefPage is the view preference that holds the markets the user
// pinned on Settings, comma-separated.
const levelsPrefPage = "levels.markets"

// levelsPinned are the markets the user asked for on Settings, whatever
// they traded.
func (h *Handlers) levelsPinned(ctx context.Context) []string {
	if h.Queries == nil {
		return nil
	}
	v, err := h.Queries.GetViewPref(ctx, models.GetViewPrefParams{UserID: userID(ctx), Page: levelsPrefPage})
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range strings.Split(v, ",") {
		if levelmap.IsMarket(m) {
			out = append(out, m)
		}
	}
	return out
}

// levelsPinnedSet is the pinned list as a set, for the Settings card.
func (h *Handlers) levelsPinnedSet(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for _, m := range h.levelsPinned(ctx) {
		out[m] = true
	}
	return out
}

// SettingsLevelsSave: POST /settings/levels - the markets to keep on the
// Levels page besides the ones traded lately.
func (h *Handlers) SettingsLevelsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var keep []string
	for _, m := range r.PostForm["market"] {
		if levelmap.IsMarket(m) && !hasString(keep, m) {
			keep = append(keep, m)
		}
	}
	if err := h.Queries.UpsertViewPref(ctx, models.UpsertViewPrefParams{UserID: userID(ctx), Page: levelsPrefPage, Value: strings.Join(keep, ","), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/settings?flash=levels#levels", http.StatusSeeOther)
}

// levelsMarkets are the markets the user gets a map for: the full-size
// roots of what they traded in the last thirty days, the markets pinned on
// Settings, plus any market a chart posted for the day, in the map's
// display order.
func (h *Handlers) levelsMarkets(ctx context.Context, day string) []string {
	want := map[string]bool{}
	for _, m := range h.levelsPinned(ctx) {
		want[m] = true
	}
	rows, err := h.rdb().QueryContext(ctx, `
		SELECT DISTINCT e.symbol_root FROM executions e
		JOIN accounts a ON a.id = e.account_id
		WHERE a.user_id = ? AND e.deleted_at IS NULL AND e.exec_time >= ?`,
		userID(ctx), time.Now().AddDate(0, 0, -levelsRootsDays).UTC())
	if err == nil {
		for rows.Next() {
			var root string
			if rows.Scan(&root) == nil {
				if m, ok := levelmap.MarketOf(root); ok {
					want[m] = true
				}
			}
		}
		rows.Close()
	}
	if fed, err := h.Queries.ListLevelFeedMarkets(ctx, models.ListLevelFeedMarketsParams{UserID: userID(ctx), TradeDay: day}); err == nil {
		for _, m := range fed {
			want[m] = true
		}
	}
	var out []string
	for _, m := range levelmap.Markets {
		if want[m] {
			out = append(out, m)
		}
	}
	return out
}

func (h *Handlers) chicago() *time.Location {
	if loc, err := time.LoadLocation("America/Chicago"); err == nil {
		return loc
	}
	return h.localTZ()
}

// levelsAsOf is the moment a day's map is read at: now for today, the
// session's close for a past day.
func (h *Handlers) levelsAsOf(day string, now time.Time) time.Time {
	chicago := h.chicago()
	if levelmap.TradingDay(now, chicago) == day {
		return now
	}
	d, err := time.ParseInLocation("2006-01-02", day, chicago)
	if err != nil {
		return now
	}
	return d.Add(16 * time.Hour)
}

// levelMap builds (or recalls) the map of one market for a trading day.
func (h *Handlers) levelMap(ctx context.Context, market, day string, now time.Time) (*levelmap.Map, error) {
	uid := userID(ctx)
	key := fmt.Sprintf("%d|%s|%s", uid, market, day)
	h.levelsMu.Lock()
	if e, ok := h.levelsCache[key]; ok && now.Sub(e.at) < levelsCacheTTL {
		h.levelsMu.Unlock()
		return e.m, nil
	}
	h.levelsMu.Unlock()

	chicago := h.chicago()
	asOf := h.levelsAsOf(day, now)
	past := levelmap.TradingDay(now, chicago) != day
	// A past day: what was written at the time, when there is a record.
	if past {
		if row, err := h.Queries.GetLevelMap(ctx, models.GetLevelMapParams{UserID: uid, Market: market, TradeDay: day, Phase: "session"}); err == nil {
			var m levelmap.Map
			if json.Unmarshal([]byte(row.Data), &m) == nil && len(m.Levels) > 0 {
				h.rememberLevels(key, &m, now)
				return &m, nil
			}
		}
	}
	in := levelmap.Input{Market: market, Day: day, AsOf: asOf, Chicago: chicago}
	if inst, err := h.Queries.GetInstrument(ctx, market); err == nil && inst.TickSize != nil && *inst.TickSize > 0 {
		in.Tick = *inst.TickSize
	}
	if h.MarketData != nil {
		if sym, ok := marketdata.YahooSymbol(market); ok {
			if daily, err := h.MarketData.DailyBars(ctx, sym, asOf.AddDate(0, 0, -levelsDailyDays), asOf); err == nil {
				in.Daily = levelmap.DaysFromDaily(daily)
			} else if !errors.Is(err, marketdata.ErrNoData) {
				h.logger().Debug("levels: daily bars", "market", market, "err", err)
			}
			_ = h.MarketData.EnsureWindow(ctx, sym, asOf.AddDate(0, 0, -3), asOf)
			if bars, tf, err := h.MarketData.RecentBars(ctx, sym, levelsIntradayDays, asOf); err == nil && len(bars) > 0 {
				in.Intraday, in.IntradayTF = bars, string(tf)
				if tf.Duration() <= 5*time.Minute {
					in.Daily = levelmap.MergeDays(in.Daily, levelmap.TradingDays(bars, chicago))
				}
			}
		}
	}
	if row, err := h.Queries.LatestLevelFeed(ctx, models.LatestLevelFeedParams{UserID: uid, Market: market, TradeDay: day}); err == nil {
		var f levelmap.Feed
		if json.Unmarshal([]byte(row.Payload), &f) == nil {
			in.Feed = &f
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("levels: feed: %w", err)
	}
	if lv, at := h.gammaLevels(ctx, market, asOf, levelsGammaMaxAge); len(lv) > 0 {
		for _, g := range lv {
			in.Gamma = append(in.Gamma, levelmap.Level{Kind: "gamma", Name: g.Name, Price: g.Price, Source: levelmap.SourceGex})
		}
		in.GammaAt = at
	}
	m := levelmap.Build(in)
	h.rememberLevels(key, &m, now)
	if data, err := json.Marshal(m); err == nil {
		phases := []string{"session"}
		if !past && !h.rthOpenPassed(now) {
			phases = append(phases, "premarket")
		}
		for _, ph := range phases {
			_ = h.Queries.UpsertLevelMap(ctx, models.UpsertLevelMapParams{UserID: uid, Market: market, TradeDay: day, Phase: ph, AsOf: asOf.UTC(), Data: string(data)})
		}
	}
	return &m, nil
}

func (h *Handlers) rthOpenPassed(now time.Time) bool {
	lt := now.In(h.chicago())
	return lt.Hour()*60+lt.Minute() >= 8*60+30
}

func (h *Handlers) rememberLevels(key string, m *levelmap.Map, now time.Time) {
	h.levelsMu.Lock()
	if h.levelsCache == nil {
		h.levelsCache = map[string]levelsCacheEntry{}
	}
	h.levelsCache[key] = levelsCacheEntry{m: m, at: now}
	h.levelsMu.Unlock()
}

// forgetLevels drops a market's cached maps: a post arrived.
func (h *Handlers) forgetLevels(uid int64, market, day string) {
	h.levelsMu.Lock()
	delete(h.levelsCache, fmt.Sprintf("%d|%s|%s", uid, market, day))
	h.levelsMu.Unlock()
}

// levelsDay reads ?day= (today by default) as a trading day.
func (h *Handlers) levelsDay(r *http.Request, now time.Time) (string, bool) {
	day := r.URL.Query().Get("day")
	if day == "" {
		return levelmap.TradingDay(now, h.chicago()), true
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", false
	}
	return day, true
}

// ---- the page ---------------------------------------------------------------

// levelRow is one line of the page's table: a level with its distance.
type levelRow struct {
	levelmap.Level
	Distance float64 // points from the last price, positive above
	Ticks    float64
	ATRShare float64
	Above    bool
	Tier     int     // 1 strong, 2 medium, 3 weak: how the row is drawn
	ScorePct float64 // the score against the strongest level, 0 to 100
}

// Score tiers: a level at or above tierStrong is drawn heavy and bold, one
// under tierMedium light and muted. The scores are kind weight times
// timeframe weight plus touches (levelmap), so the prior day's extremes,
// the period extremes and a touched pivot sit in the first tier.
const (
	tierStrong = 5.0
	tierMedium = 2.5
)

func scoreTier(score float64) int {
	switch {
	case score >= tierStrong:
		return 1
	case score >= tierMedium:
		return 2
	}
	return 3
}

// ladderLine is one mark on the ladder: a level at a vertical position.
type ladderLine struct {
	Y      float64 // 0 at the top, 100 at the bottom (percent)
	LabelY float64 // where its label sits: pushed down when the one above is close
	Price  float64
	Name   string
	Kind   string
	Source string
	Score  float64
	Tier   int
	Above  bool
	Left   bool // the label on the left; sides alternate so neighbours do not overlap
}

// ladderLabelGap is the least distance between two labels on one side, in
// percent of the ladder's height (a label is about 14 px of 560).
const ladderLabelGap = 2.7

// ladderMax is how many levels the ladder draws: the strongest, plus the
// session's own marks whatever their score.
const ladderMax = 22

var ladderAlways = map[string]bool{"pdh": true, "pdl": true, "pdc": true, "onh": true, "onl": true, "orh": true, "orl": true, "vwap": true, "poc": true, "vah": true, "val": true}

// ladderBand is a zone on the ladder.
type ladderBand struct {
	Top, Height float64 // percent
	Score       float64
	Kinds       string
	Center      float64
}

// LevelsPage: GET /levels and /levels/{market}?day=
func (h *Handlers) LevelsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	day, ok := h.levelsDay(r, now)
	if !ok {
		http.Error(w, "bad day", http.StatusBadRequest)
		return
	}
	markets := h.levelsMarkets(ctx, day)
	market := r.PathValue("market")
	if market == "" {
		if len(markets) > 0 {
			market = markets[0]
		} else {
			market = "ES"
		}
	}
	if !levelmap.IsMarket(market) {
		http.NotFound(w, r)
		return
	}
	if !hasString(markets, market) {
		markets = append(markets, market)
	}
	today := levelmap.TradingDay(now, h.chicago())
	data := map[string]any{
		"NavActive": "levels",
		"Market":    market,
		"Markets":   markets,
		"Day":       day,
		"Today":     today,
		"IsToday":   day == today,
		"PrevDay":   prevTradingDay(day),
		"NextDay":   nextTradingDay(day),
		"BackHref":  validBackHref(r),
		"TZ":        h.localTZ().String(),
	}
	m, err := h.levelMap(ctx, market, day, now)
	if err != nil {
		h.serverError(w, err)
		return
	}
	data["Map"] = m
	data["HasMap"] = len(m.Levels) > 0
	rows := levelRows(m)
	data["Rows"] = rows
	data["Ladder"], data["Bands"] = ladder(m)
	data["TopZones"] = topZones(m, 8)
	data["LastAt"] = m.LastAt.In(h.localTZ()).Format("Mon 2 Jan 15:04")
	if m.Sources.ChartAt != nil {
		data["ChartAt"] = m.Sources.ChartAt.In(h.localTZ()).Format("15:04")
	}
	if m.Sources.GexAt != nil {
		data["GexAt"] = m.Sources.GexAt.In(h.localTZ()).Format("15:04")
	}
	data["PlanLines"] = planLines(m, 10)
	if page, err := json.Marshal(map[string]any{"market": market, "day": day, "dataURL": "/levels/data/" + market + "?day=" + day}); err == nil {
		data["PageJSON"] = template.JS(page) // a JSON island: not a JS string
	}
	h.renderPage(w, r, "levels.html", data)
}

func prevTradingDay(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	for {
		t = t.AddDate(0, 0, -1)
		if t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
			return t.Format("2006-01-02")
		}
	}
}

func nextTradingDay(day string) string {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day
	}
	for {
		t = t.AddDate(0, 0, 1)
		if t.Weekday() != time.Saturday && t.Weekday() != time.Sunday {
			return t.Format("2006-01-02")
		}
	}
}

func levelRows(m *levelmap.Map) []levelRow {
	tick := levelmap.DefaultTick(m.Market)
	maxScore := 0.0
	for _, l := range m.Levels {
		maxScore = math.Max(maxScore, l.Score)
	}
	rows := make([]levelRow, 0, len(m.Levels))
	for _, l := range m.Levels {
		r := levelRow{Level: l, Tier: scoreTier(l.Score)}
		if maxScore > 0 {
			r.ScorePct = math.Round(l.Score / maxScore * 100)
		}
		if m.Last > 0 {
			r.Distance = math.Round((l.Price-m.Last)*100) / 100
			r.Ticks = math.Round(r.Distance / tick)
			r.Above = r.Distance > 0
			if m.ATR > 0 {
				r.ATRShare = math.Round(r.Distance/m.ATR*100) / 100
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// ladder places the levels and zones on a 0..100 vertical scale around the
// last price: a band of 1.3 ATR each way, widened to hold the strongest
// zones.
func ladder(m *levelmap.Map) ([]ladderLine, []ladderBand) {
	if len(m.Levels) == 0 {
		return nil, nil
	}
	mid := m.Last
	if mid <= 0 {
		mid = m.Levels[len(m.Levels)/2].Price
	}
	half := 1.3 * m.ATR
	if half <= 0 {
		half = mid * 0.01
	}
	lo, hi := mid-half, mid+half
	y := func(p float64) float64 { return math.Round((hi-p)/(hi-lo)*1000) / 10 }
	var pool []levelmap.Level
	for _, l := range m.Levels {
		if l.Price >= lo && l.Price <= hi && l.Kind != "round" {
			pool = append(pool, l)
		}
	}
	sort.SliceStable(pool, func(i, j int) bool {
		ai, aj := ladderAlways[pool[i].Kind], ladderAlways[pool[j].Kind]
		if ai != aj {
			return ai
		}
		return pool[i].Score > pool[j].Score
	})
	if len(pool) > ladderMax {
		pool = pool[:ladderMax]
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].Price > pool[j].Price })
	var lines []ladderLine
	lastLabel := map[bool]float64{true: -100, false: -100}
	for i, l := range pool {
		left := i%2 == 1
		ly := y(l.Price)
		if ly-lastLabel[left] < ladderLabelGap {
			ly = lastLabel[left] + ladderLabelGap
		}
		lastLabel[left] = ly
		lines = append(lines, ladderLine{Y: y(l.Price), LabelY: ly, Price: l.Price, Name: l.Name, Kind: l.Kind, Source: l.Source, Score: l.Score, Tier: scoreTier(l.Score), Above: l.Price > mid, Left: left})
	}
	var bands []ladderBand
	for _, z := range m.Zones {
		if z.High < lo || z.Low > hi || len(z.Levels) < 2 {
			continue
		}
		top := y(math.Min(z.High, hi))
		bottom := y(math.Max(z.Low, lo))
		bands = append(bands, ladderBand{Top: top, Height: math.Max(bottom-top, 0.6), Score: z.Score, Kinds: strings.Join(z.Kinds, ", "), Center: math.Round(z.Center*100) / 100})
	}
	return lines, bands
}

// zoneRow is one of the strongest zones with its distance.
type zoneRow struct {
	levelmap.Zone
	Distance float64
	ATRShare float64
	Names    string
}

func topZones(m *levelmap.Map, n int) []zoneRow {
	var out []zoneRow
	for _, z := range m.Zones {
		if len(out) == n {
			break
		}
		r := zoneRow{Zone: z}
		if m.Last > 0 {
			r.Distance = math.Round((z.Center-m.Last)*100) / 100
			if m.ATR > 0 {
				r.ATRShare = math.Round(r.Distance/m.ATR*100) / 100
			}
		}
		names := make([]string, 0, len(z.Levels))
		for _, l := range z.Levels {
			if len(names) == 5 {
				names = append(names, fmt.Sprintf("+%d more", len(z.Levels)-5))
				break
			}
			names = append(names, l.Name)
		}
		r.Names = strings.Join(names, " · ")
		out = append(out, r)
	}
	return out
}

// planLines are the lines the map offers the day plan: the strongest
// zones, nearest first, one per line.
func planLines(m *levelmap.Map, n int) []string {
	zones := append([]levelmap.Zone(nil), m.Zones...)
	if len(zones) > n {
		zones = zones[:n]
	}
	sort.SliceStable(zones, func(i, j int) bool { return zones[i].Center > zones[j].Center })
	var out []string
	for _, z := range zones {
		names := make([]string, 0, len(z.Levels))
		for _, l := range z.Levels {
			if len(names) < 3 {
				names = append(names, l.Name)
			}
		}
		out = append(out, fmt.Sprintf("%s %s: %s", m.Market, fmtPrice(z.Center), strings.Join(names, ", ")))
	}
	return out
}

func fmtPrice(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

// LevelsData: GET /levels/data/{market}?day= - the map and the bars the
// page's charts draw (daily, weekly, 30-minute).
func (h *Handlers) LevelsData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	market := r.PathValue("market")
	day, ok := h.levelsDay(r, now)
	if !ok || !levelmap.IsMarket(market) {
		writeJSONError(w, http.StatusBadRequest, "unknown market or bad day")
		return
	}
	m, err := h.levelMap(ctx, market, day, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "levels unavailable")
		return
	}
	out := map[string]any{"map": m, "daily": []marketdata.Bar{}, "weekly": []marketdata.Bar{}, "intraday": []marketdata.Bar{}, "intradayTf": ""}
	if h.MarketData != nil {
		if sym, ok := marketdata.YahooSymbol(market); ok {
			asOf := h.levelsAsOf(day, now)
			if daily, err := h.MarketData.DailyBars(ctx, sym, asOf.AddDate(0, 0, -levelsDailyDays), asOf); err == nil {
				out["daily"] = daily
				out["weekly"] = weeklyBars(daily)
			}
			if bars, tf, err := h.MarketData.RecentBars(ctx, sym, 6, asOf); err == nil && len(bars) > 0 {
				show := marketdata.TF30m
				if tf.Duration() > show.Duration() {
					show = tf
				}
				out["intraday"] = marketdata.AggregateTo(bars, show)
				out["intradayTf"] = string(show)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// weeklyBars folds daily bars into ISO weeks.
func weeklyBars(daily []marketdata.Bar) []marketdata.Bar {
	var out []marketdata.Bar
	last := ""
	for _, b := range daily {
		t := time.Unix(b.Time, 0).UTC()
		y, wk := t.ISOWeek()
		k := fmt.Sprintf("%d-%02d", y, wk)
		if k != last {
			out = append(out, b)
			last = k
			continue
		}
		o := &out[len(out)-1]
		o.High, o.Low, o.Close = math.Max(o.High, b.High), math.Min(o.Low, b.Low), b.Close
		o.Volume += b.Volume
	}
	return out
}

// LevelsToPlan: POST /levels/{market}/plan - writes the map's strongest
// zones into the day plan's levels (appended under a dated line when the
// plan already names some), then opens the plan.
func (h *Handlers) LevelsToPlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	market := r.PathValue("market")
	if err := r.ParseForm(); err != nil || !levelmap.IsMarket(market) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	day := r.FormValue("day")
	if _, err := time.Parse("2006-01-02", day); err != nil {
		http.Error(w, "bad day", http.StatusBadRequest)
		return
	}
	m, err := h.levelMap(ctx, market, day, now)
	if err != nil {
		h.serverError(w, err)
		return
	}
	lines := planLines(m, 10)
	if len(lines) == 0 {
		http.Redirect(w, r, "/levels/"+market+"?day="+day, http.StatusSeeOther)
		return
	}
	uid := userID(ctx)
	var p dayplan.Plan
	if cur, err := dayplan.Get(ctx, h.Queries, uid, day); err == nil && cur != nil {
		p = *cur
	}
	p.Day = day
	block := strings.Join(lines, "\n")
	if strings.TrimSpace(p.Levels) == "" {
		p.Levels = block
	} else {
		p.Levels = strings.TrimSpace(p.Levels) + "\n\nFrom the map, " + now.In(h.localTZ()).Format("15:04") + ":\n" + block
	}
	if _, err := dayplan.Save(ctx, h.Queries, uid, p, now); err != nil {
		h.planFailed(w, err)
		return
	}
	h.LiveData.Bump(uid)
	http.Redirect(w, r, "/journal/"+day+"#day-plan", http.StatusSeeOther)
}
