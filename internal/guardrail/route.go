package guardrail

import (
	"net/url"
	"strings"
)

// RouteFor maps a nudge's web href onto the phone app's route, so the
// mapping lives in one place for the API and the push payload:
//
//	/journal/2026-10-15                          -> day/2026-10-15
//	/trades?status=open&sheet=pos:7:MESZ6        -> position/7/MESZ6
//	/trades?sheet=4412                           -> trade/4412
//	anything else                                -> today
func RouteFor(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return "today"
	}
	if strings.HasPrefix(u.Path, "/journal/") {
		if day := strings.TrimPrefix(u.Path, "/journal/"); len(day) == 10 && day[4] == '-' && day[7] == '-' {
			return "day/" + day
		}
	}
	if sheet := u.Query().Get("sheet"); sheet != "" {
		if strings.HasPrefix(sheet, "pos:") {
			rest := sheet[len("pos:"):]
			if i := strings.Index(rest, ":"); i > 0 && i < len(rest)-1 {
				return "position/" + rest[:i] + "/" + rest[i+1:]
			}
		} else if isDigits(sheet) {
			return "trade/" + sheet
		}
	}
	return "today"
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
