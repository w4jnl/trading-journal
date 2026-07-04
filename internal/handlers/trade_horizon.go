package handlers

import (
	"fmt"
	"time"
)

// tradeHorizon classifies a closed trade by how long the position was held (exit - entry),
// a purely derived label (trades are rebuilt from executions, so nothing is stored):
//
//	Scalp     < 10 minutes
//	Intraday  < 1 day
//	Swing     < 14 days
//	Long-term >= 14 days
//
// An open trade (no exit_time) has no horizon yet, so it returns "".
func tradeHorizon(entry time.Time, exit *time.Time) string {
	if exit == nil {
		return ""
	}
	switch d := exit.Sub(entry); {
	case d < 10*time.Minute:
		return "Scalp"
	case d < 24*time.Hour:
		return "Intraday"
	case d < 14*24*time.Hour:
		return "Swing"
	default:
		return "Long-term"
	}
}

// heldFor renders the holding time compactly for the horizon pill's tooltip:
// "45s", "8m", "3h 20m", "4d 2h". Empty for an open trade.
func heldFor(entry time.Time, exit *time.Time) string {
	if exit == nil {
		return ""
	}
	d := exit.Sub(entry)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh %dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		if h := int(d.Hours()) % 24; h != 0 {
			return fmt.Sprintf("%dd %dh", int(d.Hours())/24, h)
		}
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
