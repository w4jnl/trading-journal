package importer

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"time"
)

// utf8BOM is the byte sequence Excel and some CSV exporters (TopstepX) prepend.
// encoding/csv doesn't strip it, so we strip it ourselves before parsing.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// stripBOMReader wraps r so a leading UTF-8 BOM is silently skipped.
func stripBOMReader(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	if peek, err := br.Peek(3); err == nil && bytes.Equal(peek, utf8BOM) {
		_, _ = br.Discard(3)
	}
	return br
}

// stripBOMBytes removes a leading UTF-8 BOM from b if present.
func stripBOMBytes(b []byte) []byte {
	return bytes.TrimPrefix(b, utf8BOM)
}

// Fill is the unified output of all CSV parsers — one row from a broker export
// after normalization. The ImportService turns these into rows in the
// `executions` table.
type Fill struct {
	AccountRef   string    // raw account string from CSV (e.g. "APEX-100001-28")
	BrokerExecID string    // unique fill/order id within (account, source)
	Symbol       string    // contract code, e.g. "ESM6"; left as-is per source even when SymbolRoot is canonicalized
	SymbolRoot   string    // canonicalized root, e.g. "ES"; CQG's "EP" is mapped to "ES"
	Exchange     string    // optional; empty for TopstepX
	Source       string    // data-feed origin from the export (MotiveWave Source col: "Rithmic", "AMP Global Clearing - CQG", …); empty for TopstepX
	Side         string    // "buy" or "sell"
	Qty          float64
	Price        float64
	ExecTime     time.Time // UTC
	RawData      string    // JSON-encoded original row, optional
}

// Format identifies a CSV import source.
type Format string

const (
	FormatMotiveWave Format = "motivewave_csv"
	FormatTopstepX   Format = "topstepx_csv"
	FormatUnknown    Format = ""
)

// GuessFirm maps an account reference and import-source hints to a clearing firm.
// Returns "" when nothing matches; the importer still creates the account but
// flags it needs_setup=1 so the user can assign firm via the UI.
//
// MotiveWave's Source column is informative: AMP fills carry "AMP Global Clearing - CQG"
// but their account_ref is just numeric (e.g. "120042"), so the prefix rules below
// would otherwise miss them.
func GuessFirm(format Format, accountRef, source string) string {
	switch format {
	case FormatTopstepX:
		return "topstep"
	case FormatMotiveWave:
		if strings.Contains(source, "AMP") {
			return "amp"
		}
		switch {
		case strings.HasPrefix(accountRef, "APEX-"):
			return "apex"
		case strings.HasPrefix(accountRef, "LFE"), strings.HasPrefix(accountRef, "LFF"):
			return "lucid"
		case looksLikeIBKR(accountRef):
			return "ibkr"
		}
	}
	return ""
}

// CanonicalizeSymbolRoot maps source-specific aliases to the canonical root
// stored in the `instruments` table. Currently handles CQG's "EP" → "ES" alias
// for the E-mini S&P 500 (CQG/AMP feed uses EP; everywhere else uses ES).
//
// Kept in code rather than the instrument_aliases table because there's only
// one mapping today and the static form is easier to test. Move to DB-backed
// resolution if a second mapping shows up.
func CanonicalizeSymbolRoot(root, source string) string {
	if (strings.Contains(source, "CQG") || strings.Contains(source, "AMP")) && root == "EP" {
		return "ES"
	}
	return root
}

func looksLikeIBKR(ref string) bool {
	if len(ref) < 2 || ref[0] != 'U' {
		return false
	}
	for i := 1; i < len(ref); i++ {
		if ref[i] < '0' || ref[i] > '9' {
			return false
		}
	}
	return true
}

// futuresMonthCodes are CME/CBOT/NYMEX month codes (Jan-Dec, skipping I,L,P,T,W,Y).
const futuresMonthCodes = "FGHJKMNQUVXZ"

// DeriveSymbolRoot strips the trailing month-letter+year-digits from a futures
// contract code. "ESM6" -> "ES", "MESH26" -> "MES". Returns the input unchanged
// if the suffix doesn't match a known pattern.
func DeriveSymbolRoot(contract string) string {
	contract = strings.TrimSpace(contract)
	if len(contract) < 3 {
		return contract
	}
	// Try double-digit year first: <root><month-letter><2 digits>
	if len(contract) >= 4 {
		if isMonthCode(contract[len(contract)-3]) &&
			isDigit(contract[len(contract)-2]) &&
			isDigit(contract[len(contract)-1]) {
			return contract[:len(contract)-3]
		}
	}
	// Single-digit year: <root><month-letter><1 digit>
	if isMonthCode(contract[len(contract)-2]) && isDigit(contract[len(contract)-1]) {
		return contract[:len(contract)-2]
	}
	return contract
}

func isMonthCode(b byte) bool { return strings.IndexByte(futuresMonthCodes, b) >= 0 }
func isDigit(b byte) bool     { return b >= '0' && b <= '9' }
