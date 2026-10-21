package aimethod

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ---- The trader profile -------------------------------------------------
//
// The skills are code; every trader tunes how they are applied through a
// profile: their style, the evidence they weigh most, their markets and
// free-text house rules. It renders as one cached block in every prompt,
// travels to every MCP client, and sizes the evidence the app sends.

// Profile is the trader's tuning of the method.
type Profile struct {
	Style      string   `json:"style,omitempty"`      // scalper | daytrader | swing
	Focus      []string `json:"focus,omitempty"`      // ranked FocusKeys
	Markets    []string `json:"markets,omitempty"`    // the roots the trader works
	HouseRules string   `json:"houseRules,omitempty"` // standing instructions, free text
}

// Styles are the trading styles, in display order.
var Styles = []struct{ Key, Label, Horizon string }{
	{"scalper", "Scalper", "minutes: the entry against the last bars, the exit against the next move; one-minute bars matter most, the daily context is background"},
	{"daytrader", "Day trader", "the session: the entry against the session's structure, the exit against the day's range; five and fifteen-minute bars matter most"},
	{"swing", "Swing trader", "days: the entry against the daily and weekly structure; intraday bars are detail, the daily and weekly context lead"},
}

// FocusKeys are the evidence families a trader can rank, in display order.
var FocusKeys = []struct{ Key, Label, Means string }{
	{"levels", "Short-term levels", "the day map's zones: prior day and overnight extremes, the opening range, pivots, swings"},
	{"session", "Session structure", "the prior RTH, the overnight, the opening range, VWAP, the bars around the trade"},
	{"volume", "Volume profile", "the prior day's point of control and value area from the chart feed"},
	{"gamma", "Gamma map", "the dealer-gamma walls, the flip and the expected move"},
	{"coach", "Coach checks", "the rule-based findings: timing, size, heat against the stop, what the exit kept"},
	{"playbooks", "Playbooks", "the strategies' rules and the checklist, followed or broken"},
}

const (
	DefaultStyle  = "daytrader"
	houseRulesMax = 2000
)

// ParseProfile reads the stored JSON; empty or broken input is the default
// profile.
func ParseProfile(raw string) Profile {
	var p Profile
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	p.normalize()
	return p
}

// Encode is the profile as stored.
func (p Profile) Encode() string {
	p.normalize()
	if p.IsDefault() {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

// Validate refuses unknown keys and an overlong rules text.
func (p *Profile) Validate() error {
	if p.Style != "" && !validStyle(p.Style) {
		return fmt.Errorf("unknown style %q", p.Style)
	}
	for _, f := range p.Focus {
		if !validFocus(f) {
			return fmt.Errorf("unknown focus %q", f)
		}
	}
	if utf8.RuneCountInString(p.HouseRules) > houseRulesMax {
		return fmt.Errorf("house rules over %d characters", houseRulesMax)
	}
	p.normalize()
	return nil
}

func (p *Profile) normalize() {
	if !validStyle(p.Style) {
		p.Style = DefaultStyle
	}
	seen := map[string]bool{}
	var focus []string
	for _, f := range p.Focus {
		if validFocus(f) && !seen[f] {
			seen[f] = true
			focus = append(focus, f)
		}
	}
	p.Focus = focus
	var markets []string
	seen = map[string]bool{}
	for _, m := range p.Markets {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" && !seen[m] {
			seen[m] = true
			markets = append(markets, m)
		}
	}
	sort.Strings(markets)
	p.Markets = markets
	p.HouseRules = strings.TrimSpace(p.HouseRules)
}

// IsDefault says whether the profile adds nothing to the method.
func (p Profile) IsDefault() bool {
	return p.Style == DefaultStyle && len(p.Focus) == 0 && len(p.Markets) == 0 && p.HouseRules == ""
}

func validStyle(s string) bool {
	for _, st := range Styles {
		if st.Key == s {
			return true
		}
	}
	return false
}

func validFocus(f string) bool {
	for _, k := range FocusKeys {
		if k.Key == f {
			return true
		}
	}
	return false
}

// Block renders the profile as the prose block every prompt carries; empty
// for the default profile.
func (p Profile) Block() string {
	p.normalize()
	if p.IsDefault() {
		return ""
	}
	var b strings.Builder
	b.WriteString("THE TRADER: ")
	for _, st := range Styles {
		if st.Key == p.Style {
			b.WriteString("a " + strings.ToLower(st.Label) + "; judge entries and exits on that horizon (" + st.Horizon + ").")
		}
	}
	if len(p.Focus) > 0 {
		names := make([]string, 0, len(p.Focus))
		for _, f := range p.Focus {
			for _, k := range FocusKeys {
				if k.Key == f {
					names = append(names, strings.ToLower(k.Label)+" ("+k.Means+")")
				}
			}
		}
		b.WriteString(" They weigh this evidence most, in this order: " + strings.Join(names, "; ") + ". Lead with it; the rest is context.")
	}
	if len(p.Markets) > 0 {
		b.WriteString(" Their markets: " + strings.Join(p.Markets, ", ") + ".")
	}
	if p.HouseRules != "" {
		b.WriteString("\n\nTHE TRADER'S STANDING INSTRUCTIONS (follow them where they do not contradict the grounding rules or the machine-readable lines):\n" + p.HouseRules)
	}
	return b.String()
}

// Evidence sizes what the app sends with a review for this profile.
type Evidence struct {
	Bars15m, Bars5m, Bars1m int
	GammaBrief              bool // the gamma map as one line rather than every level
}

// Plan is the evidence plan for the profile.
func (p Profile) Plan() Evidence {
	p.normalize()
	e := Evidence{Bars15m: 52, Bars5m: 24, Bars1m: 20}
	switch p.Style {
	case "scalper":
		e = Evidence{Bars15m: 26, Bars5m: 36, Bars1m: 45}
	case "swing":
		e = Evidence{Bars15m: 78, Bars5m: 12, Bars1m: 0}
	}
	if len(p.Focus) > 0 && !p.weighs("gamma") {
		e.GammaBrief = true
	}
	return e
}

func (p Profile) weighs(key string) bool {
	for _, f := range p.Focus {
		if f == key {
			return true
		}
	}
	return false
}
