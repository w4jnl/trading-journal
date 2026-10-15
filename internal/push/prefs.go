// Package push sends the phone app what the server decides deserves the
// trader's attention: the guardrail's nudges as notifications, a closed
// trade as a quiet alert, a silent sync after any change, and the Session
// Live Activity. Devices register with their APNs token and their
// notification settings (Prefs); the server filters before each push, since
// iOS cannot drop a push once it arrived. The APNs client is in the apns
// subpackage.
package push

import "fmt"

// Prefs is what one phone wants pushed. The app owns them (its notification
// settings screen) and sends them whole, with the registration and on
// every change; the server only reads them.
type Prefs struct {
	// Kinds by push kind; a missing kind has its defaults.
	Kinds map[string]KindPrefs `json:"kinds,omitempty"`
	// NoActivity: no Session activity in the Dynamic Island / on the Lock
	// Screen for this phone.
	NoActivity bool `json:"noActivity,omitempty"`
	// FinishedAsNotification keeps a closed trade a notification while the
	// Session activity shows; false: it rides the activity as its alert.
	FinishedAsNotification bool `json:"finishedAsNotification,omitempty"`
	// QuietOutsideRth drops the closed-trade alerts outside regular hours.
	QuietOutsideRth bool `json:"quietOutsideRth,omitempty"`
}

// KindPrefs are the settings of one push kind; the zero value is the
// default.
type KindPrefs struct {
	Off           bool   `json:"off,omitempty"`
	Sound         string `json:"sound,omitempty"`         // "" or "tj": the app's sound; "default": the system's; "none": silent
	TimeSensitive *bool  `json:"timeSensitive,omitempty"` // nil: on for nudges, off otherwise
}

// Push kinds a phone filters on.
const (
	KindNudge       = "nudge"       // a guardrail nudge: needs the trader
	KindTradeClosed = "tradeClosed" // a trade closed: passive
	KindPlanMissing = "planMissing" // the open is near and no plan is written (opt-in)
)

// Sounds the app bundles, named in the push.
const (
	SoundNudge = "tj-nudge.caf"
	SoundTrade = "tj-trade.caf"
)

var knownKinds = map[string]bool{KindNudge: true, KindTradeClosed: true, KindPlanMissing: true}

// Allows reports whether a push of kind goes to the phone. Nil prefs allow
// everything but the opt-in kinds.
func (p *Prefs) Allows(kind string) bool { return p.Filtered(kind) == "" }

// Filtered names why the phone does not want a push of kind ("" = it
// does), for the log.
func (p *Prefs) Filtered(kind string) string {
	var k KindPrefs
	set := false
	if p != nil {
		k, set = p.Kinds[kind]
	}
	if kind == KindPlanMissing && !set {
		return kind + " is opt-in"
	}
	if k.Off {
		return kind + " is off"
	}
	return ""
}

// Sound is the payload's sound for a kind: the app's file, "default", or
// "" for none. Closed trades are silent unless asked.
func (p *Prefs) Sound(kind string) string {
	var k KindPrefs
	if p != nil {
		k = p.Kinds[kind]
	}
	switch k.Sound {
	case "none":
		return ""
	case "default":
		return "default"
	case "tj":
		if kind == KindTradeClosed {
			return SoundTrade
		}
		return SoundNudge
	}
	if kind == KindNudge || kind == KindPlanMissing {
		return SoundNudge
	}
	return ""
}

// TimeSensitive reports whether a kind's push breaks through Focus.
func (p *Prefs) TimeSensitive(kind string) bool {
	if p != nil {
		if v := p.Kinds[kind].TimeSensitive; v != nil {
			return *v
		}
	}
	return kind == KindNudge
}

// Validate bounds what a phone may store: known kinds and sounds.
func (p *Prefs) Validate() error {
	if p == nil {
		return nil
	}
	for kind, k := range p.Kinds {
		if !knownKinds[kind] {
			return fmt.Errorf("unknown push kind %q", kind)
		}
		switch k.Sound {
		case "", "tj", "default", "none":
		default:
			return fmt.Errorf("unknown sound %q for %s (tj, default or none)", k.Sound, kind)
		}
	}
	return nil
}
