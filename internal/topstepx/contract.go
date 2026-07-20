package topstepx

import "strings"

// futuresMonthCodes are CME month codes (Jan-Dec, skipping I,L,P,T,W,Y).
// Mirrors importer.futuresMonthCodes; duplicated to keep this package free of
// an importer dependency at the parsing layer.
const futuresMonthCodes = "FGHJKMNQUVXZ"

// SymbolFromContractID maps a ProjectX contractId to the contract code the
// CSV importers store, so API- and CSV-sourced executions share FIFO queues
// and dedup keys.
//
//	"CON.F.US.MES.M26" -> symbol "MESM6", root "MES"
//
// Format: CON.F.<region>.<root>.<monthYY>. The year is compressed to its last
// digit because the TopstepX CSV exports use single-digit years ("MESM6").
// ok=false for anything that doesn't match; the caller falls back to
// Contract/searchById.
func SymbolFromContractID(contractID string) (symbol, root string, ok bool) {
	parts := strings.Split(strings.TrimSpace(contractID), ".")
	if len(parts) != 5 || parts[0] != "CON" || parts[1] != "F" {
		return "", "", false
	}
	root = parts[3]
	code := parts[4] // e.g. "M26"
	if root == "" || len(code) != 3 {
		return "", "", false
	}
	month := code[0]
	if !strings.ContainsRune(futuresMonthCodes, rune(month)) {
		return "", "", false
	}
	if code[1] < '0' || code[1] > '9' || code[2] < '0' || code[2] > '9' {
		return "", "", false
	}
	return root + string(month) + string(code[2]), root, true
}
