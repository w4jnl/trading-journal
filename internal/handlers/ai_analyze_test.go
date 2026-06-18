package handlers

import (
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
)

func TestParseAIVerdict(t *testing.T) {
	cases := []struct {
		in    string
		tag   string
		strat string
	}{
		{"analysis…\n\n<!-- tj-ai:verdict tag=good strategy=12 -->", "good", "12"},
		{"<!-- tj-ai:verdict tag=bad strategy=none -->", "bad", "none"},
		{"<!-- tj-ai:verdict strategy=5 tag=neutral -->", "neutral", "5"},             // order-independent
		{`<!-- tj-ai:verdict tag=GOOD strategy="7" -->`, "good", "7"},                 // case + quotes
		{"<!-- tj-ai:verdict tag=good strategy=ORB VWAP -->", "good", "ORB VWAP"},     // NAME with a space
		{"<!-- tj-ai:verdict strategy=VWAP Bounce tag=bad -->", "bad", "VWAP Bounce"}, // name first
		{"no verdict line here", "", ""},
		{"<!-- tj-ai:verdict tag=weird strategy=abc -->", "", "abc"}, // bad tag dropped; raw strategy kept
	}
	for _, c := range cases {
		v := parseAIVerdict(c.in)
		if v.Tag != c.tag || v.Strategy != c.strat {
			t.Errorf("parseAIVerdict(%q) = {tag:%q strategy:%q}, want {tag:%q strategy:%q}", c.in, v.Tag, v.Strategy, c.tag, c.strat)
		}
	}
}

func TestResolveStrategyID(t *testing.T) {
	strats := []models.Strategy{{ID: 1, Name: "ORB VWAP"}, {ID: 2, Name: "VWAP Bounce"}}
	cases := []struct {
		raw  string
		want int64
	}{
		{"1", 1},
		{"2", 2},
		{"ORB VWAP", 1},    // exact name
		{"vwap bounce", 2}, // case-insensitive name
		{"none", 0},
		{"", 0},
		{"99", 0},               // numeric but not owned
		{"Unknown Strategy", 0}, // no name match
	}
	for _, c := range cases {
		if got := resolveStrategyID(c.raw, strats); got != c.want {
			t.Errorf("resolveStrategyID(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestStripAIVerdict(t *testing.T) {
	if got := stripAIVerdict("Analysis line.\n\n<!-- tj-ai:verdict tag=good strategy=1 -->"); got != "Analysis line." {
		t.Errorf("stripAIVerdict = %q, want %q", got, "Analysis line.")
	}
	if got := stripAIVerdict("no verdict"); got != "no verdict" {
		t.Errorf("stripAIVerdict left text unchanged = %q", got)
	}
}
