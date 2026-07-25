package mwmail

import (
	"bytes"
	"io"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

// Result is what the provisional parser extracted from one captured message.
// Parsed is true only when side, qty and price were all found - the minimum
// for a fill to be actionable later.
type Result struct {
	From    string
	To      string
	Date    string // Date header verbatim
	Subject string
	Body    string

	Parsed      bool
	Side        string // "buy" | "sell"
	Qty         float64
	Symbol      string
	Price       float64
	Account     string
	OrderID     string
	FillTimeRaw string
}

// PROVISIONAL: the exact MotiveWave notification format is unknown until real
// samples are captured (that is what the capture phase is for). This parser
// is a tolerant best-effort extractor over subject+body, tuned against
// synthetic fixtures; every captured real .eml becomes a fixture and the
// patterns get tightened then. Treat its output as advisory.
var (
	// BUY/SELL/BOT/SLD/BOUGHT/SOLD as a standalone word, optionally followed
	// by the quantity ("BOT 2", "Bought 10").
	reSideQty = regexp.MustCompile(`(?i)\b(BUY|SELL|BOT|SLD|BOUGHT|SOLD)\b[:\s]*([0-9][0-9,]*(?:\.\d+)?)?`)
	// Bare quantity qualifiers when the side word did not carry one.
	reQty = regexp.MustCompile(`(?i)\b(?:qty|quantity|filled)[:\s]+([0-9][0-9,]*(?:\.\d+)?)\b`)
	// Futures contract code: 1-4 root letters + month code + 1-2 digit year.
	// \b alone is not enough on the right (a trailing digit of a 2-digit year
	// is still a word char), so require a non-alnum boundary via lookahead-free
	// grouping in FindAllString + manual filtering below.
	reSymbol = regexp.MustCompile(`\b([A-Z]{1,4}[FGHJKMNQUVXZ]\d{1,2})\b`)
	// Price: "@ 6,852.25", "price: 6852.25", "avg fill price 6852.25".
	rePriceAt   = regexp.MustCompile(`@\s*([0-9][0-9,]*(?:\.\d+)?)`)
	rePriceWord = regexp.MustCompile(`(?i)\b(?:avg\.?\s+fill\s+price|fill\s+price|price)[:\s]+([0-9][0-9,]*(?:\.\d+)?)`)
	// Account: "Account: APEX-..." / "Acct: 120042".
	reAccount = regexp.MustCompile(`(?i)\b(?:account|acct)[.:\s]+([A-Za-z0-9._-]+)`)
	// Order id requires an explicit qualifier AND at least one digit in the
	// token - a bare "order" qualifier would happily match "Order Filled".
	reOrderID = regexp.MustCompile(`(?i)\border\s*(?:id|number|no\.?|#)[:\s]*#?\s*([A-Za-z0-9-]*\d[A-Za-z0-9-]*)`)
	// Fill time: "Time: 09:31:02", "Filled at 2026-07-21 09:31:02", or an
	// ISO-ish/clock-ish token after "at".
	reFillTime = regexp.MustCompile(`(?i)\b(?:time|filled\s+at|at)[:\s]+([0-9][0-9:/\- .APMapm]{4,30})`)

	// Known roots for the fallback when no dated contract code appears.
	knownRoots = []string{"MES", "MNQ", "MCL", "MGC", "MYM", "M2K", "ES", "NQ", "CL", "GC", "YM", "RTY", "ZB", "ZN", "6E"}
)

// Parse runs the provisional extractor on a raw RFC-5322 message.
func Parse(raw []byte) Result {
	var res Result

	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	var body string
	if err == nil {
		res.From = msg.Header.Get("From")
		res.To = msg.Header.Get("To")
		res.Date = msg.Header.Get("Date")
		res.Subject = msg.Header.Get("Subject")
		b, _ := io.ReadAll(io.LimitReader(msg.Body, 1<<20))
		body = string(b)
	} else {
		// Not valid RFC-5322 (or headerless) - scan the whole thing.
		body = string(raw)
	}
	res.Body = body

	// Subject first, then body: MW-style notifications usually put the
	// essentials in the subject line.
	text := res.Subject + "\n" + body

	if m := reSideQty.FindStringSubmatch(text); m != nil {
		switch strings.ToUpper(m[1]) {
		case "BUY", "BOT", "BOUGHT":
			res.Side = "buy"
		default:
			res.Side = "sell"
		}
		if m[2] != "" {
			res.Qty = parseNum(m[2])
		}
	}
	if res.Qty == 0 {
		if m := reQty.FindStringSubmatch(text); m != nil {
			res.Qty = parseNum(m[1])
		}
	}

	if m := rePriceAt.FindStringSubmatch(text); m != nil {
		res.Price = parseNum(m[1])
	} else if m := rePriceWord.FindStringSubmatch(text); m != nil {
		res.Price = parseNum(m[1])
	}

	res.Symbol = findSymbol(text)
	if m := reAccount.FindStringSubmatch(text); m != nil {
		res.Account = m[1]
	}
	if m := reOrderID.FindStringSubmatch(text); m != nil {
		res.OrderID = m[1]
	}
	if m := reFillTime.FindStringSubmatch(text); m != nil {
		res.FillTimeRaw = strings.TrimSpace(m[1])
	}

	res.Parsed = res.Side != "" && res.Qty > 0 && res.Price > 0
	return res
}

// monthWords are calendar-month abbreviations that the futures-code pattern
// can false-positive on ("JUN26" parses as root JU + month N + year 26).
var monthWords = map[string]bool{
	"JAN": true, "FEB": true, "MAR": true, "APR": true, "MAY": true, "JUN": true,
	"JUL": true, "AUG": true, "SEP": true, "OCT": true, "NOV": true, "DEC": true,
}

// findSymbol prefers a dated futures code; falls back to a known bare root.
func findSymbol(text string) string {
	for _, cand := range reSymbol.FindAllStringSubmatch(text, -1) {
		sym := cand[1]
		if len(sym) >= 3 && !monthWords[sym[:3]] {
			return sym
		}
	}
	upper := strings.ToUpper(text)
	for _, root := range knownRoots {
		if regexp.MustCompile(`\b` + root + `\b`).MatchString(upper) {
			return root
		}
	}
	return ""
}

func parseNum(s string) float64 {
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return 0
	}
	return v
}
