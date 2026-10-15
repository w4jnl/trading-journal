package push

import "testing"

// Defaults: nudges on, time-sensitive, with the app's sound; closed trades
// on and silent; the opt-in kind off until the phone sends it; validation
// refuses unknown kinds and sounds.
func TestPrefs(t *testing.T) {
	var none *Prefs
	if !none.Allows(KindNudge) || !none.TimeSensitive(KindNudge) || none.Sound(KindNudge) != SoundNudge {
		t.Errorf("nil prefs: nudge defaults wrong")
	}
	if !none.Allows(KindTradeClosed) || none.TimeSensitive(KindTradeClosed) || none.Sound(KindTradeClosed) != "" {
		t.Errorf("nil prefs: trade defaults wrong")
	}
	if none.Allows(KindPlanMissing) || none.Filtered(KindPlanMissing) != "planMissing is opt-in" {
		t.Errorf("planMissing must be opt-in")
	}
	off := false
	p := &Prefs{Kinds: map[string]KindPrefs{
		KindNudge:       {Off: true},
		KindTradeClosed: {Sound: "tj", TimeSensitive: &off},
		KindPlanMissing: {Sound: "default"},
	}}
	if p.Allows(KindNudge) || p.Filtered(KindNudge) != "nudge is off" {
		t.Errorf("nudge should be off")
	}
	if p.Sound(KindTradeClosed) != SoundTrade || p.TimeSensitive(KindTradeClosed) {
		t.Errorf("trade sound/sensitivity = %q %v", p.Sound(KindTradeClosed), p.TimeSensitive(KindTradeClosed))
	}
	if !p.Allows(KindPlanMissing) || p.Sound(KindPlanMissing) != "default" {
		t.Errorf("planMissing sent = %v sound %q", p.Allows(KindPlanMissing), p.Sound(KindPlanMissing))
	}
	if err := p.Validate(); err != nil {
		t.Errorf("valid prefs refused: %v", err)
	}
	if err := (&Prefs{Kinds: map[string]KindPrefs{"x": {}}}).Validate(); err == nil {
		t.Errorf("unknown kind accepted")
	}
	if err := (&Prefs{Kinds: map[string]KindPrefs{KindNudge: {Sound: "loud"}}}).Validate(); err == nil {
		t.Errorf("unknown sound accepted")
	}
}
