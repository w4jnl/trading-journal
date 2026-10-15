package guardrail

import "testing"

func TestRouteFor(t *testing.T) {
	for href, want := range map[string]string{
		"/journal/2026-10-15":                       "day/2026-10-15",
		"/trades?status=open&sheet=pos%3A7%3AMESZ6": "position/7/MESZ6",
		"/trades?status=open&sheet=pos:7:MESZ6":     "position/7/MESZ6",
		"/trades?sheet=4412":                        "trade/4412",
		"/trades?status=open&sheet=pos:7":           "today",
		"":                                          "today",
		"/journal/week/2026-W42":                    "today",
		positionHref(3, "NQZ6"):                     "position/3/NQZ6",
	} {
		if got := RouteFor(href); got != want {
			t.Errorf("RouteFor(%q) = %q, want %q", href, got, want)
		}
	}
}
