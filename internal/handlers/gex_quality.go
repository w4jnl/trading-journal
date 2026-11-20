package handlers

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
// it is fine or cannot be asked. Rendered next to the "session closed" and
// "unreachable" banners so an outage reads as an outage; the engine's text
// is condensed first (condenseEngineNotes), the raw lines go to the debug
// log and the banner's title.
func (h *Handlers) gexHealthNotes(ctx context.Context) (notes, raw []string) {
	if h.GEX == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, gexHealthTimeout)
	defer cancel()
	hl, err := h.GEX.Health(ctx)
	if err != nil {
		return nil, nil
	}
	if hl.Status == "ok" && len(hl.Reasons) == 0 {
		return nil, nil
	}
	raw = make([]string, 0, len(hl.Reasons)+len(hl.Notices)+1)
	if hl.Status != "" && hl.Status != "ok" {
		raw = append(raw, "status "+hl.Status)
	}
	raw = append(raw, hl.Reasons...)
	raw = append(raw, hl.Notices...)
	notes = condenseEngineNotes(raw)
	if strings.Join(notes, "; ") != strings.Join(raw, "; ") {
		h.logger().Debug("gex health notes condensed", "raw", raw)
	}
	return notes, raw
}

// The engine relays its scheduler's failure as the exception's own text,
// so one "cycle failing" reason can carry four fetch errors, each with the
// vendor URL and a Mozilla link, plus a per-market line for every market
// (engine v0.12: "ES: newest snapshot 62804s old", "NQ: no marks"). The
// banner wants one clause per fact, markets grouped, and ages a trader can
// read.
var (
	// A Python exception repr around a quoted message: RuntimeError('...'),
	// HTTPStatusError("..."), ReadTimeout('...'). The quote's own escapes
	// stay inside the match.
	engineExceptionRe = regexp.MustCompile(`\b[A-Z][A-Za-z]*\(("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')\)`)
	// httpx: Client error '403 Forbidden' for url 'https://...'
	engineHTTPErrorRe = regexp.MustCompile(`(?i)^(?:client|server) error '([^']*)'(?: for url '[^']*')?\s*$`)
	engineURLRe       = regexp.MustCompile(`\s*https?://\S+`)
	// "<label>: <MARKET>: <detail>" after any wrapper prefix the engine put
	// in front ("cycle produced no market views: fetch failed: NDX: ...").
	engineMarketItemRe = regexp.MustCompile(`^(?:.*?: )??(?:([a-z][a-z ]*[a-z]): )?([A-Z][A-Z0-9]{1,5}): (.+)$`)
	engineAgeRe        = regexp.MustCompile(`\b(\d+)s old\b`)
)

// condenseEngineNotes shortens the /health lines for the banner: exception
// wrappers and URLs go, per-market items with the same fact are grouped
// ("fetch failed (403 Forbidden): NDX, QQQ, SPX, SPY"), seconds become
// minutes or hours. A line it does not recognise passes through unchanged,
// so a new engine message still reaches the trader.
func condenseEngineNotes(raw []string) []string {
	type group struct {
		key     string
		markets []string
	}
	var (
		out    []string
		groups []*group
		byKey  = map[string]*group{}
		seen   = map[string]bool{}
	)
	flush := func() {
		for _, g := range groups {
			out = append(out, g.key+": "+strings.Join(g.markets, ", "))
		}
		groups, byKey = nil, map[string]*group{}
	}
	add := func(line string) {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			return
		}
		seen[line] = true
		out = append(out, line)
	}
	for _, line := range raw {
		for _, item := range strings.Split(unwrapEngineExceptions(line), "; ") {
			item = humanizeEngineAges(strings.TrimSpace(item))
			m := engineMarketItemRe.FindStringSubmatch(item)
			if m == nil {
				flush()
				add(item)
				continue
			}
			label, market, detail := m[1], m[2], shortenEngineDetail(m[3])
			key := detail
			if label != "" {
				key = label + " (" + detail + ")"
			}
			g := byKey[key]
			if g == nil {
				g = &group{key: key}
				byKey[key] = g
				groups = append(groups, g)
			}
			g.markets = append(g.markets, market)
		}
	}
	flush()
	return out
}

// unwrapEngineExceptions replaces every Name('message') call with its
// message, outermost first, and drops anything after the message's first
// line break (httpx appends a "For more information" link there).
func unwrapEngineExceptions(s string) string {
	for range 32 { // one wrapper per failed fetch, plus the scheduler's own
		m := engineExceptionRe.FindStringSubmatchIndex(s)
		if m == nil {
			break
		}
		inner := s[m[2]+1 : m[3]-1]
		r := strings.NewReplacer(`\'`, "'", `\"`, `"`, `\n`, "\n", `\\`, `\`)
		inner = r.Replace(inner)
		if i := strings.IndexByte(inner, '\n'); i >= 0 {
			inner = inner[:i]
		}
		s = s[:m[0]] + strings.TrimSpace(inner) + s[m[1]:]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// shortenEngineDetail turns an httpx status line into its status and drops
// URLs from anything else.
func shortenEngineDetail(d string) string {
	d = strings.TrimSpace(d)
	if m := engineHTTPErrorRe.FindStringSubmatch(d); m != nil {
		return m[1]
	}
	return strings.TrimSpace(engineURLRe.ReplaceAllString(d, ""))
}

// humanizeEngineAges rewrites "62804s old" as "17h old" (minutes under an
// hour, seconds under two minutes), so the per-market lines group.
func humanizeEngineAges(s string) string {
	return engineAgeRe.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.Atoi(engineAgeRe.FindStringSubmatch(m)[1])
		if err != nil {
			return m
		}
		switch {
		case n >= 3600:
			return fmt.Sprintf("%dh old", (n+1800)/3600)
		case n >= 120:
			return fmt.Sprintf("%dm old", (n+30)/60)
		}
		return m
	})
}
