package handlers

import (
	"testing"
	"time"
)

func TestTradeHorizon(t *testing.T) {
	entry := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := entry.Add(d); return &x }
	cases := []struct {
		name string
		exit *time.Time
		want string
	}{
		{"open (no exit)", nil, ""},
		{"5 min", at(5 * time.Minute), "Scalp"},
		{"just under 10m", at(10*time.Minute - time.Second), "Scalp"},
		{"exactly 10m -> Intraday", at(10 * time.Minute), "Intraday"},
		{"3 hours", at(3 * time.Hour), "Intraday"},
		{"just under 1 day", at(24*time.Hour - time.Minute), "Intraday"},
		{"exactly 1 day -> Swing", at(24 * time.Hour), "Swing"},
		{"4 days", at(4 * 24 * time.Hour), "Swing"},
		{"just under 14 days", at(14*24*time.Hour - time.Hour), "Swing"},
		{"14 days -> Long-term", at(14 * 24 * time.Hour), "Long-term"},
		{"30 days", at(30 * 24 * time.Hour), "Long-term"},
	}
	for _, c := range cases {
		if got := tradeHorizon(entry, c.exit); got != c.want {
			t.Errorf("%s: tradeHorizon = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHeldFor(t *testing.T) {
	entry := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := entry.Add(d); return &x }
	cases := []struct {
		exit *time.Time
		want string
	}{
		{nil, ""},
		{at(45 * time.Second), "45s"},
		{at(8 * time.Minute), "8m"},
		{at(3 * time.Hour), "3h"},
		{at(3*time.Hour + 20*time.Minute), "3h 20m"},
		{at(4 * 24 * time.Hour), "4d"},
		{at(4*24*time.Hour + 2*time.Hour), "4d 2h"},
	}
	for _, c := range cases {
		if got := heldFor(entry, c.exit); got != c.want {
			t.Errorf("heldFor(%v) = %q, want %q", c.exit, got, c.want)
		}
	}
}
