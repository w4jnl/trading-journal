package levelmap

// DefaultTick is the tick of the market's full-size contract when the
// instruments table does not say.
func DefaultTick(market string) float64 {
	switch market {
	case "ES", "NQ":
		return 0.25
	case "CL":
		return 0.01
	case "GC", "RTY":
		return 0.1
	case "YM":
		return 1
	}
	return 0.01
}

// MarketOf maps an instrument root to the market the map is keyed by: the
// full-size root, micros folded (MES -> ES), CQG's EP -> ES.
func MarketOf(root string) (string, bool) {
	switch root {
	case "ES", "MES", "EP":
		return "ES", true
	case "NQ", "MNQ":
		return "NQ", true
	case "CL", "MCL":
		return "CL", true
	case "GC", "MGC":
		return "GC", true
	case "YM", "MYM":
		return "YM", true
	case "RTY", "M2K":
		return "RTY", true
	}
	return "", false
}

// Markets the map knows, in display order.
var Markets = []string{"ES", "NQ", "YM", "RTY", "CL", "GC"}

// IsMarket says whether the string is one of them.
func IsMarket(m string) bool {
	for _, k := range Markets {
		if k == m {
			return true
		}
	}
	return false
}

// kindWeight ranks what a level is, before touches and timeframe.
func kindWeight(kind string) float64 {
	switch kind {
	case "pdh", "pdl":
		return 3
	case "pdc":
		return 2
	case "onh", "onl":
		return 2.5
	case "orh", "orl":
		return 2
	case "poc":
		return 2.5
	case "vah", "val":
		return 2
	case "vwap":
		return 2
	case "vwap_band":
		return 1
	case "atr":
		return 1.5
	case "pivot":
		return 1.5
	case "camarilla":
		return 1
	case "period":
		return 2.5
	case "swing":
		return 1.5
	case "gap":
		return 2
	case "round":
		return 0.75
	case "gamma":
		return 2
	}
	return 1
}

// tfWeight ranks the timeframe a level was read from.
func tfWeight(tf string) float64 {
	switch tf {
	case "30m":
		return 0.8
	case "1h":
		return 1
	case "4h":
		return 1.3
	case "1d":
		return 1.5
	case "1w":
		return 2
	case "1mo":
		return 2.5
	}
	return 1
}

// multWeight: the whole-ATR band counts more than the quarter.
func multWeight(k float64) float64 {
	switch {
	case k >= 1:
		return 1.5
	case k >= 0.5:
		return 1.2
	}
	return 0.8
}
