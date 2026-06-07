package marketdata

import "strings"

// yahooSymbols maps an instruments.symbol_root to a Yahoo continuous
// front-month futures symbol. Micros map to the full-size series because the
// price is identical (only the contract multiplier differs); we never chart a
// micro separately. Roots not present here have no chart - the handler renders
// a "no market data" note and never makes a network call.
var yahooSymbols = map[string]string{
	"ES":  "ES=F",
	"EP":  "ES=F", // CQG / AMP-CQG feed labels the E-mini S&P "EP" (same as ES)
	"MES": "ES=F",
	"NQ":  "NQ=F",
	"MNQ": "NQ=F",
	"CL":  "CL=F",
	"MCL": "CL=F",
	"GC":  "GC=F",
	"MGC": "GC=F",
	"YM":  "YM=F",
	"MYM": "YM=F",
	"RTY": "RTY=F",
	"M2K": "RTY=F",
}

// YahooSymbol returns the Yahoo symbol for an instrument root and whether a
// mapping exists.
func YahooSymbol(root string) (string, bool) {
	s, ok := yahooSymbols[strings.ToUpper(strings.TrimSpace(root))]
	return s, ok
}
