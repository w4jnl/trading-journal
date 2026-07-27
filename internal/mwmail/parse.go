package mwmail

import (
	"bytes"
	"io"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Result is what the parser extracted from one captured message.
// Parsed is true only when side, qty and price were all found - the minimum
// for a fill to be actionable later.
type Result struct {
	From    string
	To      string
	Date    string // Date header verbatim
	Subject string
	Body    string

	Parsed      bool
	IsTest      bool   // MW "Test Email" from the Email Settings panel
	Side        string // "buy" | "sell"
	Qty         float64
	Symbol      string
	Price       float64
	Account     string
	OrderID     string
	Broker      string // "Broker:" label, e.g. "Rithmic WS" - the source hint for firm guessing / symbol aliasing
	FillTimeRaw string
	// ExecTime is FillTimeRaw resolved to UTC. MW writes the execution time
	// without year or zone ("Jul-27 09:04:52 AM"); both come from the Date
	// header. Zero when underivable.
	ExecTime time.Time
}

// IngestReady reports whether the message carries everything the provisional
// ingest needs: the fill triple plus the account / order-id join keys and a
// resolvable execution time.
func (r Result) IngestReady() bool {
	return r.Parsed && !r.IsTest && r.Account != "" && r.OrderID != "" &&
		r.Symbol != "" && !r.ExecTime.IsZero()
}

// The real MotiveWave notification (verified against live captures) is a
// labeled key-value block inside a single text/plain MIME part:
//
//	Execution Time: Jul-27 09:04:52 AM
//	Broker: Rithmic WS
//	Account ID: LFE050-EXAMPLE2-TEST019
//	Order ID: 172408392
//	Order Type: LMT
//	Symbol: ESU6
//	Buy/Sell: SELL
//	Quantity: 1
//	Avg Fill Price: 7515.50
//	Time In Force: DAY
//
// parseLabels reads that shape line by line; the regex heuristics below
// remain as a FALLBACK for fields the labels did not yield (unknown MW
// versions / other notification kinds), so the extractor degrades instead
// of failing outright.
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
	res.IsTest = strings.Contains(res.Subject, "MotiveWave Test Email")

	// Labeled pass first - the authoritative shape of real MW fill mails.
	parseLabels(body, &res)

	// Heuristic fallback over subject+body for whatever the labels missed.
	// MW-style notifications usually put the essentials in the subject line.
	text := res.Subject + "\n" + body

	if res.Side == "" || res.Qty == 0 {
		applySideQtyHeuristics(text, &res)
	}
	if res.Price == 0 {
		if m := rePriceAt.FindStringSubmatch(text); m != nil {
			res.Price = parseNum(m[1])
		} else if m := rePriceWord.FindStringSubmatch(text); m != nil {
			res.Price = parseNum(m[1])
		}
	}
	if res.Symbol == "" {
		res.Symbol = findSymbol(text)
	}
	if res.Account == "" {
		if m := reAccount.FindStringSubmatch(text); m != nil {
			res.Account = m[1]
		}
	}
	if res.OrderID == "" {
		if m := reOrderID.FindStringSubmatch(text); m != nil {
			res.OrderID = m[1]
		}
	}
	if res.FillTimeRaw == "" {
		if m := reFillTime.FindStringSubmatch(text); m != nil {
			res.FillTimeRaw = strings.TrimSpace(m[1])
		}
	}

	res.Parsed = res.Side != "" && res.Qty > 0 && res.Price > 0
	res.ExecTime = deriveExecTime(res.FillTimeRaw, res.Date)
	return res
}

// labelValue matches one "Key: Value" line of the MW notification block.
var labelValue = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z /]*?)\s*:\s*(.+?)\s*$`)

// parseLabels fills res from the labeled key-value lines of the body. First
// occurrence of each label wins.
func parseLabels(body string, res *Result) {
	for _, line := range strings.Split(body, "\n") {
		m := labelValue.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		val := m[2]
		switch strings.ToLower(m[1]) {
		case "execution time":
			if res.FillTimeRaw == "" {
				res.FillTimeRaw = val
			}
		case "broker":
			if res.Broker == "" {
				res.Broker = val
			}
		case "account id", "account":
			if res.Account == "" {
				res.Account = val
			}
		case "order id":
			if res.OrderID == "" {
				res.OrderID = val
			}
		case "symbol":
			if res.Symbol == "" {
				res.Symbol = strings.ToUpper(val)
			}
		case "buy/sell", "side":
			if res.Side == "" {
				switch strings.ToUpper(val) {
				case "BUY", "BOT", "BOUGHT":
					res.Side = "buy"
				case "SELL", "SLD", "SOLD":
					res.Side = "sell"
				}
			}
		case "quantity":
			if res.Qty == 0 {
				res.Qty = parseNum(val)
			}
		case "avg fill price", "fill price":
			if res.Price == 0 {
				res.Price = parseNum(val)
			}
		}
	}
}

// mwExecTimeLayout matches the "Jul-27 15:31:26" core of MW's execution
// time - no year, no zone, and the AM/PM marker handled separately (below).
const mwExecTimeLayout = "Jan-2 15:04:05"

// deriveExecTime resolves MW's year-less, zone-less execution time using the
// message's Date header for both. The Date header is stamped at compose time,
// seconds after the fill at most; if composing straddled New Year the naive
// combination lands ~a year ahead, so anything more than a day past the
// header date rolls back one year.
//
// The hour is a trap: MW appends an AM/PM marker to a 24-HOUR clock
// ("Jul-27 15:31:26 PM" - seen live; morning fills read "09:04:52 AM" which
// is indistinguishable from 12-hour). So the marker is stripped, the hour
// parsed as 24-hour, and the marker applied only where it carries meaning:
// PM with an hour below 12 shifts forward (covers a true-12-hour variant if
// some MW version emits one), AM with hour 12 means midnight. Under the
// observed 24-hour-with-marker format both adjustments are no-ops.
func deriveExecTime(fillRaw, dateHeader string) time.Time {
	if fillRaw == "" || dateHeader == "" {
		return time.Time{}
	}
	d, err := mail.ParseDate(dateHeader)
	if err != nil {
		return time.Time{}
	}
	raw := strings.TrimSpace(fillRaw)
	marker := ""
	switch upper := strings.ToUpper(raw); {
	case strings.HasSuffix(upper, " AM"):
		marker, raw = "AM", strings.TrimSpace(raw[:len(raw)-3])
	case strings.HasSuffix(upper, " PM"):
		marker, raw = "PM", strings.TrimSpace(raw[:len(raw)-3])
	}
	t, err := time.ParseInLocation(mwExecTimeLayout, raw, d.Location())
	if err != nil {
		return time.Time{}
	}
	hour := t.Hour()
	switch marker {
	case "PM":
		if hour < 12 {
			hour += 12
		}
	case "AM":
		if hour == 12 {
			hour = 0
		}
	}
	exec := time.Date(d.Year(), t.Month(), t.Day(), hour, t.Minute(), t.Second(), 0, d.Location())
	if exec.After(d.Add(24 * time.Hour)) {
		exec = exec.AddDate(-1, 0, 0)
	}
	return exec.UTC()
}

// applySideQtyHeuristics is the regex fallback for side and quantity.
func applySideQtyHeuristics(text string, res *Result) {
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
